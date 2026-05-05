package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// State snapshots are injected into daemon → supervisor / merge-queue
// messages so those agents can see current worker + PR status without
// running `oat worker list` / `gh pr list` themselves. Benchmark evidence:
// the supervisor spent 3.66M tokens (47% of a 7.7M run) issuing 118 shell
// commands to re-discover state across 45 LLM turns. Pre-computing that
// state in the message collapses the typical 5-6-turn "status check loop"
// into a single turn, cutting supervisor tokens by an estimated 70-80%.

// snapshotCacheTTL bounds how long a snapshot is re-used across rapidly
// successive notifications. When 3 workers complete within ~1s of each
// other, all 6 resulting messages (3 supervisor + 3 merge-queue) share
// the same PR state — fetching once instead of six times.
const snapshotCacheTTL = 3 * time.Second

// prFetchTimeout bounds the `gh pr list` shell-out so a slow or failed
// call never wedges the daemon's routing. On timeout we send the message
// without the PR section; the supervisor/merge-queue can still fall back
// to fetching it themselves.
const prFetchTimeout = 3 * time.Second

// prListLimit is intentionally small. Pure worker flows rarely have > 10
// PRs open simultaneously; trimming keeps the injected context compact.
const prListLimit = 20

// maxWorkerLines caps the number of worker entries in the injected
// snapshot. Prevents unbounded message growth in repos with many
// long-lived or orphaned workers. If more workers exist, the snapshot
// shows the first N (stable-sorted by name) plus a "...and M more" line.
const maxWorkerLines = 30

// snapshotCacheEntry holds a cached per-repo snapshot.
type snapshotCacheEntry struct {
	supervisor string
	mergeQueue string
	at         time.Time
}

var (
	snapshotCache   = make(map[string]snapshotCacheEntry)
	snapshotCacheMu sync.Mutex
)

// snapshotFetchErrorDebounce is the minimum interval between repeated
// gh-failure log lines for the same repo. Prevents log spam when an
// expired auth token or network outage persists across many nudges.
const snapshotFetchErrorDebounce = 60 * time.Second

var (
	lastFetchErrorLogged   = make(map[string]time.Time)
	lastFetchErrorLoggedMu sync.Mutex
)

// prSummary is the minimal subset of `gh pr list` output we care about.
type prSummary struct {
	Number           int    `json:"number"`
	Title            string `json:"title"`
	URL              string `json:"url"`
	State            string `json:"state"`
	Mergeable        string `json:"mergeable"`
	MergeStateStatus string `json:"mergeStateStatus"`
	HeadRefName      string `json:"headRefName"`
}

// extractRepoFull pulls "owner/repo" out of a github URL like
// https://github.com/owner/repo(.git)?. Returns "" if it can't parse.
//
// Security: the returned string is passed as the value of `gh --repo`.
// gh's flag parser could be tricked if either path component starts
// with "-" (e.g. a URL like "https://github.com/--limit/5" would yield
// "--limit/5" and could be re-interpreted as a flag by gh). We
// explicitly reject any component starting with "-" to block flag
// smuggling, and require both owner and repo to be non-empty and
// composed of GitHub-valid characters.
func extractRepoFull(url string) string {
	url = strings.TrimSuffix(url, ".git")
	url = strings.TrimSuffix(url, "/")
	parts := strings.Split(url, "/")
	if len(parts) < 2 {
		return ""
	}
	owner := parts[len(parts)-2]
	repo := parts[len(parts)-1]
	if !isValidRepoComponent(owner) || !isValidRepoComponent(repo) {
		return ""
	}
	return owner + "/" + repo
}

// isValidRepoComponent reports whether s is a safe owner or repo name
// to embed in `gh --repo <owner>/<repo>`. Rejects empty strings,
// strings starting with "-" (flag smuggling), and strings containing
// characters outside GitHub's allowed set for owner/repo names
// (alphanumerics, hyphen, underscore, period).
func isValidRepoComponent(s string) bool {
	if s == "" || s[0] == '-' {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

// fetchOpenPRs is a package-level variable so tests can substitute a stub
// without shelling out to `gh`. Production code always uses fetchOpenPRsReal;
// reassignment is test-only (guard your test with t.Cleanup to restore).
//
// Signature returns (prs, err) so buildRepoSnapshots can distinguish
// "gh failed" from "gh returned no PRs" and log the former.
var fetchOpenPRs = fetchOpenPRsReal

// fetchOpenPRsReal calls `gh pr list` with a hard timeout. Returns
// (nil, err) on error/timeout and ([]prSummary{}, nil) when gh
// succeeds with zero open PRs. Callers should treat err != nil as
// "snapshot unavailable" and log it once per failure.
func fetchOpenPRsReal(githubURL string) ([]prSummary, error) {
	repoFull := extractRepoFull(githubURL)
	if repoFull == "" {
		return nil, fmt.Errorf("invalid github url: %q", githubURL)
	}

	ctx, cancel := context.WithTimeout(context.Background(), prFetchTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "pr", "list",
		"--repo", repoFull,
		"--state", "open",
		"--limit", fmt.Sprintf("%d", prListLimit),
		"--json", "number,title,url,state,mergeable,mergeStateStatus,headRefName",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}

	var prs []prSummary
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("parse gh output: %w", err)
	}
	return prs, nil
}

// workerStatusLines returns one-line descriptions of worker/verify-worker
// agents, sorted by name for stable output. Only workers are listed —
// merge-queue and supervisor don't need to see persistent-agent status.
func workerStatusLines(agents map[string]state.Agent) []string {
	var names []string
	for name, a := range agents {
		if a.Type == state.AgentTypeWorker || a.Type == state.AgentTypeVerification {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	lines := make([]string, 0, len(names))
	for _, name := range names {
		a := agents[name]
		status := "running"
		switch {
		case a.ReadyForCleanup:
			status = "completed"
		case a.WaitingForPR:
			status = "waiting_for_pr"
		case a.WaitingForVerification:
			status = "waiting_for_verification"
		case a.LastNudge.IsZero():
			status = "spawning"
		}

		var extras []string
		if a.PRNumber > 0 {
			extras = append(extras, fmt.Sprintf("PR=#%d", a.PRNumber))
		}
		if a.IssueNumber != "" {
			extras = append(extras, fmt.Sprintf("issue=#%s", a.IssueNumber))
		}
		if a.NudgeCount > 0 {
			extras = append(extras, fmt.Sprintf("nudges=%d", a.NudgeCount))
		}
		line := fmt.Sprintf("  %s: %s", name, status)
		if len(extras) > 0 {
			line += " [" + strings.Join(extras, " ") + "]"
		}
		lines = append(lines, line)
	}
	return lines
}

// maxPRURLLength bounds the PR URL length we embed in merge-queue
// snapshots. GitHub doesn't cap branch names, so a crafted fork branch
// can make pr.URL arbitrarily long; capping here prevents unbounded
// prompt content being injected into agent context.
const maxPRURLLength = 200

// sanitizeSnapshotField returns s with any characters that could
// reshape the enclosing markdown block stripped or replaced. Applies
// to gh-reported enum-ish fields like Mergeable / MergeStateStatus /
// HeadRefName that are normally well-formed but could theoretically
// contain newlines, carriage returns, or markdown headers if a gh
// binary on PATH were tampered with or GitHub changed its API.
func sanitizeSnapshotField(s string) string {
	// Strip any character that could break out of the current line/section.
	replacer := strings.NewReplacer(
		"\n", " ",
		"\r", " ",
		"\t", " ",
	)
	out := replacer.Replace(s)
	// Also collapse markdown header characters when at the start of a
	// line — they can't appear here after the newline strip, but belt
	// and suspenders for future changes in the enclosing template.
	return out
}

// truncate returns s truncated to max bytes with an ellipsis marker
// when the cut happens. Operates on bytes rather than runes because
// callers pass ASCII-dominant fields (URLs, enum names); the small
// risk of a rune boundary split is acceptable for a display string.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max < 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

// formatPRLineSupervisor returns a compact PR line for the supervisor.
// Supervisor just needs to know "is this PR in good shape or blocked?"
func formatPRLineSupervisor(pr prSummary) string {
	flags := ""
	if m := sanitizeSnapshotField(pr.Mergeable); m != "" && m != "MERGEABLE" {
		flags += " " + m
	}
	if st := sanitizeSnapshotField(pr.MergeStateStatus); st != "" && st != "CLEAN" {
		flags += " " + st
	}
	title := truncate(sanitizeSnapshotField(pr.Title), 60)
	return fmt.Sprintf("  #%d %s%s", pr.Number, title, flags)
}

// formatPRLineMergeQueue returns a merge-queue-focused PR line with the
// URL (needed for `gh pr merge <url>`) and full mergeability detail.
// Fields are sanitized and truncated before embedding to bound both
// the injected context size and any markdown-injection surface from
// attacker-controlled PR metadata (branch name, title).
func formatPRLineMergeQueue(pr prSummary) string {
	title := truncate(sanitizeSnapshotField(pr.Title), 50)
	merge := sanitizeSnapshotField(pr.Mergeable)
	if merge == "" {
		merge = "UNKNOWN"
	}
	status := sanitizeSnapshotField(pr.MergeStateStatus)
	if status == "" {
		status = "unknown"
	}
	url := truncate(sanitizeSnapshotField(pr.URL), maxPRURLLength)
	return fmt.Sprintf("  #%d %s\n    mergeable=%s merge_state=%s\n    url=%s",
		pr.Number, title, merge, status, url)
}

// logSnapshotFetchError emits a warn-level log entry for a gh fetch
// failure, debounced per-repo to avoid spam. An operator watching
// daemon.log gets one line per failure-window per repo — enough to
// notice an auth / network problem without drowning real events.
func (d *Daemon) logSnapshotFetchError(repoName string, err error) {
	lastFetchErrorLoggedMu.Lock()
	defer lastFetchErrorLoggedMu.Unlock()
	if last, ok := lastFetchErrorLogged[repoName]; ok && time.Since(last) < snapshotFetchErrorDebounce {
		return
	}
	lastFetchErrorLogged[repoName] = time.Now()
	d.logger.Warn("snapshot fetch failed for repo %s: %v (snapshot will omit PR list; further errors suppressed for %ds)",
		repoName, err, int(snapshotFetchErrorDebounce/time.Second))
}

// buildRepoSnapshots returns (supervisorSnapshot, mergeQueueSnapshot).
// Uses a short-lived cache so bursts of worker completions share one
// `gh pr list` call. Returned strings are ready to append to the base
// notification message; empty string means "nothing to add" (e.g. the
// repo is unknown — callers should still send the base message).
//
// Concurrency: holds snapshotCacheMu across the gh fetch so N concurrent
// cache-miss callers coalesce into one fetch (later arrivals block on
// the lock, then find fresh cache). This is a simpler alternative to
// singleflight since it doesn't require an extra dependency; the cost
// is brief cross-repo serialization during the fetch, which is fine at
// the scale we operate (bounded repo count, fetch ≤ prFetchTimeout=3s).
func (d *Daemon) buildRepoSnapshots(repoName string) (supSnap, mqSnap string) {
	snapshotCacheMu.Lock()
	defer snapshotCacheMu.Unlock()

	// Re-check cache after acquiring the lock: another goroutine may have
	// populated it while we were waiting.
	if e, ok := snapshotCache[repoName]; ok && time.Since(e.at) < snapshotCacheTTL {
		return e.supervisor, e.mergeQueue
	}

	repo, exists := d.state.GetRepo(repoName)
	if !exists || repo == nil {
		return "", ""
	}

	// Fetch PRs once — same list informs both snapshots. Log failures
	// with a per-repo debounce so an operator can diagnose an expired
	// gh token without log spam: status-check nudges fire at minute
	// granularity, and only the first error in a rolling window logs.
	prs, err := fetchOpenPRs(repo.GithubURL)
	if err != nil {
		d.logSnapshotFetchError(repoName, err)
	}
	workerLines := workerStatusLines(repo.Agents)

	sup := buildSupervisorText(workerLines, prs)
	mq := buildMergeQueueText(prs)

	snapshotCache[repoName] = snapshotCacheEntry{
		supervisor: sup,
		mergeQueue: mq,
		at:         time.Now(),
	}
	return sup, mq
}

// buildSupervisorText renders the supervisor snapshot. Structured as a
// markdown block so it's clearly separate from the base message in the
// agent's conversation history.
func buildSupervisorText(workerLines []string, prs []prSummary) string {
	var sb strings.Builder
	sb.WriteString("\n\n---\n## Current State (injected by daemon)\n")

	sb.WriteString("### Workers\n")
	if len(workerLines) == 0 {
		sb.WriteString("  (no workers tracked)\n")
	} else {
		shown := workerLines
		extra := 0
		if len(shown) > maxWorkerLines {
			extra = len(shown) - maxWorkerLines
			shown = shown[:maxWorkerLines]
		}
		for _, line := range shown {
			sb.WriteString(line + "\n")
		}
		if extra > 0 {
			fmt.Fprintf(&sb, "  ...and %d more (run `oat worker list` for the full list)\n", extra)
		}
	}

	sb.WriteString("\n### Open PRs\n")
	if prs == nil {
		sb.WriteString("  (unavailable — run `gh pr list` if needed)\n")
	} else if len(prs) == 0 {
		sb.WriteString("  (none)\n")
	} else {
		for _, pr := range prs {
			sb.WriteString(formatPRLineSupervisor(pr) + "\n")
		}
	}

	sb.WriteString("\n_This snapshot is fresh — prefer it over re-running `oat worker list` / `gh pr list` unless you need detail not shown here._\n")
	return sb.String()
}

// buildMergeQueueText renders the merge-queue snapshot. Merge-queue's
// whole job is PR-centric so we omit worker status and expand PR detail.
func buildMergeQueueText(prs []prSummary) string {
	var sb strings.Builder
	sb.WriteString("\n\n---\n## Current Open PRs (injected by daemon)\n")

	if prs == nil {
		sb.WriteString("  (unavailable — run `gh pr list` if needed)\n")
	} else if len(prs) == 0 {
		sb.WriteString("  (none)\n")
	} else {
		for _, pr := range prs {
			sb.WriteString(formatPRLineMergeQueue(pr) + "\n")
		}
	}

	sb.WriteString("\n_PR list is fresh — use it to decide what to merge or escalate._\n")
	return sb.String()
}
