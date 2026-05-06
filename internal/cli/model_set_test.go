package cli

import (
	"strings"
	"testing"
)

// TestUpdateRuntimeBlock_InsertWhenAbsent asserts that editing a profile
// that has no `runtime:` block yet appends one at the end, in the canonical
// key order (max_tokens first, then nudge_interval_seconds).
func TestUpdateRuntimeBlock_InsertWhenAbsent(t *testing.T) {
	in := `model_id: "foo:bar"
provider:
  name: foo
`
	out := updateRuntimeBlock(in, map[string]int{
		"max_tokens":             24000,
		"nudge_interval_seconds": 120,
	})

	if !strings.Contains(out, "\nruntime:\n  max_tokens: 24000\n  nudge_interval_seconds: 120\n") {
		t.Fatalf("expected runtime block appended with canonical ordering; got:\n%s", out)
	}
	// Original content must survive untouched.
	if !strings.Contains(out, "model_id: \"foo:bar\"") || !strings.Contains(out, "provider:\n  name: foo") {
		t.Fatalf("existing content mutated; got:\n%s", out)
	}
}

// TestUpdateRuntimeBlock_ReplaceInExisting asserts that keys inside an
// existing block are rewritten in place and untouched keys inside the
// block are preserved verbatim.
func TestUpdateRuntimeBlock_ReplaceInExisting(t *testing.T) {
	in := `model_id: "foo:bar"

runtime:
  max_tokens: 8000
  nudge_interval_seconds: 60
  future_flag: keep_me

provider:
  name: foo
`
	out := updateRuntimeBlock(in, map[string]int{"max_tokens": 32000})
	if !strings.Contains(out, "  max_tokens: 32000") {
		t.Fatalf("max_tokens should be updated to 32000; got:\n%s", out)
	}
	if !strings.Contains(out, "  nudge_interval_seconds: 60") {
		t.Fatalf("untouched nudge_interval_seconds dropped; got:\n%s", out)
	}
	if !strings.Contains(out, "  future_flag: keep_me") {
		t.Fatalf("unrelated key dropped; got:\n%s", out)
	}
	if strings.Contains(out, "max_tokens: 8000") {
		t.Fatalf("old value not replaced; got:\n%s", out)
	}
}

// TestUpdateRuntimeBlock_AppendMissingKey ensures that when the block
// exists but only has some of the keys, the missing one is appended
// inside the same block.
func TestUpdateRuntimeBlock_AppendMissingKey(t *testing.T) {
	in := `model_id: "foo:bar"

runtime:
  max_tokens: 8000

provider:
  name: foo
`
	out := updateRuntimeBlock(in, map[string]int{"nudge_interval_seconds": 300})
	if !strings.Contains(out, "  max_tokens: 8000") {
		t.Fatalf("existing key dropped; got:\n%s", out)
	}
	if !strings.Contains(out, "  nudge_interval_seconds: 300") {
		t.Fatalf("missing key not appended; got:\n%s", out)
	}
	// The appended entry must live inside the runtime: block, which
	// means it must come before the `provider:` line.
	runtimeIdx := strings.Index(out, "runtime:")
	providerIdx := strings.Index(out, "provider:")
	nudgeIdx := strings.Index(out, "nudge_interval_seconds: 300")
	if runtimeIdx < 0 || providerIdx < 0 || nudgeIdx < 0 {
		t.Fatalf("expected markers present in output; got:\n%s", out)
	}
	if !(runtimeIdx < nudgeIdx && nudgeIdx < providerIdx) {
		t.Fatalf("nudge_interval_seconds should be inside runtime block; got:\n%s", out)
	}
}

// TestUpdateRuntimeBlock_NoOpOnEmptyUpdates guards the early-return so
// future refactors don't silently rewrite the file on a no-op call.
func TestUpdateRuntimeBlock_NoOpOnEmptyUpdates(t *testing.T) {
	in := "model_id: foo\n"
	if got := updateRuntimeBlock(in, nil); got != in {
		t.Fatalf("expected identity on nil updates; got:\n%s", got)
	}
	if got := updateRuntimeBlock(in, map[string]int{}); got != in {
		t.Fatalf("expected identity on empty updates; got:\n%s", got)
	}
}
