package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const dedupLookback = 30       // how many recent lines to scan for progressive extension
const exactDedupLookback = 200 // larger window for exact duplicate checks (no false-positive risk)
const staleLookback = 5        // stale fragments only checked in very recent lines

// dedupPrefixMin is the minimum length a line must be for prefix-based
// dedup. With word-boundary checking, 3 is safe: "the" won't falsely match
// "therapy" (boundary check rejects it), but "Let" will correctly match
// "Let me check..." (space boundary). Without word boundary, 8+ was needed
// to avoid "Yes" → "Yesterday" style false positives.
const dedupPrefixMin = 3

// dedupCategory returns a coarse content category for a trimmed line.
// Progressive extension and stale fragment checks are restricted to lines
// in the same category — a tool call should never be "extended" by agent
// text, and user input should never suppress tool output.
func dedupCategory(trimmed string) int {
	if strings.HasPrefix(trimmed, "(*) ") || strings.HasPrefix(trimmed, "⏺ ") || strings.HasPrefix(trimmed, "● ") {
		return 1 // tool call
	}
	if strings.HasPrefix(trimmed, "⎿") || strings.HasPrefix(trimmed, "[Command ") ||
		strings.HasPrefix(trimmed, "  | ") || strings.HasPrefix(trimmed, "  ▎ ") ||
		strings.HasPrefix(trimmed, "Exit code:") {
		return 2 // tool output
	}
	if strings.HasPrefix(trimmed, "> ") {
		return 3 // user input
	}
	return 0 // agent text
}

// DeduplicateAppend adds new lines to an existing buffer, suppressing exact
// duplicates and progressive streaming fragments from recent lines.
// Returns the updated buffer and the index of the earliest replaced line
// (-1 if no in-place replacements were made). The caller can use the index
// to do surgical render cache invalidation.
func DeduplicateAppend(existing []string, newLines []string) ([]string, int) {
	result := existing
	earliestReplaced := -1
	for _, line := range newLines {
		var replacedIdx int
		result, replacedIdx = appendDeduped(result, line)
		if replacedIdx >= 0 && (earliestReplaced < 0 || replacedIdx < earliestReplaced) {
			earliestReplaced = replacedIdx
		}
	}
	return result, earliestReplaced
}

// DeduplicateAppendTyped is like DeduplicateAppend but also maintains a parallel
// types slice. Each entry in types corresponds to the same index in the lines buffer.
// When dedup replaces a line, the type is updated. When a line is suppressed, the
// type is not added. When a line is appended, the type is appended.
func DeduplicateAppendTyped(existing []string, existingTypes []string, newLines []string, newTypes []string) ([]string, []string, int) {
	result := existing
	types := existingTypes
	// Ensure types has the same length as result
	for len(types) < len(result) {
		types = append(types, "")
	}
	earliestReplaced := -1
	for i, line := range newLines {
		prevLen := len(result)
		var replacedIdx int
		result, replacedIdx = appendDeduped(result, line)
		lt := ""
		if i < len(newTypes) {
			lt = newTypes[i]
		}
		if replacedIdx >= 0 {
			// In-place replacement — update the type at that index
			types[replacedIdx] = lt
			if earliestReplaced < 0 || replacedIdx < earliestReplaced {
				earliestReplaced = replacedIdx
			}
		} else if len(result) > prevLen {
			// Line was appended
			types = append(types, lt)
		}
		// else: line was suppressed, nothing to do
	}
	return result, types, earliestReplaced
}

// appendDeduped adds a single line to the buffer with deduplication.
// Returns the updated buffer and the index of the replaced line (-1 if none).
func appendDeduped(buf []string, line string) ([]string, int) {
	trimNew := strings.TrimSpace(line)

	// Blank lines: collapse runs of 2+ consecutive blanks (at most 1 blank in a row).
	// Agent TUI redraws produce many blank lines from cursor repositioning —
	// allowing 2+ creates a sparse, double-spaced appearance.
	if trimNew == "" {
		if n := len(buf); n >= 1 {
			prev := strings.TrimSpace(buf[n-1])
			// Already have a blank — collapse
			if prev == "" {
				return buf, -1
			}
			// Suppress blanks after tool output indicators (▎, ⎿, |).
			// The agent TUI inserts blank rows between every context line,
			// creating an ugly double-spaced look in tool output blocks.
			if isToolOutputPrefix(prev) {
				return buf, -1
			}
		}
		return append(buf, line), -1
	}

	// --- Exact duplicate scan: use the larger window (no false-positive risk) ---
	exactStart := len(buf) - exactDedupLookback
	if exactStart < 0 {
		exactStart = 0
	}
	for i := len(buf) - 1; i >= exactStart; i-- {
		if strings.TrimSpace(buf[i]) == trimNew {
			return buf, -1
		}
	}

	// --- Progressive extension + stale fragment scan: smaller window ---
	// We scan the window and pick the best match rather than returning
	// on the first hit. Cost: 30 string comparisons worst case (~100ns).
	start := len(buf) - dedupLookback
	if start < 0 {
		start = 0
	}
	bestExtendIdx := -1 // index of the best progressive extension candidate
	bestExtendLen := 0  // length of the best match (longer = better)

	// Strip inline markdown for comparison — streaming fragments may have
	// backticks/bold markers at different positions than the final text.
	strippedNew := stripInlineMarkdown(trimNew)
	catNew := dedupCategory(trimNew)

	for i := len(buf) - 1; i >= start; i-- {
		existing := strings.TrimSpace(buf[i])
		if existing == "" {
			continue
		}

		// Skip cross-category comparisons: a tool call line should never be
		// progressive-extended by agent text, and vice versa.
		catExisting := dedupCategory(existing)
		if catNew != catExisting {
			continue
		}

		strippedExisting := stripInlineMarkdown(existing)

		// Progressive extension: new line starts with an existing shorter line.
		// Track the longest (best) match in the window.
		// Check both raw and stripped versions — raw catches exact prefix matches
		// (cheaper), stripped catches markdown-shifted matches.
		if len(existing) >= dedupPrefixMin {
			isPrefix := strings.HasPrefix(trimNew, existing)
			if !isPrefix && strippedExisting != existing {
				// Raw prefix failed but there's markdown — try stripped.
				// Require the new line to be strictly longer (stripped) to
				// avoid false positives where only markdown formatting differs
				// (e.g., "**Hello** world" vs "Hello world").
				isPrefix = len(strippedExisting) >= dedupPrefixMin &&
					len(strippedNew) > len(strippedExisting) &&
					strings.HasPrefix(strippedNew, strippedExisting)
			}
			if isPrefix {
				// Use stripped length for word boundary check
				checkLen := len(existing)
				checkStr := trimNew
				if !strings.HasPrefix(trimNew, existing) {
					checkLen = len(strippedExisting)
					checkStr = strippedNew
				}
				if isWordBoundary(checkStr, checkLen) && len(existing) > bestExtendLen {
					bestExtendIdx = i
					bestExtendLen = len(existing)
				}
			}
		}

		// Stale shorter fragment: new line is a prefix of an existing longer line.
		// Only check within the last few lines — distant matches are likely
		// coincidental and would eat legitimate new content.
		distFromEnd := len(buf) - 1 - i
		if distFromEnd < staleLookback && len(trimNew) >= dedupPrefixMin {
			isStale := strings.HasPrefix(existing, trimNew) && isWordBoundary(existing, len(trimNew))
			if !isStale && strippedNew != trimNew {
				isStale = strings.HasPrefix(strippedExisting, strippedNew) &&
					isWordBoundary(strippedExisting, len(strippedNew))
			}
			if isStale {
				return buf, -1
			}
		}
	}

	// Apply the best progressive extension match (if any)
	if bestExtendIdx >= 0 {
		buf[bestExtendIdx] = line
		return buf, bestExtendIdx
	}

	return append(buf, line), -1
}

// stripInlineMarkdown removes inline markdown formatting characters (backticks,
// bold/italic markers, strikethrough) from a string. Used for dedup comparison
// so that progressive streaming fragments with shifting markdown positions
// (e.g., "start `" → "start BotScheduler") are correctly identified as extensions.
// The original line is preserved for display — only comparison uses stripped text.
func stripInlineMarkdown(s string) string {
	// Fast path: no markdown chars present
	if !strings.ContainsAny(s, "`*_~") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '`':
			// skip backticks
		case '*', '_':
			// skip bold/italic markers, but only consecutive pairs or singles
			// that are clearly formatting (adjacent to same char)
		case '~':
			// skip strikethrough markers (~~ pairs)
			if i+1 < len(s) && s[i+1] == '~' {
				i++ // skip the pair
				continue
			}
			b.WriteByte(s[i])
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// isToolOutputPrefix returns true if the line looks like tool output content
// that shouldn't have blank lines inserted after it. This prevents the
// double-spaced appearance in ▎-prefixed context blocks.
func isToolOutputPrefix(trimmed string) bool {
	return strings.HasPrefix(trimmed, "▎") ||
		strings.HasPrefix(trimmed, "⎿") ||
		strings.HasPrefix(trimmed, "|") ||
		strings.HasPrefix(trimmed, "⏺ ") ||
		strings.HasPrefix(trimmed, "● ") ||
		strings.HasPrefix(trimmed, "(*) ") ||
		strings.HasPrefix(trimmed, "[Command ") ||
		strings.HasPrefix(trimmed, "Exit code:")
}

// isWordBoundary checks that a prefix match ends at a word boundary.
// Given that s[:prefixLen] matched some prefix, this verifies that the
// character at s[prefixLen] (the first character after the prefix) is
// a space, punctuation, or end-of-string — not a continuation of the
// same word.
//
// Examples:
//
//	"This is the information" with prefixLen=11 ("This is the")
//	  → s[11] = ' ' → true (space boundary)
//
//	"This is therapy" with prefixLen=11 ("This is the")
//	  → s[11] = 'r' → false (mid-word: "the" vs "therapy")
//
//	"I understand" with prefixLen=12
//	  → end of string → true
func isWordBoundary(s string, prefixLen int) bool {
	if prefixLen >= len(s) {
		return true // prefix is the entire string
	}

	// Also check the LAST character of the prefix: if the prefix ends with
	// a code/markdown delimiter (backtick, =, <, etc.), the next character
	// starts code content and is always a valid boundary regardless of what
	// it is. Example: "The `" → "The `cleanLogWriter`" — the backtick at
	// the end of the prefix means 'c' starts a code span, not a word.
	if prefixLen > 0 {
		last := s[prefixLen-1]
		if last == '`' || last == '=' || last == '(' || last == '[' ||
			last == '{' || last == '<' || last == '#' || last == '@' ||
			last == '~' || last == '|' || last == '/' || last == '\\' ||
			last == ' ' || last == '\t' || last == ',' || last == '.' ||
			last == ':' || last == ';' {
			return true
		}
	}

	b := s[prefixLen]
	// Fast path for ASCII (covers >99% of agent output).
	// Includes code/markdown delimiters (backtick, =, (, [, {, <, #, @, ~)
	// and digits — these appear at streaming fragment boundaries when the
	// LLM emits inline code, URLs, or variable assignments token-by-token.
	if b < 128 {
		return b == ' ' || b == '\t' || b == ',' || b == '.' ||
			b == ';' || b == ':' || b == '!' || b == '?' ||
			b == ')' || b == ']' || b == '}' || b == '"' ||
			b == '\'' || b == '-' || b == '/' || b == '\n' ||
			b == '`' || b == '=' || b == '(' || b == '[' ||
			b == '{' || b == '<' || b == '#' || b == '@' ||
			b == '~' || b == '|' || b == '\\' ||
			(b >= '0' && b <= '9')
	}
	// Non-ASCII byte at the boundary means a multi-byte rune starts here.
	// Decode it to check properly. CJK and other non-Latin scripts don't
	// use spaces between words, so any non-ASCII rune is a valid boundary.
	r, _ := utf8.DecodeRuneInString(s[prefixLen:])
	if r == utf8.RuneError {
		return true // malformed UTF-8, allow it
	}
	// CJK characters, emoji, etc. are self-delimiting — always a boundary.
	// Only reject if it's a Latin-script letter continuation (e.g. accented chars).
	return !unicode.IsLetter(r) || !unicode.Is(unicode.Latin, r)
}
