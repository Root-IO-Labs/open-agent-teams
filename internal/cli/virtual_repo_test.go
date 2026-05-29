// Tests for the Part 5c virtual-repo helpers. Focused tests because
// the helpers themselves are small; the heavy integration coverage
// (daemon round-trip of `add_repo is_virtual=true`, `list_repos`
// filtering) lives in the daemon test suite. Here we pin:
//
//   - Name validation: every documented-rejection case must reject;
//     every documented-acceptance case must accept. The validator is
//     the only thing standing between user input and a
//     filesystem-path component / state-key.
//   - Canonical name derivation: the `_assistant-<name>` prefix is
//     load-bearing across the daemon (handleAddRepo INFO log,
//     `oat repo list` mode column) and the CLI (`oat assistant`
//     verb tree). Pin it so a refactor that renames the prefix has
//     to update this test deliberately.

package cli

import (
	"strings"
	"testing"
)

func TestValidateVirtualRepoName_Part5c(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantErr   bool
		wantInErr string // substring expected in error, if any
	}{
		// Acceptance cases (the documented allowed shapes).
		{"plain alphanumeric", "personal", false, ""},
		{"with hyphen", "my-assistant", false, ""},
		{"with underscore", "work_helper", false, ""},
		{"mixed", "Personal-1_X", false, ""},
		{"single char", "a", false, ""},
		{"32 char max", strings.Repeat("a", 32), false, ""},
		{"all digits", "12345", false, ""},
		{"hyphen surrounded", "a-b-c", false, ""},

		// Rejection cases (documented in the plan body).
		{"empty", "", true, "required"},
		{"33 chars (just over max)", strings.Repeat("a", 33), true, "too long"},
		{"space", "my name", true, "invalid"},
		{"dot", "my.name", true, "invalid"},
		{"double dot path traversal", "..", true, "invalid"},
		{"single dot", ".", true, "invalid"},
		{"forward slash", "evil/name", true, "invalid"},
		{"back slash", `evil\name`, true, "invalid"},
		{"tilde", "~", true, "invalid"},
		{"newline", "hello\n", true, "invalid"},
		{"tab", "a\tb", true, "invalid"},
		{"null byte", "a\x00b", true, "invalid"},
		{"shell metachar pipe", "a|b", true, "invalid"},
		{"shell metachar dollar", "a$b", true, "invalid"},
		{"emoji", "personal😀", true, "invalid"},
		{"leading underscore is ALLOWED at validator level", "_internal", false, ""},
		// (The underscore prefix collides with our canonical
		// `_assistant-<name>` prefix at the SHAPE level — the
		// resulting state key would be `_assistant-_internal`,
		// which is valid. The validator deliberately doesn't
		// special-case this; the only collision risk is naming
		// a SECOND assistant `_assistant-personal` literally,
		// and that's the daemon's duplicate-name guard's job.)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateVirtualRepoName(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateVirtualRepoName(%q): err=%v, wantErr=%v", tc.input, err, tc.wantErr)
			}
			if tc.wantErr && tc.wantInErr != "" && !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("validateVirtualRepoName(%q): error %q does not contain %q", tc.input, err.Error(), tc.wantInErr)
			}
		})
	}
}

// TestVirtualRepoNameFor_Part5c pins the `_assistant-<name>`
// prefix. If this changes, every downstream code path that grep'd
// for the string (CLI listing UI, plan docs, daemon logs) needs to
// update with it.
func TestVirtualRepoNameFor_Part5c(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"personal", "_assistant-personal"},
		{"work", "_assistant-work"},
		{"a", "_assistant-a"},
		{strings.Repeat("x", 32), "_assistant-" + strings.Repeat("x", 32)},
		// The function doesn't validate -- it's pure prefixing.
		// Validation belongs to validateVirtualRepoName, which the
		// caller is expected to run first. We still pin the
		// pass-through shape so a future refactor doesn't add
		// "helpful" sanitization here and silently change the
		// state-key shape for existing users.
		{"unsanitized name with spaces", "_assistant-unsanitized name with spaces"},
		{"", "_assistant-"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			if got := virtualRepoNameFor(tc.input); got != tc.want {
				t.Errorf("virtualRepoNameFor(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestAssistantNameFromVirtualRepo pins the reverse direction
// used by the generic `oat agent remove` router to delegate
// assistant removals through the existing `oat assistant
// remove` flow. virtualRepoNameFor + assistantNameFromVirtualRepo
// must round-trip for every valid assistant name; non-virtual
// repo keys must return "" so the router falls through to its
// default branch.
func TestAssistantNameFromVirtualRepo(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"_assistant-personal", "personal"},
		{"_assistant-work", "work"},
		{"_assistant-a", "a"},
		{"_assistant-with-dashes", "with-dashes"},
		// A non-virtual repo (regular GitHub-style name) doesn't
		// start with the prefix → empty so the router knows to
		// fall through to the default daemon-remove path.
		{"my-real-repo", ""},
		{"oat-browser-agent", ""},
		// Empty + the bare prefix both round-trip back to ""
		// (the empty-name case would have been rejected by
		// validateVirtualRepoName upstream).
		{"", ""},
		{"_assistant-", ""},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := assistantNameFromVirtualRepo(tc.input)
			if got != tc.want {
				t.Errorf("assistantNameFromVirtualRepo(%q) = %q, want %q", tc.input, got, tc.want)
			}
			// Round-trip: virtualRepoNameFor(assistantNameFromVirtualRepo(x)) == x
			// when x is a non-empty virtual repo key.
			if tc.want != "" {
				if back := virtualRepoNameFor(got); back != tc.input {
					t.Errorf("round-trip failed: virtualRepoNameFor(%q) = %q, want %q",
						got, back, tc.input)
				}
			}
		})
	}
}

// TestFindAgentTypeInListing pins the daemon-response walk used
// by removeAgentGeneric to pick the right cleanup path. The
// shape comes from handleListAgents (a JSON array of
// {name, type, …} maps); this test guards the field-extraction
// against a daemon response-shape drift breaking the router
// silently.
func TestFindAgentTypeInListing(t *testing.T) {
	listing := []interface{}{
		map[string]interface{}{"name": "personal", "type": "assistant", "pid": 12345.0},
		map[string]interface{}{"name": "browser-agent", "type": "browser", "pid": 67890.0},
		map[string]interface{}{"name": "worker-swift-eagle", "type": "worker"},
	}
	cases := []struct {
		agent string
		want  string
	}{
		{"personal", "assistant"},
		{"browser-agent", "browser"},
		{"worker-swift-eagle", "worker"},
		// Unknown agent → empty (router turns this into a clear
		// "not found in repo" error rather than walking blindly
		// into the assistant cleanup path).
		{"does-not-exist", ""},
	}
	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			if got := findAgentTypeInListing(listing, tc.agent); got != tc.want {
				t.Errorf("findAgentTypeInListing(_, %q) = %q, want %q", tc.agent, got, tc.want)
			}
		})
	}

	// Defensive: a non-list payload returns "" without panic.
	// Older daemons or a swapped-in mock might return a
	// map-shape; the router should fall through cleanly.
	t.Run("non-list-payload", func(t *testing.T) {
		if got := findAgentTypeInListing("oops", "anything"); got != "" {
			t.Errorf("non-list payload should return empty; got %q", got)
		}
		if got := findAgentTypeInListing(nil, "anything"); got != "" {
			t.Errorf("nil payload should return empty; got %q", got)
		}
	})
}
