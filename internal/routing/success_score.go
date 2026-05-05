package routing

// SuccessScoreBasis identifies which evidence contributed to the score.
// Used for slicing the corpus during analysis ("only records where success
// was confirmed by PR merge" vs "records where success was self-reported").
type SuccessScoreBasis string

const (
	BasisPRMerged             SuccessScoreBasis = "pr_merged"
	BasisPRMergedWithFollowup SuccessScoreBasis = "pr_merged_with_followup"
	BasisPRMergedThenReverted SuccessScoreBasis = "pr_merged_then_reverted"
	BasisPRClosedUnmerged     SuccessScoreBasis = "pr_closed_unmerged"
	BasisPRStillOpen          SuccessScoreBasis = "pr_still_open"
	BasisRecoveredViaPRMerge  SuccessScoreBasis = "recovered_via_pr_merge"
	BasisVerifierApprovedNoPR SuccessScoreBasis = "verifier_approved_no_pr"
	BasisVerifierRejected     SuccessScoreBasis = "verifier_rejected"
	BasisSelfReportComplete   SuccessScoreBasis = "self_report_complete"
	BasisRemovedFailed        SuccessScoreBasis = "removed_failed"
	BasisRemovedSuperseded    SuccessScoreBasis = "removed_superseded"
	BasisRemovedManual        SuccessScoreBasis = "removed_manual"
	BasisBudgetExceeded       SuccessScoreBasis = "budget_exceeded"
	BasisTimeout              SuccessScoreBasis = "timeout"
	BasisDaemonRestart        SuccessScoreBasis = "daemon_restart"
	BasisRemovedUnspecified   SuccessScoreBasis = "removed_unspecified"
	BasisKilledOrTimeout      SuccessScoreBasis = "killed_or_timeout"
	BasisUnknown              SuccessScoreBasis = "unknown"
)

// DeriveSuccessScore reduces an OutcomeRecord's evidence into a single scalar
// in [0, 1] plus a basis tag that names which evidence won. The score is the
// primary label for replay grading and any future ML training; the basis lets
// callers filter by evidence quality (e.g., "only records confirmed by PR
// merge" for high-confidence analyses).
//
// `hasScore` is false for records that are unscoreable today — typically
// in-flight work (PR still open) or operator actions (manual remove). Such
// records SHOULD be excluded from training and from per-model success-rate
// summaries; including them biases the model away from the true distribution.
//
// Priority hierarchy (strongest evidence wins):
//
//  1. Terminal PR state (merged → 1.0, with downgrades for revert/followup;
//     closed → 0.0; still open → unscoreable).
//  2. "recovered" outcome (failed worker but PR eventually merged — set by
//     daemon's pr_monitor.go backfillRecoveredTasks).
//  3. Verifier verdict (approved → 0.7; rejected → 0.0).
//  4. outcome="completed" with no further signal (self-reported, weakest
//     positive evidence — 0.5).
//  5. outcome="removed" with reason (failed/budget/timeout → 0.0;
//     superseded/manual/daemon_restart → unscoreable).
//  6. outcome="killed" or "timed-out" (legacy values) → 0.0.
//  7. Anything else → unscoreable.
//
// CI status would slot between PR-merged and verifier-only when added; until
// then the function gracefully degrades by trusting PR merge alone.
func DeriveSuccessScore(rec OutcomeRecord) (score float64, basis SuccessScoreBasis, hasScore bool) {
	prState := terminalPRState(rec.PRStateHistory)
	reverted := rec.PostMergeRevertWithin7d != nil && *rec.PostMergeRevertWithin7d
	followup := rec.PostMergeFollowupWithin24h != nil && *rec.PostMergeFollowupWithin24h

	switch prState {
	case "merged":
		switch {
		case reverted:
			return 0.1, BasisPRMergedThenReverted, true
		case followup:
			return 0.6, BasisPRMergedWithFollowup, true
		default:
			return 1.0, BasisPRMerged, true
		}
	case "closed":
		return 0.0, BasisPRClosedUnmerged, true
	case "open":
		return 0, BasisPRStillOpen, false
	}

	if rec.Outcome == "recovered" {
		return 0.95, BasisRecoveredViaPRMerge, true
	}

	if rec.VerifyPassed != nil {
		if *rec.VerifyPassed {
			return 0.7, BasisVerifierApprovedNoPR, true
		}
		return 0.0, BasisVerifierRejected, true
	}

	switch rec.Outcome {
	case "completed":
		return 0.5, BasisSelfReportComplete, true
	case "removed":
		switch rec.RemovalReason {
		case "failed":
			return 0.0, BasisRemovedFailed, true
		case "superseded":
			return 0, BasisRemovedSuperseded, false
		case "manual":
			return 0, BasisRemovedManual, false
		case "budget_exceeded":
			return 0.0, BasisBudgetExceeded, true
		case "timeout":
			return 0.0, BasisTimeout, true
		case "daemon_restart":
			return 0, BasisDaemonRestart, false
		default:
			return 0.0, BasisRemovedUnspecified, true
		}
	case "killed", "timed-out":
		return 0.0, BasisKilledOrTimeout, true
	}

	return 0, BasisUnknown, false
}

// terminalPRState picks the strongest terminal PR state from the snapshot
// history. "merged" beats "closed" beats "open"; if no terminal state has
// been observed, the most recent snapshot wins. Empty history returns "".
//
// Why prefer terminal: a record with one "open" snapshot at 1h and one
// "merged" snapshot at 24h should clearly count as merged, not as in-flight.
// The 24h observation supersedes the 1h.
func terminalPRState(history []PRStateSnapshot) string {
	if len(history) == 0 {
		return ""
	}
	hasMerged := false
	hasClosed := false
	hasOpen := false
	for _, s := range history {
		switch s.State {
		case "merged":
			hasMerged = true
		case "closed":
			hasClosed = true
		case "open":
			hasOpen = true
		}
	}
	switch {
	case hasMerged:
		return "merged"
	case hasClosed:
		return "closed"
	case hasOpen:
		return "open"
	default:
		return history[len(history)-1].State
	}
}
