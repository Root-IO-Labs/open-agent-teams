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
