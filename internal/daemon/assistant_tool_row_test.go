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
