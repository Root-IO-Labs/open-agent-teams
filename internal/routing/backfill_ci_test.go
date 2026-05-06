package routing

import "testing"

func TestSummarizeCheckRollup_Table(t *testing.T) {
	tests := []struct {
		name        string
		in          []ghCheckEntry
		wantStatus  string
		wantFailure string
	}{
		{
			name:       "empty_rollup",
			in:         nil,
			wantStatus: "none",
		},
		{
			name: "all_passed_check_objects",
			in: []ghCheckEntry{
				{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"},
				{Name: "lint", Status: "COMPLETED", Conclusion: "SUCCESS"},
			},
			wantStatus: "passed",
		},
		{
			name: "single_failure_first_listed",
			in: []ghCheckEntry{
				{Name: "lint", Status: "COMPLETED", Conclusion: "SUCCESS"},
				{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"},
			},
			wantStatus:  "failed",
			wantFailure: "test",
		},
		{
			name: "two_failures_first_wins",
			in: []ghCheckEntry{
				{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"},
				{Name: "lint", Status: "COMPLETED", Conclusion: "FAILURE"},
			},
			wantStatus:  "failed",
			wantFailure: "test",
		},
		{
			name: "pending_no_failure_yet",
			in: []ghCheckEntry{
				{Name: "test", Status: "IN_PROGRESS"},
			},
			wantStatus: "pending",
		},
		{
			name: "pending_plus_passed",
			in: []ghCheckEntry{
				{Name: "lint", Status: "COMPLETED", Conclusion: "SUCCESS"},
				{Name: "test", Status: "QUEUED"},
			},
			wantStatus: "pending",
		},
		{
			name: "failure_outranks_pending",
			in: []ghCheckEntry{
				{Name: "test", Status: "IN_PROGRESS"},
				{Name: "lint", Status: "COMPLETED", Conclusion: "FAILURE"},
			},
			wantStatus:  "failed",
			wantFailure: "lint",
		},
		{
			name: "skipped_neutral_treated_as_passed",
			in: []ghCheckEntry{
				{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"},
				{Name: "skipped-task", Status: "COMPLETED", Conclusion: "SKIPPED"},
				{Name: "neutral-task", Status: "COMPLETED", Conclusion: "NEUTRAL"},
			},
			wantStatus: "passed",
		},
		{
			name: "timed_out_is_failure",
			in: []ghCheckEntry{
				{Name: "test", Status: "COMPLETED", Conclusion: "TIMED_OUT"},
			},
			wantStatus:  "failed",
			wantFailure: "test",
		},
		{
			name: "status_context_passed",
			in: []ghCheckEntry{
				{Context: "ci/jenkins", State: "SUCCESS"},
			},
			wantStatus: "passed",
		},
		{
			name: "status_context_pending",
			in: []ghCheckEntry{
				{Context: "ci/jenkins", State: "PENDING"},
			},
			wantStatus: "pending",
		},
		{
			name: "status_context_failure",
			in: []ghCheckEntry{
				{Context: "ci/jenkins", State: "FAILURE"},
			},
			wantStatus:  "failed",
			wantFailure: "ci/jenkins",
		},
		{
			name: "mixed_check_and_status_context",
			in: []ghCheckEntry{
				{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"},
				{Context: "ci/jenkins", State: "FAILURE"},
			},
			wantStatus:  "failed",
			wantFailure: "ci/jenkins",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotStatus, gotFailure := summarizeCheckRollup(tt.in)
			if gotStatus != tt.wantStatus {
				t.Errorf("status = %q, want %q", gotStatus, tt.wantStatus)
			}
			if gotFailure != tt.wantFailure {
				t.Errorf("firstFailure = %q, want %q", gotFailure, tt.wantFailure)
			}
		})
	}
}
