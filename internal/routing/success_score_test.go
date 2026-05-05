package routing

import "testing"

func boolp(b bool) *bool { return &b }

// TestDeriveSuccessScore_Table exercises every priority branch. New rows
// added here also serve as the schema-v2 success-signal documentation —
// what each combination of evidence is worth.
func TestDeriveSuccessScore_Table(t *testing.T) {
	tests := []struct {
		name     string
		rec      OutcomeRecord
		want     float64
		basis    SuccessScoreBasis
		hasScore bool
	}{
		// PR-merged tier
		{
			name: "pr_merged_clean",
			rec: OutcomeRecord{
				PRStateHistory: []PRStateSnapshot{{State: "merged", LagBucket: "1h"}},
			},
			want: 1.0, basis: BasisPRMerged, hasScore: true,
		},
		{
			name: "pr_merged_with_followup",
			rec: OutcomeRecord{
				PRStateHistory:             []PRStateSnapshot{{State: "merged", LagBucket: "24h"}},
				PostMergeFollowupWithin24h: boolp(true),
			},
			want: 0.6, basis: BasisPRMergedWithFollowup, hasScore: true,
		},
		{
			name: "pr_merged_then_reverted",
			rec: OutcomeRecord{
				PRStateHistory:          []PRStateSnapshot{{State: "merged", LagBucket: "7d"}},
				PostMergeRevertWithin7d: boolp(true),
			},
			want: 0.1, basis: BasisPRMergedThenReverted, hasScore: true,
		},
		{
			// Revert outranks followup when both are flagged.
			name: "pr_merged_revert_outranks_followup",
			rec: OutcomeRecord{
				PRStateHistory:             []PRStateSnapshot{{State: "merged"}},
				PostMergeRevertWithin7d:    boolp(true),
				PostMergeFollowupWithin24h: boolp(true),
			},
			want: 0.1, basis: BasisPRMergedThenReverted, hasScore: true,
		},

		// PR-closed / open
		{
			name: "pr_closed_unmerged",
			rec: OutcomeRecord{
				PRStateHistory: []PRStateSnapshot{{State: "closed"}},
			},
			want: 0.0, basis: BasisPRClosedUnmerged, hasScore: true,
		},
		{
			name: "pr_still_open_unscoreable",
			rec: OutcomeRecord{
				PRStateHistory: []PRStateSnapshot{{State: "open"}},
			},
			want: 0, basis: BasisPRStillOpen, hasScore: false,
		},
		{
			// Terminal state at any lag wins over earlier "open" snapshot.
			name: "pr_merged_at_24h_overrides_open_at_1h",
			rec: OutcomeRecord{
				PRStateHistory: []PRStateSnapshot{
					{State: "open", LagBucket: "1h"},
					{State: "merged", LagBucket: "24h"},
				},
			},
			want: 1.0, basis: BasisPRMerged, hasScore: true,
		},

		// Recovered (failed worker, PR eventually merged — daemon's TaskStatusRecovered)
		{
			name: "recovered_outcome",
			rec: OutcomeRecord{
				Outcome: "recovered",
			},
			want: 0.95, basis: BasisRecoveredViaPRMerge, hasScore: true,
		},
		{
			// PR-merged history outranks "recovered" outcome (more direct evidence).
			name: "pr_merged_outranks_recovered",
			rec: OutcomeRecord{
				Outcome:        "recovered",
				PRStateHistory: []PRStateSnapshot{{State: "merged"}},
			},
			want: 1.0, basis: BasisPRMerged, hasScore: true,
		},

		// Verifier verdict
		{
			name: "verifier_approved_no_pr",
			rec: OutcomeRecord{
				Outcome:      "completed",
				VerifyPassed: boolp(true),
			},
			want: 0.7, basis: BasisVerifierApprovedNoPR, hasScore: true,
		},
		{
			name: "verifier_rejected",
			rec: OutcomeRecord{
				Outcome:      "completed",
				VerifyPassed: boolp(false),
			},
			want: 0.0, basis: BasisVerifierRejected, hasScore: true,
		},

		// Self-report only
		{
			name: "self_report_complete",
			rec: OutcomeRecord{
				Outcome: "completed",
			},
			want: 0.5, basis: BasisSelfReportComplete, hasScore: true,
		},

		// Removed cases
		{
			name: "removed_failed",
			rec: OutcomeRecord{
				Outcome:       "removed",
				RemovalReason: "failed",
			},
			want: 0.0, basis: BasisRemovedFailed, hasScore: true,
		},
		{
			name: "removed_superseded_unscoreable",
			rec: OutcomeRecord{
				Outcome:       "removed",
				RemovalReason: "superseded",
			},
			want: 0, basis: BasisRemovedSuperseded, hasScore: false,
		},
		{
			name: "removed_manual_unscoreable",
			rec: OutcomeRecord{
				Outcome:       "removed",
				RemovalReason: "manual",
			},
			want: 0, basis: BasisRemovedManual, hasScore: false,
		},
		{
			name: "removed_budget_exceeded",
			rec: OutcomeRecord{
				Outcome:       "removed",
				RemovalReason: "budget_exceeded",
			},
			want: 0.0, basis: BasisBudgetExceeded, hasScore: true,
		},
		{
			name: "removed_timeout",
			rec: OutcomeRecord{
				Outcome:       "removed",
				RemovalReason: "timeout",
			},
			want: 0.0, basis: BasisTimeout, hasScore: true,
		},
		{
			name: "removed_daemon_restart_unscoreable",
			rec: OutcomeRecord{
				Outcome:       "removed",
				RemovalReason: "daemon_restart",
			},
			want: 0, basis: BasisDaemonRestart, hasScore: false,
		},
		{
			name: "removed_unspecified_reason",
			rec: OutcomeRecord{
				Outcome: "removed",
			},
			want: 0.0, basis: BasisRemovedUnspecified, hasScore: true,
		},

		// Legacy outcomes
		{
			name: "killed",
			rec: OutcomeRecord{
				Outcome: "killed",
			},
			want: 0.0, basis: BasisKilledOrTimeout, hasScore: true,
		},
		{
			name: "timed_out",
			rec: OutcomeRecord{
				Outcome: "timed-out",
			},
			want: 0.0, basis: BasisKilledOrTimeout, hasScore: true,
		},

		// Unknown
		{
			name:     "empty_record",
			rec:      OutcomeRecord{},
			want:     0,
			basis:    BasisUnknown,
			hasScore: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score, basis, has := DeriveSuccessScore(tt.rec)
			if score != tt.want {
				t.Errorf("score = %v, want %v", score, tt.want)
			}
			if basis != tt.basis {
				t.Errorf("basis = %q, want %q", basis, tt.basis)
			}
			if has != tt.hasScore {
				t.Errorf("hasScore = %v, want %v", has, tt.hasScore)
			}
		})
	}
}

// TestDeriveSuccessScore_RangeBounded confirms every (score, hasScore=true)
// output is in [0, 1]. Catches future hierarchy edits that introduce
// out-of-range constants.
func TestDeriveSuccessScore_RangeBounded(t *testing.T) {
	cases := []OutcomeRecord{
		{Outcome: "completed"},
		{Outcome: "removed", RemovalReason: "failed"},
		{Outcome: "removed", RemovalReason: "budget_exceeded"},
		{Outcome: "killed"},
		{Outcome: "recovered"},
		{Outcome: "completed", VerifyPassed: boolp(true)},
		{Outcome: "completed", VerifyPassed: boolp(false)},
		{PRStateHistory: []PRStateSnapshot{{State: "merged"}}},
		{PRStateHistory: []PRStateSnapshot{{State: "merged"}}, PostMergeFollowupWithin24h: boolp(true)},
		{PRStateHistory: []PRStateSnapshot{{State: "merged"}}, PostMergeRevertWithin7d: boolp(true)},
		{PRStateHistory: []PRStateSnapshot{{State: "closed"}}},
	}
	for _, rec := range cases {
		score, basis, has := DeriveSuccessScore(rec)
		if !has {
			continue
		}
		if score < 0 || score > 1 {
			t.Errorf("basis=%q produced score=%v out of [0,1]", basis, score)
		}
	}
}

// TestDeriveSuccessScore_MonotonicInEvidence: adding strictly stronger
// evidence should not lower the score. We check the ladder rung-by-rung.
func TestDeriveSuccessScore_MonotonicInEvidence(t *testing.T) {
	bare := OutcomeRecord{Outcome: "completed"} // 0.5 self-report
	withVerify := bare
	withVerify.VerifyPassed = boolp(true) // → 0.7

	withMerge := withVerify
	withMerge.PRStateHistory = []PRStateSnapshot{{State: "merged"}} // → 1.0

	bareScore, _, _ := DeriveSuccessScore(bare)
	verifyScore, _, _ := DeriveSuccessScore(withVerify)
	mergeScore, _, _ := DeriveSuccessScore(withMerge)

	if !(bareScore <= verifyScore && verifyScore <= mergeScore) {
		t.Errorf("score not monotonic in evidence: bare=%v verify=%v merge=%v",
			bareScore, verifyScore, mergeScore)
	}
}

// TestTerminalPRState_PriorityRules checks the helper directly. Terminal
// states (merged > closed) outrank "open"; with no terminal observation,
// we fall back to the most recent snapshot.
func TestTerminalPRState_PriorityRules(t *testing.T) {
	tests := []struct {
		name string
		in   []PRStateSnapshot
		want string
	}{
		{"empty", nil, ""},
		{"single open", []PRStateSnapshot{{State: "open"}}, "open"},
		{"merged_only", []PRStateSnapshot{{State: "merged"}}, "merged"},
		{"closed_only", []PRStateSnapshot{{State: "closed"}}, "closed"},
		{"merged_beats_closed", []PRStateSnapshot{{State: "closed"}, {State: "merged"}}, "merged"},
		{"merged_beats_open", []PRStateSnapshot{{State: "open"}, {State: "merged"}}, "merged"},
		{"closed_beats_open", []PRStateSnapshot{{State: "open"}, {State: "closed"}}, "closed"},
		{"three_buckets_merged_at_7d", []PRStateSnapshot{
			{State: "open", LagBucket: "1h"},
			{State: "open", LagBucket: "24h"},
			{State: "merged", LagBucket: "7d"},
		}, "merged"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := terminalPRState(tt.in); got != tt.want {
				t.Errorf("terminalPRState(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
