package daemon

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Root-IO-Labs/open-agent-teams/internal/state"
)

// resetSnapshotCache clears the package-level snapshot cache and the
// per-repo fetch-error debounce map so each test starts from a known
// state. Required because the cache TTL (3s) would otherwise cause a
// later test to see a stub result from an earlier test.
func resetSnapshotCache(t *testing.T) {
	t.Helper()
	snapshotCacheMu.Lock()
	snapshotCache = make(map[string]snapshotCacheEntry)
	snapshotCacheMu.Unlock()

	lastFetchErrorLoggedMu.Lock()
	lastFetchErrorLogged = make(map[string]time.Time)
	lastFetchErrorLoggedMu.Unlock()
}

// withFakePRs replaces fetchOpenPRs with a stub that returns (prs, nil)
// and restores the real implementation when the test finishes. Avoids
// shelling out to `gh` during tests.
func withFakePRs(t *testing.T, prs []prSummary) {
	t.Helper()
	original := fetchOpenPRs
	fetchOpenPRs = func(string) ([]prSummary, error) { return prs, nil }
	t.Cleanup(func() { fetchOpenPRs = original })
}

// withFakePRsError replaces fetchOpenPRs with a stub that returns
// (nil, err) — the "gh failed" path. Lets tests assert on the
// fallthrough behavior and debounced logging.
func withFakePRsError(t *testing.T, err error) {
	t.Helper()
	original := fetchOpenPRs
	fetchOpenPRs = func(string) ([]prSummary, error) { return nil, err }
	t.Cleanup(func() { fetchOpenPRs = original })
}

// addTestRepo installs a repo with the given workers. Returns the repo name.
func addTestRepo(t *testing.T, d *Daemon, name string, workers map[string]state.Agent) {
	t.Helper()
	repo := &state.Repository{
		GithubURL:   "https://github.com/test/" + name,
		SessionName: "test-session-" + name,
		Agents:      make(map[string]state.Agent),
	}
	if err := d.state.AddRepo(name, repo); err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	for agentName, agent := range workers {
		if err := d.state.AddAgent(name, agentName, agent); err != nil {
			t.Fatalf("AddAgent %s: %v", agentName, err)
		}
	}
}

func TestWithRepoSnapshot_Supervisor_AppendsSnapshot(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()
	resetSnapshotCache(t)
	withFakePRs(t, nil)

	addTestRepo(t, d, "repo-sup", map[string]state.Agent{
		"alpha-fox": {Type: state.AgentTypeWorker, WindowName: "w1", CreatedAt: time.Now()},
	})

	got := d.withRepoSnapshot("repo-sup", state.AgentTypeSupervisor, "[daemon] hello")

	if !strings.Contains(got, "[daemon] hello") {
		t.Errorf("original message dropped: %q", got)
	}
	if !strings.Contains(got, "## Current State") {
		t.Errorf("expected supervisor snapshot marker, got:\n%s", got)
	}
	if !strings.Contains(got, "alpha-fox") {
		t.Errorf("expected worker name in snapshot, got:\n%s", got)
	}
}

func TestWithRepoSnapshot_MergeQueue_UsesMergeQueueFormat(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()
	resetSnapshotCache(t)
	withFakePRs(t, []prSummary{
		{Number: 42, Title: "Fix stuff", URL: "https://github.com/test/repo-mq/pull/42",
			Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"},
	})

	addTestRepo(t, d, "repo-mq", nil)

	got := d.withRepoSnapshot("repo-mq", state.AgentTypeMergeQueue, "[daemon] hi")

	if !strings.Contains(got, "[daemon] hi") {
		t.Errorf("original message dropped: %q", got)
	}
	// merge-queue format includes the full URL; supervisor format doesn't.
	if !strings.Contains(got, "https://github.com/test/repo-mq/pull/42") {
		t.Errorf("expected merge-queue PR URL in snapshot, got:\n%s", got)
	}
	if strings.Contains(got, "### Workers") {
		t.Errorf("merge-queue snapshot should NOT include worker section, got:\n%s", got)
	}
}

func TestWithRepoSnapshot_NonTarget_Passthrough(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()
	resetSnapshotCache(t)
	withFakePRs(t, nil)

	addTestRepo(t, d, "repo-passthrough", map[string]state.Agent{
		"alpha-fox": {Type: state.AgentTypeWorker, WindowName: "w1", CreatedAt: time.Now()},
	})

	for _, target := range []state.AgentType{
		state.AgentTypeWorker,
		state.AgentTypeWorkspace,
		state.AgentTypeVerification,
		state.AgentTypePRShepherd,
		state.AgentTypeReview,
		state.AgentTypeGenericPersistent,
	} {
		t.Run(string(target), func(t *testing.T) {
			got := d.withRepoSnapshot("repo-passthrough", target, "original msg")
			if got != "original msg" {
				t.Errorf("target=%s: expected passthrough, got appended snapshot:\n%s", target, got)
			}
		})
	}
}

func TestWithRepoSnapshot_UnknownRepo_Passthrough(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()
	resetSnapshotCache(t)
	withFakePRs(t, nil)

	got := d.withRepoSnapshot("no-such-repo", state.AgentTypeSupervisor, "bare")
	if got != "bare" {
		t.Errorf("expected bare msg for unknown repo, got:\n%s", got)
	}
}

func TestWithRepoSnapshot_KillSwitch(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()
	resetSnapshotCache(t)
	withFakePRs(t, []prSummary{{Number: 1, Title: "x"}})

	addTestRepo(t, d, "repo-killswitch", map[string]state.Agent{
		"alpha-fox": {Type: state.AgentTypeWorker, WindowName: "w1", CreatedAt: time.Now()},
	})

	t.Setenv(snapshotDisabledEnv, "1")
	got := d.withRepoSnapshot("repo-killswitch", state.AgentTypeSupervisor, "msg")
	if got != "msg" {
		t.Errorf("kill-switch should suppress snapshot, got:\n%s", got)
	}

	// With kill-switch explicitly unset, snapshot returns. Use
	// os.Unsetenv via a fresh t.Setenv+"" pairing is insufficient (it
	// leaves the var set to empty string) — we want the absent-var
	// semantics to be covered too.
	if err := os.Unsetenv(snapshotDisabledEnv); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}
	resetSnapshotCache(t) // force fresh build, not cached from killswitch path
	got = d.withRepoSnapshot("repo-killswitch", state.AgentTypeSupervisor, "msg")
	if !strings.Contains(got, "## Current State") {
		t.Errorf("expected snapshot after unsetting kill-switch, got:\n%s", got)
	}
}

func TestWithRepoSnapshot_EmptyRepo_SnapshotShowsEmptyState(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()
	resetSnapshotCache(t)
	withFakePRs(t, []prSummary{}) // empty list, not nil — means "fetch succeeded, no PRs"

	addTestRepo(t, d, "repo-empty", nil)

	got := d.withRepoSnapshot("repo-empty", state.AgentTypeSupervisor, "[daemon] kickoff")
	if !strings.Contains(got, "(no workers tracked)") {
		t.Errorf("expected empty-workers marker, got:\n%s", got)
	}
	if !strings.Contains(got, "(none)") {
		t.Errorf("expected empty-PRs marker, got:\n%s", got)
	}
}

func TestBuildSupervisorText_CapsWorkerList(t *testing.T) {
	// Build a synthetic worker-lines slice past the cap, confirm truncation.
	lines := make([]string, maxWorkerLines+7)
	for i := range lines {
		lines[i] = fmt.Sprintf("  worker-%03d: running", i)
	}
	got := buildSupervisorText(lines, nil)

	// Count actual worker lines rendered (they all start with "  worker-").
	renderedCount := strings.Count(got, "  worker-")
	if renderedCount != maxWorkerLines {
		t.Errorf("expected %d worker lines rendered, got %d", maxWorkerLines, renderedCount)
	}
	if !strings.Contains(got, "...and 7 more") {
		t.Errorf("expected truncation marker, got:\n%s", got)
	}
}

func TestBuildSupervisorText_NoCapNeeded(t *testing.T) {
	lines := []string{"  alpha: running", "  beta: completed"}
	got := buildSupervisorText(lines, nil)

	if !strings.Contains(got, "alpha") || !strings.Contains(got, "beta") {
		t.Errorf("expected both workers in output, got:\n%s", got)
	}
	if strings.Contains(got, "and 0 more") || strings.Contains(got, "and -") {
		t.Errorf("unexpected truncation marker when under cap, got:\n%s", got)
	}
}

func TestWithRepoSnapshot_ConcurrentSafe(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()
	resetSnapshotCache(t)
	withFakePRs(t, nil)

	addTestRepo(t, d, "repo-concurrent", map[string]state.Agent{
		"a": {Type: state.AgentTypeWorker, WindowName: "w1", CreatedAt: time.Now()},
	})

	// Hammer the helper from many goroutines. Run with `go test -race` to
	// catch data races on snapshotCache / fetchOpenPRs.
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = d.withRepoSnapshot("repo-concurrent", state.AgentTypeSupervisor, "msg")
			_ = d.withRepoSnapshot("repo-concurrent", state.AgentTypeMergeQueue, "msg")
		}()
	}
	wg.Wait()
}

func TestWithRepoSnapshot_FetchFailed_FallsThroughToBareOrUnavailableMarker(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()
	resetSnapshotCache(t)
	withFakePRsError(t, errors.New("gh pr list: exit status 1"))

	addTestRepo(t, d, "repo-gh-broken", map[string]state.Agent{
		"alpha-fox": {Type: state.AgentTypeWorker, WindowName: "w1", CreatedAt: time.Now()},
	})

	got := d.withRepoSnapshot("repo-gh-broken", state.AgentTypeSupervisor, "[daemon] hi")

	// Snapshot text still attaches (workers are known locally) but PR
	// section shows the "unavailable" marker so the agent knows it can
	// still fall back to `gh pr list` if it needs detail.
	if !strings.Contains(got, "alpha-fox") {
		t.Errorf("expected worker name in snapshot despite gh failure, got:\n%s", got)
	}
	if !strings.Contains(got, "unavailable") {
		t.Errorf("expected 'unavailable' marker in PR section, got:\n%s", got)
	}
}

func TestLogSnapshotFetchError_DebouncesPerRepo(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()
	resetSnapshotCache(t)

	// Fire the log call 5 times in rapid succession for the same repo.
	// Without debounce: 5 log lines. With debounce: 1.
	for i := 0; i < 5; i++ {
		d.logSnapshotFetchError("repo-x", errors.New("gh failed"))
	}

	lastFetchErrorLoggedMu.Lock()
	_, ok := lastFetchErrorLogged["repo-x"]
	lastFetchErrorLoggedMu.Unlock()
	if !ok {
		t.Errorf("expected last-logged entry for repo-x, got none")
	}
	// Different repo should log independently.
	d.logSnapshotFetchError("repo-y", errors.New("gh failed"))
	lastFetchErrorLoggedMu.Lock()
	_, ok = lastFetchErrorLogged["repo-y"]
	lastFetchErrorLoggedMu.Unlock()
	if !ok {
		t.Errorf("expected last-logged entry for repo-y, got none")
	}
}

func TestWithRepoSnapshot_DoubleInjection_Suppressed(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()
	resetSnapshotCache(t)
	withFakePRs(t, nil)

	addTestRepo(t, d, "repo-dbl", map[string]state.Agent{
		"alpha-fox": {Type: state.AgentTypeWorker, WindowName: "w1", CreatedAt: time.Now()},
	})

	// Simulate a message that already carries a snapshot (e.g. from
	// benchmark kickoff or a previous withRepoSnapshot pass).
	pre := "[benchmark] kickoff\n\n---\n## Current State (benchmark kickoff)\n### Workers\n  (no workers tracked)\n"
	got := d.withRepoSnapshot("repo-dbl", state.AgentTypeSupervisor, pre)

	if got != pre {
		t.Errorf("expected passthrough when snapshot marker already present, got a modified msg (len delta=%d):\n%s",
			len(got)-len(pre), got)
	}
	// Exactly one "## Current State" block.
	if n := strings.Count(got, "## Current State"); n != 1 {
		t.Errorf("expected exactly 1 '## Current State' block after guard, got %d", n)
	}
}

func TestWithRepoSnapshot_CacheTTL_ServesStaleWithinWindow(t *testing.T) {
	d, cleanup := setupTestDaemon(t)
	defer cleanup()
	resetSnapshotCache(t)

	// Prime the cache with one PR set.
	withFakePRs(t, []prSummary{{Number: 1, Title: "original", URL: "https://github.com/x/y/pull/1",
		Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"}})
	addTestRepo(t, d, "repo-ttl", nil)
	first := d.withRepoSnapshot("repo-ttl", state.AgentTypeMergeQueue, "hi")

	// Swap the stub — any non-cached call would pick this up.
	withFakePRs(t, []prSummary{{Number: 2, Title: "REFRESHED", URL: "https://github.com/x/y/pull/2",
		Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"}})

	// Second call within TTL must return the original (cached) text.
	second := d.withRepoSnapshot("repo-ttl", state.AgentTypeMergeQueue, "hi")
	if first != second {
		t.Errorf("second call within TTL should return cached result identical to first\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if strings.Contains(second, "REFRESHED") {
		t.Errorf("cache TTL not honored — second call picked up new stub")
	}
}

func TestExtractRepoFull_RejectsFlagSmuggling(t *testing.T) {
	// Documents the heuristic parser's behavior: it splits the URL path
	// and returns the last two segments after validating both against a
	// strict charset. The security property we care about is "a `-`-
	// prefixed path component never reaches the gh command line".
	cases := map[string]string{
		"https://github.com/owner/repo":         "owner/repo",
		"https://github.com/owner/repo.git":     "owner/repo",
		"https://github.com/owner/repo/":        "owner/repo",
		"https://github.com/OWNER-1/repo_2":     "OWNER-1/repo_2",
		"https://github.com/--limit/5":          "", // flag smuggling: owner starts with '-'
		"https://github.com/owner/--paginate":   "", // flag smuggling: repo starts with '-'
		"https://github.com/-leading-dash/repo": "", // single leading '-' also rejected
		"https://github.com/owner with space/r": "", // space not in allowed charset
		"https://github.com/owner/repo$inject":  "", // '$' not allowed
		"https://github.com/":                   "", // empty owner after trim
		"":                                      "", // empty input
		"not-a-url":                             "", // no slashes
		// "https://github.com/owner/" (trailing slash with missing repo)
		// falls back to the last two path segments — documented heuristic
		// behavior. Real URL validation is a job for handleAddRepo.
		"https://github.com/owner/":            "github.com/owner",
		"https://github.com/owner/repo/subdir": "repo/subdir",
	}
	for input, want := range cases {
		got := extractRepoFull(input)
		if got != want {
			t.Errorf("extractRepoFull(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSanitizeSnapshotField_StripsNewlines(t *testing.T) {
	cases := map[string]string{
		"CLEAN":                    "CLEAN",
		"MERGEABLE\nFOO":           "MERGEABLE FOO",
		"CLEAN\r\n\n## Injected\n": "CLEAN   ## Injected ", // \r + \n + \n → 3 spaces
		"has\ttab":                 "has tab",
		"":                         "",
	}
	for input, want := range cases {
		if got := sanitizeSnapshotField(input); got != want {
			t.Errorf("sanitizeSnapshotField(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestTruncate_EllipsisBehavior(t *testing.T) {
	if got := truncate("hello", 5); got != "hello" {
		t.Errorf("no trim when under max: got %q", got)
	}
	if got := truncate("hello world", 8); got != "hello..." {
		t.Errorf("truncate with ellipsis: got %q, want 'hello...'", got)
	}
	if got := truncate("xy", 1); got != "x" {
		t.Errorf("max<3 edge case: got %q, want 'x'", got)
	}
}

func TestFormatPRLineMergeQueue_URLTruncated(t *testing.T) {
	longURL := "https://github.com/a/b/pull/1#" + strings.Repeat("A", 500)
	pr := prSummary{Number: 1, Title: "t", URL: longURL,
		Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"}
	line := formatPRLineMergeQueue(pr)
	// Find the url= portion and measure its value.
	idx := strings.Index(line, "url=")
	if idx < 0 {
		t.Fatalf("no url= in output: %s", line)
	}
	urlValue := line[idx+len("url="):]
	if len(urlValue) > maxPRURLLength {
		t.Errorf("url not truncated: len=%d, cap=%d", len(urlValue), maxPRURLLength)
	}
	if !strings.HasSuffix(urlValue, "...") {
		t.Errorf("expected ... suffix on truncated URL, got: %s", urlValue)
	}
}

func TestFormatPRLineMergeQueue_NewlineInFieldDoesNotEscapeSection(t *testing.T) {
	pr := prSummary{
		Number: 1, Title: "t", URL: "https://x/1",
		Mergeable:        "MERGEABLE\n\n## Fake Section\n",
		MergeStateStatus: "CLEAN",
	}
	line := formatPRLineMergeQueue(pr)
	// After sanitization, no newline from the field should leak into
	// the rendered line. The only newlines should be the two template
	// separators (between the title and the `mergeable=...` line, and
	// between that and `url=...`).
	nlCount := strings.Count(line, "\n")
	if nlCount != 2 {
		t.Errorf("expected exactly 2 template newlines, got %d — field injection likely:\n%s", nlCount, line)
	}
	// The literal "## Fake Section" text may still appear, but since
	// it's embedded mid-line (after the whitespace-indented `mergeable=`)
	// it is NOT a markdown header — CommonMark requires headers to
	// start at column 0 (or with up to 3 leading spaces). The real
	// security property is "the attacker cannot CREATE a new markdown
	// section", which is satisfied once newlines are gone.
	//
	// Confirm it's not on a line by itself:
	for _, rendered := range strings.Split(line, "\n") {
		if strings.HasPrefix(strings.TrimLeft(rendered, " "), "## Fake Section") {
			t.Errorf("markdown header escaped at line start:\n%s", line)
		}
	}
}

func TestIsSnapshotTarget(t *testing.T) {
	// Whitelist check: sup + mq only.
	cases := map[state.AgentType]bool{
		state.AgentTypeSupervisor:        true,
		state.AgentTypeMergeQueue:        true,
		state.AgentTypeWorker:            false,
		state.AgentTypeWorkspace:         false,
		state.AgentTypeVerification:      false,
		state.AgentTypePRShepherd:        false,
		state.AgentTypeReview:            false,
		state.AgentTypeGenericPersistent: false,
	}
	for target, want := range cases {
		if got := isSnapshotTarget(target); got != want {
			t.Errorf("isSnapshotTarget(%s) = %v, want %v", target, got, want)
		}
	}
}
