package daemon

// assistantToolRowState tracks side-panel activity rows for one assistant
// turn so we emit exactly one RUNNING row per in-flight call.
//
// Early [OAT_GENERATING] and the later args-ready TOOL: share one row.
// After RESULT (tool_end), leftover generating heartbeats must NOT reopen
// a row — that left orphan RUNNING badges after short tools like ping.
// A later same-named call may open again only via an explicit TOOL: start.
type assistantToolRowState struct {
	open  map[string]bool // currently RUNNING
	ended map[string]bool // completed this turn; block generating reopen
}

func newAssistantToolRowState() *assistantToolRowState {
	return &assistantToolRowState{
		open:  map[string]bool{},
		ended: map[string]bool{},
	}
}

func (s *assistantToolRowState) reset() {
	if s == nil {
		return
	}
	s.open = map[string]bool{}
	s.ended = map[string]bool{}
}

// noteGeneratingStart returns true when [OAT_GENERATING] should open a
// NEW tool_start row. False when already open, or when this tool already
// ended this turn (stale heartbeat after RESULT).
func (s *assistantToolRowState) noteGeneratingStart(tool string) bool {
	if s == nil || tool == "" {
		return false
	}
	if s.open[tool] {
		return false
	}
	if s.ended[tool] {
		return false
	}
	s.open[tool] = true
	return true
}

// noteToolHeaderStart returns true when a TOOL: header should open a NEW
// tool_start row. Allows a second same-named call after tool_end (clears
// the ended latch). Dedupes against an already-open generating row.
func (s *assistantToolRowState) noteToolHeaderStart(tool string) bool {
	if s == nil || tool == "" {
		return false
	}
	if s.open[tool] {
		return false
	}
	delete(s.ended, tool)
	s.open[tool] = true
	return true
}

// noteToolEnd marks the tool no longer RUNNING and latches ended so
// late generating cannot reopen until a fresh TOOL: header.
func (s *assistantToolRowState) noteToolEnd(tool string) {
	if s == nil || tool == "" {
		return
	}
	delete(s.open, tool)
	s.ended[tool] = true
}

// openTools returns tools still marked RUNNING (for synthetic tool_end
// on turn_end). Order is non-deterministic; callers only need the set.
func (s *assistantToolRowState) openTools() []string {
	if s == nil || len(s.open) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.open))
	for tool, ok := range s.open {
		if ok && tool != "" {
			out = append(out, tool)
		}
	}
	return out
}

// noteAssistantToolStartOpen is kept for older tests / call sites that
// only need open-map dedupe (generating + TOOL share one row). Prefer
// assistantToolRowState for the full generating-after-end gate.
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
