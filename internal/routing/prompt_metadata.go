package routing

import (
	"crypto/sha256"
	"encoding/hex"
)

// PromptMetadata describes the prompt + tool surface the agent ran with.
// Captured at agent spawn time (system prompt, user message) plus optional
// after-spawn updates from the sidecar protocol (tool definitions, loaded
// skills). Hashes are full sha256 hex (64 chars) — short enough for log
// lines, long enough that collisions are not a practical concern.
//
// Privacy contract: hashes only, never the raw prompt content. The raw text
// for the user task lives in OutcomeRecord.TaskText (gated by privacy mode);
// the system prompt and tool definitions are big and not user-supplied, so
// we never store them — readers reconstruct from the binary version + the
// hash if needed.
//
// Token counts use a simple chars/4 heuristic. Imprecise vs an actual
// tokenizer (off by ~10-20% on English code-heavy text), but precise enough
// for "is this a 2k-token prompt or 200k?" bucketing and adds zero deps.
type PromptMetadata struct {
	SystemPromptHash     string   `json:"system_prompt_hash,omitempty"`
	SystemPromptTokens   int      `json:"system_prompt_tokens,omitempty"`
	UserMessageHash      string   `json:"user_message_hash,omitempty"`
	UserMessageTokens    int      `json:"user_message_tokens,omitempty"`
	ToolDefinitionsHash  string   `json:"tool_definitions_hash,omitempty"`
	ToolDefinitionsCount int      `json:"tool_definitions_count,omitempty"`
	InjectedSkills       []string `json:"injected_skills,omitempty"`
	ContextFilesCount    int      `json:"context_files_count,omitempty"`
}

// HashPromptText returns the full sha256 hex of the input. Empty input
// returns empty string so omitempty omits the JSON field entirely. The
// hash space is collision-free for our purposes (one record per spawn,
// at most ~10k spawns/year per heavy user → 10^-71 birthday probability).
func HashPromptText(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// EstimateTokens approximates token count from character count using the
// chars/4 heuristic. Standard rule of thumb for English (slightly off for
// code-heavy text where punctuation density rises; under-estimates by
// ~10% for typical code review prompts). Good enough for bucketing
// "small/medium/large prompts."
//
// Returns 0 for empty input. Never negative.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	n := len(s) / 4
	if n < 1 {
		return 1 // any non-empty content is at least 1 token
	}
	return n
}
