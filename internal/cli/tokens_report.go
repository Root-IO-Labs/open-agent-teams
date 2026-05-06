package cli

// `oat tokens report` is the historical counterpart to `oat status --tokens`.
// Where `oat status --tokens` reads the daemon's live in-memory counters off
// state.json, this command parses the `[OAT_TOKENS]` JSON lines already
// emitted to each agent's log file (~/.oat/output/<repo>/[workers/]*.log).
//
// Why two commands? Live and historical answer different questions:
//
//   - Live: "which agent is burning tokens right now?" — needs daemon state,
//     breaks if the daemon is down, only sees currently-tracked agents.
//   - Historical: "what did last week's benchmark cost me?" — needs the
//     on-disk log record, works when the daemon is stopped, sees retired
//     agents whose in-memory state has been garbage-collected.
//
// The two commands intentionally share no code paths so a bug in either
// can't silently corrupt the other.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/routing"
)

// oatTokensLogPrefix matches the sentinel string emitted by
// pkg/backend/sidecar_bridge.go:appendOatTokensSentinel and the Python
// runtime's stdout-path token emission. Both share the same JSON shape.
const oatTokensLogPrefix = "[OAT_TOKENS] "

// tokenLogPayload mirrors the on-disk JSON shape written alongside the
// sentinel prefix. Only the fields we aggregate are named; anything else
// the runtime decides to add flows through encoding/json's tolerant
// unknown-field handling.
type tokenLogPayload struct {
	DeltaInput       int64 `json:"delta_input"`
	DeltaOutput      int64 `json:"delta_output"`
	CumulativeInput  int64 `json:"cumulative_input"`
	CumulativeOutput int64 `json:"cumulative_output"`
	CacheRead        int64 `json:"cache_read"`
	CacheCreation    int64 `json:"cache_creation"`
}

// agentReport is the per-agent row we accumulate before output formatting.
type agentReport struct {
	Agent         string   `json:"agent"`
	IsWorker      bool     `json:"is_worker"`
	LogFile       string   `json:"log_file"`
	Wave          string   `json:"wave,omitempty"`
	Model         string   `json:"model,omitempty"`
	InputTokens   int64    `json:"input_tokens"`
	OutputTokens  int64    `json:"output_tokens"`
	CacheRead     int64    `json:"cache_read_tokens"`
	CacheCreation int64    `json:"cache_creation_tokens"`
	CacheHitPct   string   `json:"cache_hit_pct"`
	CostUSD       *float64 `json:"cost_usd"` // nil = not priced (model not in pricing.yaml or model unknown)
	FirstSeen     string   `json:"first_seen,omitempty"`
	LastSeen      string   `json:"last_seen,omitempty"`
}

type tokensReportPayload struct {
	Repo     string        `json:"repo"`
	Since    string        `json:"since,omitempty"`
	Until    string        `json:"until,omitempty"`
	WaveOnly string        `json:"wave,omitempty"`
	Agents   []agentReport `json:"agents"`
	Totals   totalsBlock   `json:"totals"`
}

type totalsBlock struct {
	InputTokens   int64    `json:"input_tokens"`
	OutputTokens  int64    `json:"output_tokens"`
	CacheRead     int64    `json:"cache_read_tokens"`
	CacheCreation int64    `json:"cache_creation_tokens"`
	CacheHitPct   string   `json:"cache_hit_pct"`
	CostUSD       *float64 `json:"cost_usd"` // sum across only the priced agents (unpriced ones excluded; footnote indicates this)
}

// wavesFile maps worker name (matching agent log basename without .log) to
// a wave label like "wave:2". Intended to be produced by benchmarks during
// `collect.sh` so `oat tokens report --wave 2` can filter without reading
// GitHub. Optional; when absent, --wave filter is a no-op with a warning.
type wavesFile struct {
	Waves map[string]string `json:"waves"`
}

// tokensReport is the CLI entry point for `oat tokens report`.
func (c *CLI) tokensReport(args []string) error {
	flags, _ := ParseFlags(args)

	repoName := flags["repo"]
	if repoName == "" {
		return fmt.Errorf("--repo is required (use the OAT repo name, e.g. `oat-my-project`)")
	}

	format := flags["format"]
	if format == "" {
		format = "table"
	}
	if format != "table" && format != "json" {
		return fmt.Errorf("--format must be one of: table, json")
	}

	since, err := parseSinceUntil(flags["since"])
	if err != nil {
		return fmt.Errorf("invalid --since: %w", err)
	}
	until, err := parseSinceUntil(flags["until"])
	if err != nil {
		return fmt.Errorf("invalid --until: %w", err)
	}

	var wavesByAgent map[string]string
	if wf := flags["waves-file"]; wf != "" {
		loaded, err := loadWavesFile(wf)
		if err != nil {
			return fmt.Errorf("reading --waves-file %q: %w", wf, err)
		}
		wavesByAgent = loaded
	}

	waveFilter := flags["wave"]

	repoOutDir := filepath.Join(c.paths.OutputDir, repoName)
	if _, err := os.Stat(repoOutDir); err != nil {
		return fmt.Errorf("no output logs for repo %q (looked in %s)", repoName, repoOutDir)
	}

	agents, err := scanTokenLogs(repoOutDir, since, until)
	if err != nil {
		return fmt.Errorf("scanning log files: %w", err)
	}

	for i := range agents {
		if w, ok := wavesByAgent[agents[i].Agent]; ok {
			agents[i].Wave = w
		}
	}

	// Per-agent model lookup (best-effort) and cost computation.
	// Source of truth for per-agent model is state.json; agents that have
	// already been GC'd from state will end up with Model="" and CostUSD=nil,
	// which the table footer flags via a footnote. Pricing comes from the
	// embedded pricing.yaml.
	modelByAgent := loadAgentModelsFromState(c, repoName)
	pricing := routing.LoadEmbeddedPricing()
	for i := range agents {
		if m, ok := modelByAgent[agents[i].Agent]; ok {
			agents[i].Model = m
		}
		agents[i].CostUSD = computeAgentCost(agents[i], pricing)
	}

	if waveFilter != "" {
		if len(wavesByAgent) == 0 {
			fmt.Fprintf(os.Stderr,
				"warning: --wave %s requested but no --waves-file provided; showing all agents.\n",
				waveFilter)
		} else {
			agents = filterByWave(agents, waveFilter)
		}
	}

	sort.SliceStable(agents, func(i, j int) bool {
		if agents[i].IsWorker != agents[j].IsWorker {
			return !agents[i].IsWorker
		}
		return agents[i].Agent < agents[j].Agent
	})

	payload := tokensReportPayload{
		Repo:     repoName,
		WaveOnly: waveFilter,
		Agents:   agents,
	}
	if !since.IsZero() {
		payload.Since = since.UTC().Format(time.RFC3339)
	}
	if !until.IsZero() {
		payload.Until = until.UTC().Format(time.RFC3339)
	}
	var totalCost float64
	var anyCost bool
	for _, a := range agents {
		payload.Totals.InputTokens += a.InputTokens
		payload.Totals.OutputTokens += a.OutputTokens
		payload.Totals.CacheRead += a.CacheRead
		payload.Totals.CacheCreation += a.CacheCreation
		if a.CostUSD != nil {
			totalCost += *a.CostUSD
			anyCost = true
		}
	}
	payload.Totals.CacheHitPct = hitPct(payload.Totals.CacheRead, payload.Totals.InputTokens)
	if anyCost {
		payload.Totals.CostUSD = &totalCost
	}

	if format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}
	return renderTokensTable(payload)
}

// scanTokenLogs walks the repo's output dir (both the top-level agent logs
// and the workers/ subdir), parses every [OAT_TOKENS] line in each .log
// file, and returns a final-cumulative snapshot per agent. File-level
// mtime-based filtering is applied up front; a log with mtime outside
// [since, until] is skipped wholesale rather than partially scanned, since
// the JSON payloads have no embedded timestamps.
func scanTokenLogs(repoOutDir string, since, until time.Time) ([]agentReport, error) {
	var reports []agentReport

	err := filepath.WalkDir(repoOutDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".log") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // skip unreadable log files during walk
		}
		mtime := info.ModTime()
		if !since.IsZero() && mtime.Before(since) {
			return nil
		}
		if !until.IsZero() && mtime.After(until) {
			return nil
		}

		r, err := scanSingleLog(path, repoOutDir, mtime)
		if err != nil {
			return nil //nolint:nilerr // skip unparseable log files during walk
		}
		if r != nil {
			reports = append(reports, *r)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return reports, nil
}

// scanSingleLog reads one agent log file end-to-end, tracking the final
// cumulative counters from its [OAT_TOKENS] stream. Returns nil if the log
// has no token lines (the agent never emitted any, or the log is purely a
// TUI render log with no sidecar bridge activity).
func scanSingleLog(path, repoOutDir string, fileMTime time.Time) (*agentReport, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var latest tokenLogPayload
	var sawAny bool

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		idx := strings.Index(line, oatTokensLogPrefix)
		if idx < 0 {
			continue
		}
		payload := strings.TrimSpace(line[idx+len(oatTokensLogPrefix):])
		if payload == "" {
			continue
		}
		var parsed tokenLogPayload
		if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
			continue
		}
		if parsed.CumulativeInput >= latest.CumulativeInput {
			latest = parsed
			sawAny = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !sawAny {
		return nil, nil
	}

	rel, err := filepath.Rel(repoOutDir, path)
	if err != nil {
		rel = path
	}
	agentName := strings.TrimSuffix(filepath.Base(path), ".log")
	isWorker := strings.HasPrefix(rel, "workers"+string(filepath.Separator)) ||
		strings.HasPrefix(rel, "workers/")

	r := &agentReport{
		Agent:         agentName,
		IsWorker:      isWorker,
		LogFile:       rel,
		InputTokens:   latest.CumulativeInput,
		OutputTokens:  latest.CumulativeOutput,
		CacheRead:     latest.CacheRead,
		CacheCreation: latest.CacheCreation,
		CacheHitPct:   hitPct(latest.CacheRead, latest.CumulativeInput),
		LastSeen:      fileMTime.UTC().Format(time.RFC3339),
	}
	return r, nil
}

// filterByWave keeps only agents whose recorded wave equals the requested
// label. The caller normalizes "2" → "wave:2" so users don't have to type
// the prefix.
func filterByWave(in []agentReport, wave string) []agentReport {
	target := wave
	if !strings.HasPrefix(target, "wave:") {
		target = "wave:" + strings.TrimPrefix(target, "wave:")
	}
	out := in[:0:0]
	for _, a := range in {
		if a.Wave == target {
			out = append(out, a)
		}
	}
	return out
}

// loadWavesFile reads the benchmark-supplied mapping of worker-name → wave
// label. Tolerates either {"waves": {...}} or a bare {...} object at the
// top level.
func loadWavesFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var wrapped wavesFile
	if err := json.Unmarshal(raw, &wrapped); err == nil && len(wrapped.Waves) > 0 {
		return wrapped.Waves, nil
	}
	var bare map[string]string
	if err := json.Unmarshal(raw, &bare); err != nil {
		return nil, fmt.Errorf("expected {\"waves\": {agent: wave}} or {agent: wave}: %w", err)
	}
	return bare, nil
}

// parseSinceUntil accepts either an RFC3339 timestamp or a Go-style
// duration like "2h" or "30m", interpreted as "N ago".
func parseSinceUntil(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("expected RFC3339 timestamp (2026-04-20T12:00:00Z) or duration (2h, 30m), got %q", s)
}

func hitPct(cacheRead, input int64) string {
	if input <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", float64(cacheRead)/float64(input)*100)
}

// loadAgentModelsFromState builds {agentName: model} from state.json for
// the requested repo. Best-effort: returns empty map (not an error) when
// state can't be loaded or the repo isn't there.
//
// Why best-effort: tokens_report runs offline against on-disk logs, and
// the corresponding state.json may have GC'd the retired agents. We
// degrade gracefully (cost stays nil; table footnote calls it out)
// rather than failing the whole command.
func loadAgentModelsFromState(c *CLI, repoName string) map[string]string {
	out := map[string]string{}
	st, err := c.loadState()
	if err != nil {
		return out
	}
	repo, ok := st.GetRepo(repoName)
	if !ok {
		return out
	}
	for name, ag := range repo.Agents {
		if ag.Model != "" {
			out[name] = ag.Model
		}
	}
	return out
}

// computeAgentCost returns the dollar cost for one agent's cumulative
// tokens, or nil when the model is unknown or has no pricing entry.
//
// Cost = (Input * input + Output * output + CacheRead * cache_read +
//
//	CacheCreation * cache_creation) / 1_000_000
//
// CacheCreation falls back to the per-provider helper when pricing.yaml
// omits cache_creation_per_mtok (see routing.CacheCreationPriceFor).
func computeAgentCost(a agentReport, pricing *routing.PricingRegistry) *float64 {
	if a.Model == "" {
		return nil
	}
	entry := pricing.Lookup(a.Model)
	if entry == nil {
		return nil
	}
	cacheCreatePrice := routing.CacheCreationPriceFor(a.Model, entry)
	cost := (float64(a.InputTokens)*entry.InputPerMtok +
		float64(a.OutputTokens)*entry.OutputPerMtok +
		float64(a.CacheRead)*entry.CacheReadPerMtok +
		float64(a.CacheCreation)*cacheCreatePrice) / 1_000_000.0
	return &cost
}

// fmtCost formats a possibly-nil cost value for the table view.
// Nil prints as "—" so the footer footnote is unambiguous: a dash means
// "not priced," not "$0.00."
func fmtCost(c *float64) string {
	if c == nil {
		return "—"
	}
	return fmt.Sprintf("$%.4f", *c)
}

func renderTokensTable(p tokensReportPayload) error {
	fmt.Printf("OAT Token Usage (historical) — repo: %s\n", p.Repo)
	if p.Since != "" || p.Until != "" {
		fmt.Printf("  window: %s .. %s\n", emptyOr(p.Since, "start"), emptyOr(p.Until, "now"))
	}
	if p.WaveOnly != "" {
		fmt.Printf("  wave filter: %s\n", p.WaveOnly)
	}
	fmt.Println()
	if len(p.Agents) == 0 {
		fmt.Println("  (no agents with [OAT_TOKENS] log lines in the requested window)")
		return nil
	}

	fmt.Printf("  %-45s %-8s %12s %10s %13s %13s %7s %12s\n",
		"AGENT", "WAVE", "INPUT", "OUTPUT", "CACHE_READ", "CACHE_CREATE", "HIT%", "COST_USD")
	fmt.Println("  " + strings.Repeat("-", 128))
	var unpriced int
	for _, a := range p.Agents {
		label := a.Agent
		if a.IsWorker {
			label = "worker/" + label
		}
		if len(label) > 45 {
			label = label[:42] + "..."
		}
		if a.CostUSD == nil {
			unpriced++
		}
		fmt.Printf("  %-45s %-8s %12d %10d %13d %13d %7s %12s\n",
			label, emptyOr(a.Wave, "—"),
			a.InputTokens, a.OutputTokens,
			a.CacheRead, a.CacheCreation, a.CacheHitPct,
			fmtCost(a.CostUSD))
	}
	fmt.Println("  " + strings.Repeat("-", 128))
	fmt.Printf("  %-45s %-8s %12d %10d %13d %13d %7s %12s\n",
		"TOTAL", "",
		p.Totals.InputTokens, p.Totals.OutputTokens,
		p.Totals.CacheRead, p.Totals.CacheCreation,
		p.Totals.CacheHitPct,
		fmtCost(p.Totals.CostUSD))
	if unpriced > 0 {
		fmt.Printf("\n  — : %d agent(s) have no cost — model not in internal/routing/pricing.yaml,\n"+
			"      or model unknown (agent already retired from state.json).\n"+
			"      Add the model + verified prices to pricing.yaml and rebuild to fix.\n",
			unpriced)
	}
	return nil
}

func emptyOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
