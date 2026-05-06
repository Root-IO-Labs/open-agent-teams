package routing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// OutcomeRecord is one line of the append-only routing history. Written at
// worker/review agent completion. Downstream consumers: benchmarks/routing-replay
// harness, future self-learning routers, central-DB ingest (opt-in only).
//
// Schema is versioned so changes don't silently corrupt historical analyses.
// Additions are backward-compatible (new optional fields only). Removals or type
// changes require a major version bump and a migration story.
//
// Schema history:
//
//	v1: initial — task_text, model, tokens, wall, outcome, pr_number/url/status,
//	    summary, failure_reason, escalated_from, operator_override
//	v2: routing-intelligence Phase 1 — verify_passed, escalation_count, provider,
//	    model_canonical, task_features, pr_state_history, post_merge_revert_within_7d,
//	    post_merge_followup_within_24h, plus identity/reproducibility (record_id,
//	    oat_version, pricing_snapshot_id) and removal_reason for outcome="removed".
type OutcomeRecord struct {
	SchemaVersion int `json:"schema_version"`

	// v2: globally unique, sortable record identifier. UUIDv7 — first 48 bits
	// are millisecond-precision timestamp, so lexical sort = chronological.
	// Generated at log time. Stable forever for joins, dedup, and migration.
	RecordID string `json:"record_id,omitempty"`

	// v2: build-time release identity. Lets analyses bucket records by binary
	// version when router behavior changes between releases. Format:
	// "<semver> (<short-sha>)" or "dev" for unbuilt.
	OATVersion string `json:"oat_version,omitempty"`

	// v2: short hash of the pricing registry at log time. Different hash =
	// different prices = "this record's cost basis differs from later records."
	// Empty when no pricing was loaded.
	PricingSnapshotID string `json:"pricing_snapshot_id,omitempty"`

	TS        string `json:"ts"` // ISO-8601 UTC, written at completion
	Repo      string `json:"repo"`
	Worker    string `json:"worker"`     // agent name
	AgentType string `json:"agent_type"` // worker | review | verification

	// What was asked
	TaskText string `json:"task_text"`
	IssueNum string `json:"issue_number,omitempty"`

	// Routing decision
	Model         string `json:"model"`          // the one that actually ran (full ID, e.g. claude-3-5-sonnet-20241022)
	RoutingSource string `json:"routing_source"` // router | operator-explicit | restart-fallback | repo-default | unknown

	// v2: routing-decision context for counterfactual replay. DecisionReason
	// is what the router said (human-readable). Candidates is the full ranked
	// candidate list at decision time, first entry = the pick. Allowlist is
	// the repo's worker allowlist snapshot. Together these let analyses ask
	// "if router X had been live, what would it have picked?" without
	// reconstructing the live state.
	DecisionReason       string   `json:"decision_reason,omitempty"`
	CandidatesConsidered []string `json:"candidates_considered,omitempty"`
	Allowlist            []string `json:"allowlist,omitempty"`

	// v2: prompt + tool-surface metadata. Hashes only — raw content is never
	// stored here. Populated at spawn (system prompt, user message) and
	// optionally enriched by sidecar after spawn (tool defs, skills).
	Prompt *PromptMetadata `json:"prompt,omitempty"`

	// v2: model identity for staleness handling. Provider lets analyses bucket
	// across model-ID rotations; ModelCanonical maps point releases to stable
	// names (claude-3-5-sonnet-20241022 -> claude-sonnet-3-5).
	Provider       string `json:"provider,omitempty"`        // anthropic | openai | google | bedrock | local | unknown
	ModelCanonical string `json:"model_canonical,omitempty"` // stable identifier across point releases

	// Execution
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at"`
	WallMs      int64  `json:"wall_ms"`
	TokensIn    int64  `json:"tokens_in"`
	TokensOut   int64  `json:"tokens_out"`
	CacheRead   int64  `json:"cache_read,omitempty"`
	CacheWrite  int64  `json:"cache_write,omitempty"`

	// Outcome
	Outcome       string `json:"outcome"` // completed | removed | killed | timed-out | recovered
	PRNumber      int    `json:"pr_number,omitempty"`
	PRURL         string `json:"pr_url,omitempty"`
	PRStatus      string `json:"pr_status,omitempty"` // merged | open | closed | unknown (initial snapshot; see PRStateHistory for backfill series)
	Summary       string `json:"summary,omitempty"`
	FailureReason string `json:"failure_reason,omitempty"`
	EscalatedFrom string `json:"escalated_from,omitempty"` // worker name if this is a re-route

	// v2: disambiguates outcome="removed". Without this field, "removed" was
	// a catch-all that could mean any of: supervisor force-killed a failed
	// worker, a re-route superseded an in-flight worker, the operator killed
	// the daemon mid-task, the budget cap tripped, or the agent self-reported
	// failure. Each has a different success-label interpretation.
	//
	// Values: failed | superseded | manual | timeout | budget_exceeded |
	// daemon_restart. Empty when outcome is not "removed".
	RemovalReason string `json:"removal_reason,omitempty"`

	// v2: stronger immediate success signal than `outcome`. Set true if the
	// daemon's verification step passed for this agent. Pointer so absent
	// (legacy v1 records, not-yet-verified workers) stays distinguishable from
	// confirmed-false.
	VerifyPassed *bool `json:"verify_passed,omitempty"`

	// v2: cascade-escalation depth. 0 = first-try. >0 = re-routed N times after
	// failures. Pairs with EscalatedFrom (which gives the prior worker's name).
	EscalationCount int `json:"escalation_count,omitempty"`

	// v2: structural features extracted at log time. Cheap, fully local, no
	// parser deps. Populated by ExtractLoggedFeatures (task_features.go).
	// Distinct from the classifier's TaskFeatures (task_classifier.go) which
	// is used at routing decision time, not log time.
	TaskFeatures *LoggedTaskFeatures `json:"task_features,omitempty"`

	// v2: PR state snapshots at multiple lags (1h, 24h, 7d). Populated by
	// backfill goroutine. Phase 4 training picks the right delay; logging
	// time-series lets us answer "merged within an hour" vs "merged after a
	// week of nags" without re-fetching.
	PRStateHistory []PRStateSnapshot `json:"pr_state_history,omitempty"`

	// v2: post-merge negative signals from backfill. Tristate via pointer:
	// nil = not yet checked, false = confirmed clean, true = confirmed bad.
	PostMergeRevertWithin7d    *bool `json:"post_merge_revert_within_7d,omitempty"`
	PostMergeFollowupWithin24h *bool `json:"post_merge_followup_within_24h,omitempty"`

	// Operator signals
	OperatorOverride bool `json:"operator_override,omitempty"` // true if --model was passed explicitly

	// v2: privacy + consent metadata. Stamped at write time so a future
	// reader can tell whether free-text fields are present and what
	// upload-consent the user was in. See privacy.go.
	Privacy *PrivacyMetadata `json:"privacy,omitempty"`
}

// PRStateSnapshot is one observation of a PR's state captured by the backfill
// goroutine. Multiple snapshots (at increasing lags) build the state history.
type PRStateSnapshot struct {
	TS        string `json:"ts"`                   // ISO-8601 UTC of the snapshot
	State     string `json:"state"`                // open | merged | closed (gh pr view --json state)
	MergedAt  string `json:"merged_at,omitempty"`  // ISO-8601 UTC if merged
	ClosedAt  string `json:"closed_at,omitempty"`  // ISO-8601 UTC if closed without merge
	LagBucket string `json:"lag_bucket,omitempty"` // "1h" | "24h" | "7d" — which scheduled bucket this snapshot satisfies

	// CI status from gh pr view --json statusCheckRollup. Captures the full-PR
	// rollup at observation time, not per-check granularity (per-check would
	// inflate snapshot size 10-50x with diminishing analytic value). Values:
	//   "passed"  — all required checks succeeded
	//   "failed"  — at least one required check failed
	//   "pending" — checks still running
	//   "none"    — repo has no CI configured / no checks reported
	//   ""        — gh did not return rollup (parsing failure or older gh)
	CIStatus string `json:"ci_status,omitempty"`

	// First failing required check, if any. Lets analyses bucket failures by
	// check type (e.g. "tests" vs "lint") without storing the whole rollup.
	CIFirstFailure string `json:"ci_first_failure,omitempty"`
}

// outcomeLoggerSchemaVersion is the schema this binary writes. Records on disk
// may have older versions; readers must tolerate forward AND backward gaps.
const outcomeLoggerSchemaVersion = 2

// OutcomeLogger appends OutcomeRecord lines to ~/.oat/routing-history.jsonl.
// Thread-safe for concurrent writers. Never blocks the daemon — errors log and continue.
type OutcomeLogger struct {
	path    string
	mu      sync.Mutex
	log     func(format string, args ...any) // daemon warn
	privacy PrivacyMode                      // resolved at construction
	userID  string                           // set on first share opt-in; empty otherwise
}

// NewOutcomeLogger returns an active logger. `path` should be an absolute file path.
// Parent directory must exist. Errors during writes are reported via the warn callback;
// they never propagate to callers so routing/completion flows aren't blocked by a
// full disk.
//
// Honors the OAT_OUTCOME_LOG=off kill switch by returning nil. A nil receiver's
// Log() is a no-op (see existing TestOutcomeLogger_NilReceiverNoop), so callers
// never need a nil check.
//
// Privacy mode is read once from OAT_LOG_PRIVACY at construction. Changing the
// env mid-process won't change behavior for an existing logger — that's
// intentional: privacy is a per-record decision but must be stable for the
// daemon's lifetime so the on-disk corpus is consistent.
func NewOutcomeLogger(path string, warn func(format string, args ...any)) *OutcomeLogger {
	if IsLoggingDisabled() {
		return nil
	}
	if warn == nil {
		warn = func(string, ...any) {}
	}
	return &OutcomeLogger{
		path:    path,
		log:     warn,
		privacy: CurrentPrivacyMode(),
	}
}

// SetUserID stamps the per-install UUID on every subsequent record. Empty
// pre-share-optin (no UserID is generated until the user runs `oat routing
// share` for the first time). Safe to call concurrently.
func (l *OutcomeLogger) SetUserID(id string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.userID = id
}

// PrivacyMode returns the mode this logger was constructed with. Read-only.
func (l *OutcomeLogger) PrivacyMode() PrivacyMode {
	if l == nil {
		return PrivacyModeLocal
	}
	return l.privacy
}

// Log appends a single record. Called under daemon completion hook.
// Never returns an error — logs and swallows. Routing history is nice-to-have,
// not life-support.
//
// Sanitization (per privacy mode) happens here, before serialization. Strict
// mode redacts task_text/summary/failure_reason; other modes only stamp the
// privacy metadata. Hashes are preserved across all modes.
func (l *OutcomeLogger) Log(rec OutcomeRecord) {
	if l == nil || l.path == "" {
		return
	}
	if rec.SchemaVersion == 0 {
		rec.SchemaVersion = outcomeLoggerSchemaVersion
	}
	if rec.TS == "" {
		rec.TS = time.Now().UTC().Format(time.RFC3339)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	applyPrivacy(&rec, l.privacy, l.userID)

	// Ensure parent dir exists (cheap, idempotent).
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		l.log("outcome_logger: mkdir %s failed: %v", filepath.Dir(l.path), err)
		return
	}

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		l.log("outcome_logger: open %s failed: %v", l.path, err)
		return
	}
	defer f.Close()

	data, err := json.Marshal(rec)
	if err != nil {
		l.log("outcome_logger: marshal record failed: %v", err)
		return
	}
	if _, err := fmt.Fprintln(f, string(data)); err != nil {
		l.log("outcome_logger: write failed: %v", err)
	}
}

// DefaultOutcomeHistoryPath returns the standard ~/.oat/routing-history.jsonl.
func DefaultOutcomeHistoryPath(oatRoot string) string {
	return filepath.Join(oatRoot, "routing-history.jsonl")
}
