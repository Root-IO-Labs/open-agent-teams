package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"

	"github.com/Root-IO-Labs/open-agent-teams/internal/routing"
	"github.com/Root-IO-Labs/open-agent-teams/internal/routing/stats"
	"github.com/Root-IO-Labs/open-agent-teams/internal/version"
)

// routingReport reads ~/.oat/routing-history.jsonl and prints a summary
// table grouped by canonical model.
//
// Columns:
//
//	model       canonical model id (or raw if no canonicalization)
//	n_scored    records that contributed to the success rate (excludes
//	            in-flight, superseded, manual-removed — see DeriveSuccessScore)
//	n_total     records grouped under this model overall
//	succ%       fraction of n_scored with success_score >= 0.5
//	wilson95    Wilson 95% one-sided lower bound on that fraction —
//	            refuses to overcommit on small N (3 wins of 3 isn't 100%)
//	wall_p50    median wall-clock duration (ms)
//	wall_p95    95th-percentile wall-clock duration (ms)
//
// Read-only. Excludes records flagged unscoreable from success-rate
// arithmetic (otherwise we'd bias toward whatever the operator manually
// killed).
func (c *CLI) routingReport(args []string) error {
	historyPath := routing.DefaultOutcomeHistoryPath(c.paths.Root)
	sidecarPath := routing.DefaultBackfillSidecarPath(c.paths.Root)

	// Use the joined reader so PR-merge observations from the backfill
	// sidecar feed into success_score correctly. Without this, a worker
	// that was logged as "removed/failed" but whose PR later merged would
	// stay scored as 0.0 in the report — invisible recovery.
	records, parseErrsRouting, err := routing.LoadCorpusJoined(historyPath, sidecarPath)
	if err != nil {
		return err
	}
	parseErrs := make([]parseErrLine, 0, len(parseErrsRouting))
	for _, e := range parseErrsRouting {
		parseErrs = append(parseErrs, parseErrLine{line: e.Line, err: e.Err})
	}
	if len(records) == 0 {
		fmt.Println("No routing history found at", historyPath)
		fmt.Println("(Records appear after worker / verification agents complete.)")
		return nil
	}

	type bucket struct {
		modelID string
		scores  []float64
		walls   []int64
		nScored int
		nTotal  int
	}
	groups := map[string]*bucket{}
	for _, rec := range records {
		key := rec.ModelCanonical
		if key == "" {
			key = rec.Model
		}
		if key == "" {
			key = "<unknown>"
		}
		b, ok := groups[key]
		if !ok {
			b = &bucket{modelID: key}
			groups[key] = b
		}
		b.nTotal++
		if rec.WallMs > 0 {
			b.walls = append(b.walls, rec.WallMs)
		}
		score, _, has := routing.DeriveSuccessScore(rec)
		if has {
			b.scores = append(b.scores, score)
			b.nScored++
		}
	}

	// Sort by n_scored desc, fall back to n_total.
	rows := make([]*bucket, 0, len(groups))
	for _, b := range groups {
		rows = append(rows, b)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].nScored != rows[j].nScored {
			return rows[i].nScored > rows[j].nScored
		}
		return rows[i].nTotal > rows[j].nTotal
	})

	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()

	fmt.Fprintf(w, "Routing history: %s\n", historyPath)
	fmt.Fprintf(w, "Records: %d total, %d unparseable (errors below)\n\n", len(records), len(parseErrs))

	fmt.Fprintln(w, "model                                      n_scored  n_total    succ%  wilson95  wall_p50  wall_p95")
	fmt.Fprintln(w, "─────────────────────────────────────────  ────────  ───────  ───────  ────────  ────────  ────────")
	for _, b := range rows {
		succPct := "—"
		wilson := "—"
		if b.nScored > 0 {
			succ := 0
			for _, s := range b.scores {
				if s >= 0.5 {
					succ++
				}
			}
			succPct = fmt.Sprintf("%.0f%%", 100.0*float64(succ)/float64(b.nScored))
			lo := stats.WilsonLowerBound(succ, b.nScored, 0.05)
			wilson = fmt.Sprintf("%.0f%%", lo*100)
		}
		p50 := "—"
		p95 := "—"
		if len(b.walls) > 0 {
			p50 = fmt.Sprintf("%dms", percentileInt64(b.walls, 0.5))
			p95 = fmt.Sprintf("%dms", percentileInt64(b.walls, 0.95))
		}
		fmt.Fprintf(w, "%-41s  %8d  %7d  %7s  %8s  %8s  %8s\n",
			truncate(b.modelID, 41), b.nScored, b.nTotal, succPct, wilson, p50, p95)
	}

	if len(parseErrs) > 0 {
		fmt.Fprintln(w, "\nUnparseable lines (preserved verbatim in corpus):")
		for i, e := range parseErrs {
			if i >= 5 {
				fmt.Fprintf(w, "  ... %d more\n", len(parseErrs)-5)
				break
			}
			fmt.Fprintf(w, "  line %d: %v\n", e.line, e.err)
		}
	}
	return nil
}

// routingRoute previews what the router would pick for a given task text,
// without spawning any worker or making LLM calls. Loads the same corpus +
// profiles + pricing the daemon uses, runs RouteForTask / RouteForTaskV2,
// prints the decision.
//
// Useful for: (a) debugging a confusing routing pick the user observed,
// (b) sanity-checking what V2 would do before opting in, (c) the base-case
// demo (inject records, see the pick change, prove the loop closes).
//
// Flags:
//
//	--task "<text>"   Required. The task description to route.
//	--allow "csv"     Optional. Comma-separated allowlist of model IDs.
//	--v2              Use RouteForTaskV2 (consults the corpus). Default uses V1.
//	--all             Run V1 and V2 side-by-side. Helpful for A/B comparison.
func (c *CLI) routingRoute(args []string) error {
	var task string
	var allowCSV string
	useV2 := false
	showAll := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--task":
			if i+1 >= len(args) {
				return fmt.Errorf("--task requires a value")
			}
			task = args[i+1]
			i++
		case "--allow":
			if i+1 >= len(args) {
				return fmt.Errorf("--allow requires a value")
			}
			allowCSV = args[i+1]
			i++
		case "--v2":
			useV2 = true
		case "--all":
			showAll = true
		case "--help", "-h":
			fmt.Fprintln(os.Stdout, "Usage: oat routing route --task \"<text>\" [--allow csv] [--v2 | --all]")
			fmt.Fprintln(os.Stdout, "  Previews the router's pick for the given task. No worker spawn.")
			return nil
		}
	}
	if task == "" {
		return fmt.Errorf("--task is required (try: oat routing route --task \"fix the typo\")")
	}

	// Load profiles, pricing, corpus the same way the daemon does.
	profiles, err := routing.NewProfileStore(c.paths.ModelProfilesDir)
	if err != nil {
		return fmt.Errorf("load profiles: %w", err)
	}
	if profiles.Count() == 0 {
		fmt.Fprintln(os.Stdout, "No model profiles loaded. Run `oat model onboard <provider:model>` first.")
		return nil
	}
	pricing := routing.LoadEmbeddedPricing()

	historyPath := routing.DefaultOutcomeHistoryPath(c.paths.Root)
	sidecarPath := routing.DefaultBackfillSidecarPath(c.paths.Root)
	records, _, err := routing.LoadCorpusJoined(historyPath, sidecarPath)
	if err != nil {
		return fmt.Errorf("load corpus: %w", err)
	}
	corpus := routing.BuildCorpusIndex(records)

	var allow []string
	if allowCSV != "" {
		allow = splitCSV(allowCSV)
	}
	ctx := routing.RouteContext{
		TaskText:      task,
		Role:          routing.RoleWorker,
		AllowedModels: allow,
	}

	printDecision := func(label string, d *routing.RouteDecision, err error) {
		fmt.Fprintf(os.Stdout, "\n── %s ──\n", label)
		if err != nil {
			fmt.Fprintf(os.Stdout, "  ERROR: %v\n", err)
			return
		}
		fmt.Fprintf(os.Stdout, "  chosen:     %s\n", d.ChosenModel)
		fmt.Fprintf(os.Stdout, "  source:     %s\n", d.RoutingSource)
		fmt.Fprintf(os.Stdout, "  complexity: %s\n", d.Complexity)
		fmt.Fprintf(os.Stdout, "  reason:     %s\n", d.Reason)
		if len(d.Candidates) > 0 {
			fmt.Fprintf(os.Stdout, "  candidates: %v\n", d.Candidates)
		}
	}

	fmt.Fprintf(os.Stdout, "Task: %q\n", task)
	fmt.Fprintf(os.Stdout, "Corpus records loaded: %d (sidecar joined where applicable)\n", len(records))
	if !corpus.IsEmpty() {
		fmt.Fprintf(os.Stdout, "Corpus snapshot built: %s\n", corpus.BuiltAt().Format("2006-01-02 15:04:05 MST"))
	} else {
		fmt.Fprintln(os.Stdout, "Corpus is empty — V2 will fall through to V1-equivalent behavior.")
	}

	if showAll {
		v1, v1Err := profiles.RouteForTask(ctx, pricing)
		printDecision("V1 (cost-aware)", v1, v1Err)
		v2, v2Err := profiles.RouteForTaskV2(ctx, pricing, corpus)
		printDecision("V2 (historical-weighted)", v2, v2Err)
		return nil
	}

	if useV2 {
		d, err := profiles.RouteForTaskV2(ctx, pricing, corpus)
		printDecision("V2 (historical-weighted)", d, err)
		return nil
	}
	d, err := profiles.RouteForTask(ctx, pricing)
	printDecision("V1 (cost-aware)", d, err)
	return nil
}

// splitCSV splits a comma-separated string, trimming whitespace around each
// entry. Empty input → empty slice.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := []string{}
	cur := ""
	flush := func() {
		t := trimSpaces(cur)
		if t != "" {
			parts = append(parts, t)
		}
		cur = ""
	}
	for _, r := range s {
		if r == ',' {
			flush()
			continue
		}
		cur += string(r)
	}
	flush()
	return parts
}

func trimSpaces(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// routingShare builds the upload payload according to the user's privacy
// mode and prints it (--dry-run only in v0). The actual upload endpoint
// (routing.PlaceholderShareEndpoint) is not yet implemented; this command
// proves the sanitization works end-to-end so users can audit what would
// leave their machine before consenting to actual uploads.
//
// Requires OAT_LOG_PRIVACY=share-features or =share-all. Friendly error
// otherwise.
//
// Flags:
//
//	--dry-run    Print payload to stdout (default in v0; only mode supported)
func (c *CLI) routingShare(args []string) error {
	dryRun := false
	for _, a := range args {
		switch a {
		case "--dry-run":
			dryRun = true
		case "--help", "-h":
			fmt.Fprintln(os.Stdout, "Usage: oat routing share [--dry-run]")
			fmt.Fprintln(os.Stdout, "  In v0 the upload endpoint is placeholder-only;")
			fmt.Fprintln(os.Stdout, "  --dry-run is the only supported invocation and prints the payload.")
			return nil
		}
	}
	if !dryRun {
		fmt.Fprintf(os.Stdout, "v0 only supports --dry-run. Upload endpoint (%s) is not yet live.\n",
			routing.PlaceholderShareEndpoint)
		fmt.Fprintln(os.Stdout, "Re-run with --dry-run to preview the payload that would be sent.")
		return nil
	}

	mode := routing.CurrentPrivacyMode()
	if mode != routing.PrivacyModeShareFeatures && mode != routing.PrivacyModeShareAll {
		fmt.Fprintln(os.Stdout, "Sharing requires explicit opt-in. Set OAT_LOG_PRIVACY=share-features")
		fmt.Fprintln(os.Stdout, "or =share-all and re-run. Run `oat routing privacy` to see what each")
		fmt.Fprintln(os.Stdout, "level means.")
		return nil
	}

	historyPath := routing.DefaultOutcomeHistoryPath(c.paths.Root)
	sidecarPath := routing.DefaultBackfillSidecarPath(c.paths.Root)
	// Same join-on-read as the report — share payload should reflect the
	// up-to-date success signals, not the immutable initial outcome.
	records, _, err := routing.LoadCorpusJoined(historyPath, sidecarPath)
	if err != nil {
		return err
	}
	payload, err := routing.BuildSharePayload(records, mode, "", buildOATVersion())
	if err != nil {
		return err
	}

	out, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	fmt.Fprintln(os.Stdout, string(out))
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintf(os.Stderr, "Would POST to %s (placeholder — endpoint not yet live).\n", routing.PlaceholderShareEndpoint)
	fmt.Fprintf(os.Stderr, "Records: %d, mode: %s\n", len(payload.Records), payload.Consent)
	return nil
}

// buildOATVersion is a CLI-side helper that returns the same compact
// "<semver> (<sha>)" rendering daemon.go uses, derived from the version
// package. Duplicated here (rather than exported from daemon) because the
// CLI cannot import internal/daemon — that's the wrong dependency direction.
func buildOATVersion() string {
	v := version.Current()
	if v.IsDev || v.Version == "" {
		if v.Commit != "" && v.Commit != "none" {
			return "dev (" + v.Commit + ")"
		}
		return "dev"
	}
	if v.Commit != "" && v.Commit != "none" {
		return v.Version + " (" + v.Commit + ")"
	}
	return v.Version
}

// routingPrivacy prints the current privacy mode and what each level means.
// Read-only.
func (c *CLI) routingPrivacy(args []string) error {
	mode := routing.CurrentPrivacyMode()
	fmt.Fprintf(os.Stdout, "Current privacy mode: %s\n", mode)
	if routing.IsLoggingDisabled() {
		fmt.Fprintln(os.Stdout, "Outcome logging:      DISABLED (OAT_OUTCOME_LOG=off)")
	} else {
		fmt.Fprintln(os.Stdout, "Outcome logging:      enabled")
	}
	fmt.Fprintln(os.Stdout, "")
	fmt.Fprintln(os.Stdout, "Levels (set via OAT_LOG_PRIVACY env):")
	fmt.Fprintln(os.Stdout, "  strict          — hashes only on disk; no task_text, no summary")
	fmt.Fprintln(os.Stdout, "  local (default) — full text on disk; never uploaded anywhere")
	fmt.Fprintln(os.Stdout, "  share-features  — full local + opt-in upload of features only")
	fmt.Fprintln(os.Stdout, "  share-all       — full local + opt-in upload including raw text")
	fmt.Fprintln(os.Stdout, "")
	fmt.Fprintln(os.Stdout, "Kill switch: OAT_OUTCOME_LOG=off disables all routing-history writes.")
	return nil
}

// routingMigrate is the explicit `oat routing migrate-v1` entry. Daemon
// auto-migrates on start; this is for users who want to run migration
// without restarting the daemon (e.g. before running a one-off report).
func (c *CLI) routingMigrate(args []string) error {
	historyPath := routing.DefaultOutcomeHistoryPath(c.paths.Root)
	stats, err := routing.MigrateV1ToV2(historyPath)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Migration complete: %d v1→v2, %d already v2, %d unparseable preserved.\n",
		stats.V1Migrated, stats.V2Passthrough, stats.Skipped)
	if stats.BackupCreated {
		fmt.Fprintf(os.Stdout, "Original backed up to: %s\n", stats.BackupPath)
	}
	return nil
}

// loadOutcomeRecord parsing helpers ───────────────────────────────────────

type parseErrLine struct {
	line int
	err  error
}

func percentileInt64(xs []int64, p float64) int64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]int64(nil), xs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(math.Floor(p * float64(len(sorted)-1)))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
