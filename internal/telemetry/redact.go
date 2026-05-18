package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
)

// HashAndLen returns a deterministic fingerprint plus the input length. Used in
// place of raw user input so traces show shape without leaking content.
func HashAndLen(s string) (hash string, length int) {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8]), len(s) // 8 bytes = 16 hex chars, plenty for grouping
}

// Redacted reports the length and a short fingerprint of v as a printable
// token. Used when we want to ship "something was here" without the content.
func Redacted(s string) string {
	if s == "" {
		return "<empty>"
	}
	h, n := HashAndLen(s)
	return fmt.Sprintf("<redacted:len=%d,fp=%s>", n, h)
}

// secretPatterns catches the common shapes of tokens that have shown up in
// previous incidents (Aikido scans + the Anthropic-key autofix PRs in this
// repo's history). Conservative — false positives are cheap; false negatives
// in telemetry would be a real leak.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),                // Anthropic / OpenAI
	regexp.MustCompile(`pk-lf-[A-Za-z0-9_-]{20,}`),             // Langfuse public key
	regexp.MustCompile(`sk-lf-[A-Za-z0-9_-]{20,}`),             // Langfuse secret key
	regexp.MustCompile(`ghp_[A-Za-z0-9]{30,}`),                 // GitHub PAT
	regexp.MustCompile(`gho_[A-Za-z0-9]{30,}`),                 // GitHub OAuth
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                     // AWS access key
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), // JWT
}

// Scrub returns s with any known secret pattern replaced by a redaction token.
// Idempotent; safe to apply to already-scrubbed strings.
func Scrub(s string) string {
	for _, p := range secretPatterns {
		s = p.ReplaceAllStringFunc(s, func(m string) string {
			h, n := HashAndLen(m)
			return fmt.Sprintf("<secret:len=%d,fp=%s>", n, h)
		})
	}
	return s
}
