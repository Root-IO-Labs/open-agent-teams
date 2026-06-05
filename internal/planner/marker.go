package planner

import "strings"

// markerPrefix is the opening token of a planner worker marker.
const markerPrefix = "[planner-task:"

// TaskMarker formats the worker task marker that links a spawned worker back to
// a specific plan task. The plan ID scopes the marker so task IDs (e.g. "T1")
// cannot collide across different plans executed in the same repo.
//
//	TaskMarker("plan-123", "T1") == "[planner-task:plan-123:T1]"
func TaskMarker(planID, taskID string) string {
	return markerPrefix + planID + ":" + taskID + "]"
}

// ParseTaskMarker extracts the plan ID and task ID from the first
// [planner-task:<plan-id>:<task-id>] marker found in text.
//
// For backward compatibility, a legacy two-segment marker without a plan ID,
// [planner-task:<task-id>], returns an empty planID and the task ID. Callers
// that need plan isolation should treat an empty planID as "unscoped" and
// decide whether to accept it.
//
// Returns ("", "") when no well-formed marker is present.
func ParseTaskMarker(text string) (planID, taskID string) {
	start := strings.Index(text, markerPrefix)
	if start < 0 {
		return "", ""
	}
	start += len(markerPrefix)
	rel := strings.Index(text[start:], "]")
	if rel < 0 {
		return "", ""
	}
	content := strings.TrimSpace(text[start : start+rel])
	if content == "" {
		return "", ""
	}
	// Split into at most two segments: <plan-id>:<task-id>. Plan IDs and task
	// IDs produced by the planner never contain ':' themselves.
	if parts := strings.SplitN(content, ":", 2); len(parts) == 2 {
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	}
	return "", content
}
