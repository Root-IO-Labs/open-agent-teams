package daemon

import "testing"

func TestNoteAssistantToolStartOpen_DedupeUntilEnd(t *testing.T) {
	open := map[string]bool{}
	if !noteAssistantToolStartOpen(open, "write_file") {
		t.Fatal("first start should publish")
	}
	if noteAssistantToolStartOpen(open, "write_file") {
		t.Fatal("second start (args-ready) must not publish another row")
	}
	if noteAssistantToolStartOpen(open, "browser_click") {
		// different tool — ok
	} else {
		t.Fatal("different tool should publish")
	}
	delete(open, "write_file")
	if !noteAssistantToolStartOpen(open, "write_file") {
		t.Fatal("after tool_end, same tool may open a fresh row")
	}
}

func TestNoteAssistantToolStartOpen_EmptySafe(t *testing.T) {
	if noteAssistantToolStartOpen(nil, "write_file") {
		t.Fatal("nil map must not publish")
	}
	if noteAssistantToolStartOpen(map[string]bool{}, "") {
		t.Fatal("empty tool must not publish")
	}
}

func TestAssistantToolRowState_GeneratingAfterEndDoesNotReopen(t *testing.T) {
	s := newAssistantToolRowState()
	if !s.noteGeneratingStart("ping") {
		t.Fatal("first generating should open")
	}
	if s.noteGeneratingStart("ping") {
		t.Fatal("duplicate generating must not reopen")
	}
	s.noteToolEnd("ping")
	if s.noteGeneratingStart("ping") {
		t.Fatal("stale generating after RESULT must not reopen (orphan RUNNING bug)")
	}
	if len(s.openTools()) != 0 {
		t.Fatalf("openTools after end = %v; want empty", s.openTools())
	}
}

func TestAssistantToolRowState_ToolHeaderAllowsSecondCall(t *testing.T) {
	s := newAssistantToolRowState()
	if !s.noteGeneratingStart("ping") {
		t.Fatal("first generating should open")
	}
	s.noteToolEnd("ping")
	// Real second call: TOOL: header clears the ended latch.
	if !s.noteToolHeaderStart("ping") {
		t.Fatal("TOOL: after end should open a fresh row")
	}
	if s.noteGeneratingStart("ping") {
		t.Fatal("generating while open must dedupe")
	}
	s.noteToolEnd("ping")
}

func TestAssistantToolRowState_GeneratingThenToolHeaderSharesRow(t *testing.T) {
	s := newAssistantToolRowState()
	if !s.noteGeneratingStart("write_file") {
		t.Fatal("generating should open")
	}
	if s.noteToolHeaderStart("write_file") {
		t.Fatal("args-ready TOOL must share the generating row")
	}
}

func TestAssistantToolRowState_OpenToolsAndReset(t *testing.T) {
	s := newAssistantToolRowState()
	_ = s.noteGeneratingStart("ping")
	_ = s.noteGeneratingStart("ls")
	got := s.openTools()
	if len(got) != 2 {
		t.Fatalf("openTools = %v; want 2", got)
	}
	s.reset()
	if len(s.openTools()) != 0 {
		t.Fatal("reset must clear open tools")
	}
	// After reset, generating may open again (new turn).
	if !s.noteGeneratingStart("ping") {
		t.Fatal("after reset, generating should open")
	}
}
