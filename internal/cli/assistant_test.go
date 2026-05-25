// Tests for the Part 5d `oat assistant` verb tree. Unit-scope only
// (the heavy end-to-end coverage -- start/stop/restart against a
// real daemon -- lives in test/ where a daemon harness already
// exists). What we pin here:
//
//  1. resolveAssistantName: every "no positional name → default"
//     path. The default is the only piece of UX that's invisible
//     to the user, so a regression that silently re-routes them to
//     a different assistant would be hard to spot.
//  2. findAssistantAgentMap: the map-walker tolerates every shape
//     of garbage list_agents could return. Misses are no-ops
//     (callers print "stopped"); we shouldn't panic on any input.
//  3. agentSlug: identity mapping is a deliberate design choice
//     (Part 5d plan body: "agent name == assistant name") because
//     the (repo, agent) tuple is what shows up in `oat agent list
//     --all`. Pin it so a future refactor doesn't append a
//     "-assistant" suffix and break the user's mental model.
//  4. registerAssistantCommands: the verb table is the public
//     surface. Pin the 10 documented verbs (start/stop/restart/
//     status/attach/set-model/reset/compact/logs/list). If
//     somebody drops one accidentally during a refactor, this
//     fails loudly instead of leaving an undocumented gap.

package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Root-IO-Labs/open-agent-teams/pkg/config"
)

func TestResolveAssistantName_Part5d(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no args → personal", []string{}, defaultAssistantName},
		{"empty first arg → personal", []string{""}, defaultAssistantName},
		{"whitespace first arg → personal", []string{"   "}, defaultAssistantName},
		{"flag first arg → personal", []string{"--model"}, defaultAssistantName},
		{"flag short form first arg → personal", []string{"-f"}, defaultAssistantName},
		{"valid name passthrough", []string{"work"}, "work"},
		{"valid name with hyphen passthrough", []string{"my-bot"}, "my-bot"},
		{"valid name with underscore passthrough", []string{"my_bot"}, "my_bot"},
		{"name + extra positional → name only", []string{"work", "extra"}, "work"},
		{"name + flag → name only", []string{"work", "--fresh"}, "work"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveAssistantName(tc.args); got != tc.want {
				t.Errorf("resolveAssistantName(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestAgentSlug_Part5d(t *testing.T) {
	cases := []string{"personal", "work", "my-assistant", "a", strings.Repeat("x", 32)}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if got := agentSlug(in); got != in {
				t.Errorf("agentSlug(%q) = %q, want identity %q", in, got, in)
			}
		})
	}
}

func TestFindAssistantAgentMap_Part5d(t *testing.T) {
	// Synthetic list_agents `rich: true` shape: []map with name+pid
	// (we don't synthesize the full agentDetail because the helper
	// only reads "name").
	makeRow := func(name string, extras map[string]interface{}) map[string]interface{} {
		row := map[string]interface{}{"name": name}
		for k, v := range extras {
			row[k] = v
		}
		return row
	}

	t.Run("hit returns the row", func(t *testing.T) {
		data := []interface{}{
			makeRow("other", nil),
			makeRow("personal", map[string]interface{}{"pid": float64(123), "model": "anthropic:claude-opus-4-7"}),
			makeRow("third", nil),
		}
		got, ok := findAssistantAgentMap(data, "personal")
		if !ok {
			t.Fatal("expected hit, got miss")
		}
		if got["pid"].(float64) != 123 {
			t.Errorf("expected pid=123, got %v", got["pid"])
		}
		if got["model"].(string) != "anthropic:claude-opus-4-7" {
			t.Errorf("expected model passthrough, got %v", got["model"])
		}
	})

	t.Run("miss returns false", func(t *testing.T) {
		data := []interface{}{makeRow("other", nil)}
		if _, ok := findAssistantAgentMap(data, "personal"); ok {
			t.Error("expected miss, got hit")
		}
	})

	t.Run("nil data returns false (does not panic)", func(t *testing.T) {
		if _, ok := findAssistantAgentMap(nil, "personal"); ok {
			t.Error("expected miss on nil, got hit")
		}
	})

	t.Run("non-array data returns false (does not panic)", func(t *testing.T) {
		if _, ok := findAssistantAgentMap("garbage", "personal"); ok {
			t.Error("expected miss on string, got hit")
		}
		if _, ok := findAssistantAgentMap(map[string]interface{}{"x": 1}, "personal"); ok {
			t.Error("expected miss on object, got hit")
		}
	})

	t.Run("array of non-maps returns false (does not panic)", func(t *testing.T) {
		data := []interface{}{"garbage", 42, nil}
		if _, ok := findAssistantAgentMap(data, "personal"); ok {
			t.Error("expected miss on garbage-array, got hit")
		}
	})

	t.Run("row with missing name returns false for that row", func(t *testing.T) {
		data := []interface{}{
			map[string]interface{}{"pid": float64(1)}, // no name
			map[string]interface{}{"name": "personal"},
		}
		got, ok := findAssistantAgentMap(data, "personal")
		if !ok {
			t.Fatal("expected to find personal in row 2 despite garbage row 1")
		}
		if got["name"].(string) != "personal" {
			t.Errorf("matched wrong row: %v", got)
		}
	})

	t.Run("row with wrong-typed name field is skipped", func(t *testing.T) {
		data := []interface{}{
			map[string]interface{}{"name": 42}, // wrong type
			map[string]interface{}{"name": "personal"},
		}
		if _, ok := findAssistantAgentMap(data, "personal"); !ok {
			t.Error("should still find personal despite typed-junk row")
		}
	})
}

// TestRegisterAssistantCommands_Part5d pins the verb table the
// `oat assistant` tree exposes. If a verb is renamed or removed,
// this fails loudly so the user-visible CLI contract doesn't drift
// from the docs (docs/COMMANDS.md, README, Part 5d plan body).
func TestRegisterAssistantCommands_Part5d(t *testing.T) {
	tmpDir := t.TempDir()
	cli := NewWithPaths(config.NewTestPaths(tmpDir))

	root, ok := cli.rootCmd.Subcommands["assistant"]
	if !ok {
		t.Fatal("`oat assistant` command tree not registered")
	}
	if root.Subcommands == nil {
		t.Fatal("assistant command has no subcommands map")
	}

	want := []string{
		"start", "stop", "restart", "status", "attach",
		"set-model", "reset", "compact", "logs", "list",
	}
	for _, verb := range want {
		t.Run(verb, func(t *testing.T) {
			cmd, ok := root.Subcommands[verb]
			if !ok {
				t.Fatalf("`oat assistant %s` not wired", verb)
			}
			if cmd.Run == nil {
				t.Errorf("`oat assistant %s` has no Run function", verb)
			}
			if cmd.Description == "" {
				t.Errorf("`oat assistant %s` has empty description", verb)
			}
			if cmd.Usage == "" {
				t.Errorf("`oat assistant %s` has empty usage string", verb)
			}
		})
	}

	// Negative: make sure we haven't accidentally wired anything
	// beyond the documented surface (e.g. a leftover dev verb).
	for verb := range root.Subcommands {
		found := false
		for _, w := range want {
			if w == verb {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("undocumented `oat assistant %s` subcommand present", verb)
		}
	}
}

// captureStdout runs fn while redirecting os.Stdout to a pipe, then
// returns everything fn wrote. Used by the JSON-output tests so they
// can assert on the actual byte stream produced by emitAssistantJSON.
// The Part 6a NM RPC handlers will parse the same byte stream, so an
// assertion on the test side is the same contract as the production
// caller.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()

	fn()
	_ = w.Close()
	<-done
	return buf.String()
}

// TestAssistantStatusJSON_Schema_Part6Slice1 pins the wire shape
// emitted by `oat assistant status --json`. Part 6a's NM RPC handler
// (`oat_assistant_status` in oat-browser-agent's nm-broker.ts) JSON-
// parses this exact byte stream — a silent rename of any field, or a
// shift in the State enum vocabulary, breaks the side-panel control
// area without surfacing as a Go-side test failure. So pin both:
//
//  1. JSON keys (snake_case, exact spellings).
//  2. State enum strings (matches the docstring on AssistantStatusJSON).
func TestAssistantStatusJSON_Schema_Part6Slice1(t *testing.T) {
	t.Run("full populated row round-trips byte-stable", func(t *testing.T) {
		in := AssistantStatusJSON{
			Name:                  "work",
			RepoKey:               "_assistant-work",
			AgentName:             "work",
			State:                 "running",
			PID:                   12345,
			Model:                 "anthropic:claude-sonnet-4-6",
			ModelSwappedOnRestart: true,
			ModelSwapReason:       "model unavailable on this host",
		}
		got := captureStdout(t, func() {
			if err := emitAssistantJSON(in); err != nil {
				t.Fatalf("emitAssistantJSON: %v", err)
			}
		})
		var back AssistantStatusJSON
		if err := json.Unmarshal([]byte(got), &back); err != nil {
			t.Fatalf("emitted JSON did not round-trip: %v\n%s", err, got)
		}
		if back != in {
			t.Errorf("round-trip mismatch:\n  in:  %#v\n  out: %#v", in, back)
		}
		// Snake_case key spot-check on the literal bytes — Go's
		// default Marshal would have emitted "Name", "RepoKey", etc.
		// without struct tags. The NM RPC handler greps for these
		// keys so a typo here would silently nullify a field.
		for _, key := range []string{
			`"name"`, `"repo_key"`, `"agent_name"`, `"state"`,
			`"pid"`, `"model"`, `"model_swapped_on_restart"`,
			`"model_swap_reason"`,
		} {
			if !strings.Contains(got, key) {
				t.Errorf("expected key %s in JSON, missing from:\n%s", key, got)
			}
		}
	})

	t.Run("swap_reason is omitempty when not set", func(t *testing.T) {
		in := AssistantStatusJSON{
			Name:      "personal",
			RepoKey:   "_assistant-personal",
			AgentName: "personal",
			State:     "stopped",
		}
		got := captureStdout(t, func() {
			if err := emitAssistantJSON(in); err != nil {
				t.Fatalf("emitAssistantJSON: %v", err)
			}
		})
		if strings.Contains(got, "model_swap_reason") {
			t.Errorf("model_swap_reason should be omitted when empty, got:\n%s", got)
		}
		// model (no omitempty) MUST still appear with empty string —
		// the side panel uses field presence to know whether to ask
		// the daemon for a default. If the key disappears, the side
		// panel can't distinguish "not set" from "key missing in
		// older daemon" — making "" explicit closes that gap.
		if !strings.Contains(got, `"model": ""`) {
			t.Errorf("expected explicit empty model, got:\n%s", got)
		}
	})

	t.Run("state enum vocabulary is exactly the documented set", func(t *testing.T) {
		// Documented in the AssistantStatusJSON docstring. If you add
		// a new state, also add it here AND update the side-panel
		// renderer in oat-browser-agent/extension/src/sidepanel.ts.
		want := map[string]bool{
			"no_repo":                true,
			"stopped":                true,
			"running":                true,
			"registered_not_running": true,
		}
		for state := range want {
			in := AssistantStatusJSON{State: state}
			data, _ := json.Marshal(in)
			if !strings.Contains(string(data), `"state":"`+state+`"`) {
				t.Errorf("state %q did not round-trip in marshal: %s", state, data)
			}
		}
	})
}

// TestAssistantListEntryJSON_Schema_Part6Slice1 pins the list-row
// shape. List has a wider state enum than status (adds "dead" and
// "registered") because list shows EVERY registered virtual repo
// including ones whose agent records are stale; status is always
// addressed at one specific assistant and degrades the same row to
// "registered_not_running" or "stopped" instead. Document this
// asymmetry here so a future refactor doesn't unify them naively.
func TestAssistantListEntryJSON_Schema_Part6Slice1(t *testing.T) {
	t.Run("populated row round-trips byte-stable", func(t *testing.T) {
		in := []AssistantListEntryJSON{
			{Name: "personal", State: "running", Model: "anthropic:claude-sonnet-4-6", PID: 99},
			{Name: "work", State: "stopped", Model: "", PID: 0},
			{Name: "crashed", State: "dead", Model: "anthropic:claude-opus-4-7", PID: 4242},
			{Name: "registered-only", State: "registered", Model: "", PID: 0},
		}
		got := captureStdout(t, func() {
			if err := emitAssistantJSON(in); err != nil {
				t.Fatalf("emitAssistantJSON: %v", err)
			}
		})
		var back []AssistantListEntryJSON
		if err := json.Unmarshal([]byte(got), &back); err != nil {
			t.Fatalf("emitted JSON did not round-trip: %v\n%s", err, got)
		}
		if len(back) != len(in) {
			t.Fatalf("length mismatch: got %d, want %d", len(back), len(in))
		}
		for i := range in {
			if back[i] != in[i] {
				t.Errorf("row %d mismatch:\n  in:  %#v\n  out: %#v", i, in[i], back[i])
			}
		}
		for _, key := range []string{`"name"`, `"state"`, `"model"`, `"pid"`} {
			if !strings.Contains(got, key) {
				t.Errorf("expected key %s in JSON, missing from:\n%s", key, got)
			}
		}
	})

	t.Run("empty list emits literal []", func(t *testing.T) {
		// Part 6a NM RPC handler does `length === 0` on the parsed
		// result; if we emitted `null` (the Go-default for nil slice
		// without preallocation) the handler would crash. Pin the
		// empty-slice path so the assistantList preallocation
		// `make([]AssistantListEntryJSON, 0, len(virt))` stays
		// preallocated.
		var empty []AssistantListEntryJSON = make([]AssistantListEntryJSON, 0)
		got := captureStdout(t, func() {
			if err := emitAssistantJSON(empty); err != nil {
				t.Fatalf("emitAssistantJSON: %v", err)
			}
		})
		trimmed := strings.TrimSpace(got)
		if trimmed != "[]" {
			t.Errorf("expected literal '[]' for empty list, got %q", trimmed)
		}
	})

	t.Run("list state enum vocabulary is exactly the documented set", func(t *testing.T) {
		// Wider than status — see docstring on AssistantListEntryJSON.
		want := []string{"running", "stopped", "dead", "registered"}
		for _, state := range want {
			in := AssistantListEntryJSON{Name: "x", State: state}
			data, _ := json.Marshal(in)
			if !strings.Contains(string(data), `"state":"`+state+`"`) {
				t.Errorf("state %q did not round-trip in marshal: %s", state, data)
			}
		}
	})
}

// TestEmitAssistantJSON_Indentation_Part6Slice1 pins the indentation
// convention (2 spaces, trailing newline) so the Part 6a tests in
// oat-browser-agent that snapshot CLI stdout don't drift if a future
// refactor swaps to `json.Marshal` (no indent) or to 4 spaces.
func TestEmitAssistantJSON_Indentation_Part6Slice1(t *testing.T) {
	in := AssistantStatusJSON{Name: "x", State: "stopped"}
	got := captureStdout(t, func() {
		if err := emitAssistantJSON(in); err != nil {
			t.Fatalf("emitAssistantJSON: %v", err)
		}
	})
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("expected trailing newline, got %q", got)
	}
	// 2-space indentation marker — any nested key should be preceded
	// by exactly 2 spaces (not 4, not tab). The literal "  \"" check
	// catches accidental tab/4-space drift.
	if !strings.Contains(got, "  \"name\"") {
		t.Errorf("expected 2-space indent before \"name\", got:\n%s", got)
	}
}
