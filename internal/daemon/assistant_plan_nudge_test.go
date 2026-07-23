package daemon

import (
	"strings"
	"testing"
)

func TestBuildPlanStaleReprompt(t *testing.T) {
	msg := buildPlanStaleReprompt()
	if !strings.HasPrefix(msg, "[OAT-system]") {
		t.Fatalf("must use [OAT-system] prefix: %q", msg)
	}
	if !strings.Contains(msg, "write_todos") {
		t.Fatalf("must ask for write_todos: %q", msg)
	}
}

func TestPlanStaleTryNudge_OncePerSequence(t *testing.T) {
	c := newPlanStaleController()
	const session, agent = "_assistant-personal", "personal"

	if !c.tryNudge(session, agent) {
		t.Fatal("first nudge should be authorized")
	}
	if c.tryNudge(session, agent) {
		t.Fatal("second nudge in the same sequence must be denied")
	}
	c.noteTodosUpdated(session, agent)
	if !c.tryNudge(session, agent) {
		t.Fatal("after write_todos, a new nudge should be authorized")
	}
	c.resetForUser(session, agent)
	if !c.tryNudge(session, agent) {
		t.Fatal("after resetForUser, nudge should be authorized again")
	}
}

func TestIsPlanProgressTool(t *testing.T) {
	if !isPlanProgressTool("browser_save_screenshot") {
		t.Fatal("screenshot should count as progress")
	}
	if !isPlanProgressTool("write_file") {
		t.Fatal("write_file should count as progress")
	}
	if isPlanProgressTool("browser_snapshot") {
		t.Fatal("snapshot alone should not count as plan progress")
	}
	if isPlanProgressTool("write_todos") {
		t.Fatal("write_todos is the update itself, not progress gate")
	}
}

func TestPlanStaleGates(t *testing.T) {
	// Document the turnEndInfo gates used by maybePlanStaleNudge.
	cases := []struct {
		name string
		info turnEndInfo
		want bool // whether tryNudge path would be reached (pre-daemon checks)
	}{
		{
			name: "progress + unfinished + no todos",
			info: turnEndInfo{HasUnfinishedTodos: true, ProgressToolsOK: true, SawTodosThisTurn: false},
			want: true,
		},
		{
			name: "saw todos this turn",
			info: turnEndInfo{HasUnfinishedTodos: true, ProgressToolsOK: true, SawTodosThisTurn: true},
			want: false,
		},
		{
			name: "no progress tools",
			info: turnEndInfo{HasUnfinishedTodos: true, ProgressToolsOK: false, SawTodosThisTurn: false},
			want: false,
		},
		{
			name: "all todos done",
			info: turnEndInfo{HasUnfinishedTodos: false, ProgressToolsOK: true, SawTodosThisTurn: false},
			want: false,
		},
		{
			name: "chatty turn still eligible",
			info: turnEndInfo{
				HasUnfinishedTodos: true,
				ProgressToolsOK:    true,
				SawTodosThisTurn:   false,
				VisibleReply:       true,
			},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eligible := !tc.info.SawTodosThisTurn && tc.info.HasUnfinishedTodos && tc.info.ProgressToolsOK
			if eligible != tc.want {
				t.Fatalf("eligible=%v want=%v", eligible, tc.want)
			}
		})
	}
}
