package daemon

// noteAssistantToolStartOpen tracks whether we already published a
// tool_start for an in-flight assistant tool call. Returns true when a
// NEW tool_start should be broadcast. Early generating TOOL /
// [OAT_GENERATING] and the later args-ready TOOL share one RUNNING row;
// publishing both left orphan RUNNING + OK pairs in the side panel.
// Cleared by the caller on tool_end (delete open[tool]).
func noteAssistantToolStartOpen(open map[string]bool, tool string) bool {
	if open == nil || tool == "" {
		return false
	}
	if open[tool] {
		return false
	}
	open[tool] = true
	return true
}
