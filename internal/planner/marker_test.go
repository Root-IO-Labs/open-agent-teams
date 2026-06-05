package planner

import "testing"

func TestTaskMarkerRoundTrip(t *testing.T) {
	marker := TaskMarker("plan-123", "T1")
	if marker != "[planner-task:plan-123:T1]" {
		t.Fatalf("TaskMarker = %q", marker)
	}
	planID, taskID := ParseTaskMarker("please do [planner-task:plan-123:T1] now")
	if planID != "plan-123" || taskID != "T1" {
		t.Fatalf("ParseTaskMarker = (%q, %q), want (plan-123, T1)", planID, taskID)
	}
}

func TestParseTaskMarkerLegacyHasNoPlanID(t *testing.T) {
	planID, taskID := ParseTaskMarker("[planner-task:T1] scaffold")
	if planID != "" {
		t.Fatalf("legacy planID = %q, want empty", planID)
	}
	if taskID != "T1" {
		t.Fatalf("legacy taskID = %q, want T1", taskID)
	}
}

func TestParseTaskMarkerMalformed(t *testing.T) {
	cases := []string{
		"no marker here",
		"[planner-task:unterminated",
		"[planner-task:]",
		"",
	}
	for _, c := range cases {
		if planID, taskID := ParseTaskMarker(c); planID != "" || taskID != "" {
			t.Fatalf("ParseTaskMarker(%q) = (%q, %q), want empty", c, planID, taskID)
		}
	}
}

func TestParseTaskMarkerTrimsWhitespace(t *testing.T) {
	planID, taskID := ParseTaskMarker("[planner-task: plan-9 : T2 ]")
	if planID != "plan-9" || taskID != "T2" {
		t.Fatalf("ParseTaskMarker = (%q, %q), want (plan-9, T2)", planID, taskID)
	}
}
