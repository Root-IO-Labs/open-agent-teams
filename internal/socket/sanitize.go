package socket

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// Maximum input length accepted by SanitizePTYInput, in bytes. Inputs
// longer than this are rejected outright. 32 KiB is well above any
// plausible interactive chat line (most LLM context windows surface as
// individual tokens long before this) and well below the level where
// blocking the daemon thread on a strings.Builder allocation matters.
const sanitizeMaxBytes = 32 * 1024

// Stripping more than this fraction of the input as *injection-class*
// C0 control bytes is treated as a likely attack rather than a typo.
// "Injection-class" excludes ESC (0x1B; routinely consumed as the
// prefix of a legitimate ANSI sequence paste) and CR (0x0D; routinely
// dropped during CRLF → LF normalization on Windows pastes).
// Everything else in 0x00–0x1F (NUL, BS, BEL outside OSC, etc.) is
// counted. Empirically: clean prose chats stay at 0%, an ANSI-heavy
// terminal paste stays at 0% because the ESC bytes don't count, and
// the Dropbox-2024 backspace-injection POC sits in the 15–25% range
// because every \x08 IS counted.
const sanitizeMaxStripRatio = 0.05

// Errors returned by SanitizePTYInput. Tests match against these
// sentinel values so the daemon can surface a meaningful socket
// response to the bridge without leaking the exact byte-level reason.
var (
	// ErrSanitizeOversized is returned when the input exceeds
	// sanitizeMaxBytes. The bridge logs this and surfaces a "Message
	// too large" toast in the side panel.
	ErrSanitizeOversized = errors.New("input exceeds 32 KiB limit")

	// ErrSanitizeTooMuchStripped is returned when more than
	// sanitizeMaxStripRatio of the input was control-or-escape
	// bytes. Treated as a likely prompt-injection attempt -- the
	// signal-to-noise ratio of legitimate chat is in the >99.9%
	// printable range, so a deluge of stripped bytes is the
	// Dropbox-2024 fingerprint.
	ErrSanitizeTooMuchStripped = errors.New("input contained too many control characters (possible injection)")

	// ErrSanitizeBadInterrupt is returned when SanitizeOpts.AllowInterrupt
	// is true but the input is not exactly the single byte 0x03. The
	// side panel's "Interrupt" button must send a clean Ctrl-C and
	// nothing else; padding the request with arbitrary other bytes
	// would let a malicious bridge sneak prompt text past the C0
	// filter under cover of the interrupt carve-out.
	ErrSanitizeBadInterrupt = errors.New("interrupt input must be exactly the single byte 0x03 (Ctrl-C)")

	// ErrSanitizeInvalidUTF8 is returned when, after stripping, the
	// remaining bytes are not valid UTF-8. The PTY itself doesn't
	// enforce this but downstream LLM tokenisers would surface
	// garbled mojibake; better to reject upfront.
	ErrSanitizeInvalidUTF8 = errors.New("input is not valid UTF-8 after sanitization")
)

// SanitizeOpts tunes SanitizePTYInput.
//
// In particular, AllowInterrupt opens a *single-byte carve-out* for
// 0x03 (Ctrl-C) so the side panel's 60-second-stall "Interrupt"
// button can deliver a real signal-equivalent to the agent's PTY
// (Part 2e). Outside that mode, 0x03 is stripped along with the rest
// of the C0 controls.
type SanitizeOpts struct {
	// AllowInterrupt switches the sanitizer into "interrupt mode":
	// the input must be exactly the single byte 0x03 (no leading,
	// trailing, or interspersed bytes), and the same byte is allowed
	// to survive sanitization. Any other input shape with this flag
	// set produces ErrSanitizeBadInterrupt.
	AllowInterrupt bool
}

// SanitizePTYInput filters untrusted text destined for an agent's PTY
// to mitigate control-character prompt injection.
//
// Background: Dropbox (2024) showed that LLM tokenisers can treat
// C0 control characters -- backspace (\x08) in particular -- as
// "delete the previous token" instructions, letting an attacker
// erase a portion of the system prompt right before the rest of
// their payload arrives. Subsequent OWASP guidance generalises
// this to "strip everything except an explicit allowlist of
// whitespace before any LLM input."
//
// We apply the same idea to the PTY-injection path because every
// byte the side panel forwards eventually surfaces in the agent's
// stdin buffer, which the agent process feeds verbatim into the
// model conversation.
//
// Rules (applied in order):
//
//  1. The input must be valid UTF-8 to begin with. (Invalid UTF-8
//     is a strong injection signal on its own.)
//  2. Strip all C0 controls (\x00–\x1F) EXCEPT \n (0x0A) and \t (0x09).
//     If opts.AllowInterrupt is true the single byte 0x03 is ALSO
//     allowed, but the input must consist of EXACTLY that one byte;
//     anything else with AllowInterrupt set yields ErrSanitizeBadInterrupt.
//  3. Strip all C1 controls (\x80–\x9F).
//  4. Strip ANSI escape sequences -- CSI (`ESC [ … final`), OSC
//     (`ESC ] … BEL|ST`), and bare ESC followed by a single
//     introducer/private byte.
//  5. Collapse `\r\n` to `\n`; drop bare `\r`.
//  6. Reject if the byte length exceeds sanitizeMaxBytes.
//  7. Reject if more than sanitizeMaxStripRatio of the input was
//     stripped -- this is the heuristic guard against
//     mostly-control-bytes payloads.
//  8. Final UTF-8 validity check (defence in depth against any rule
//     above silently producing a partial multi-byte sequence).
//
// The same function gates the side-panel chat input (Part 2b) AND
// the daemon-authored synthetic compaction message in Part 5e
// (`AllowInterrupt: false` in both -- the carve-out is reserved for
// the explicit interrupt button only).
func SanitizePTYInput(text string, opts SanitizeOpts) (string, error) {
	if len(text) > sanitizeMaxBytes {
		return "", ErrSanitizeOversized
	}
	if !utf8.ValidString(text) {
		return "", ErrSanitizeInvalidUTF8
	}

	// Interrupt mode is its own contract -- it has nothing to do
	// with the general stripping/threshold rules. Enforce upfront so
	// a malformed interrupt can't slip past as "5% stripped, looks OK".
	if opts.AllowInterrupt {
		if text == "\x03" {
			return "\x03", nil
		}
		return "", ErrSanitizeBadInterrupt
	}

	originalLen := len(text)
	var b strings.Builder
	b.Grow(originalLen)

	// We track "injection-class" stripped bytes separately from total
	// bytes consumed. See the sanitizeMaxStripRatio doc comment for
	// the rationale: ANSI ESC and CR-as-line-ending consumption is
	// expected, not an attack.
	injectionStripped := 0

	for i := 0; i < len(text); {
		c := text[i]

		switch {
		case c == 0x1b:
			// ANSI escape sequence. Consumed in full but NOT counted
			// against the injection threshold (pasting from a real
			// terminal routinely includes ANSI).
			i = skipANSIEscape(text, i)
		case c == '\r':
			// `\r\n` collapses to `\n`; bare `\r` disappears. Neither
			// counts as injection — Windows pastes show up clean.
			if i+1 < len(text) && text[i+1] == '\n' {
				b.WriteByte('\n')
				i += 2
			} else {
				i++
			}
		case c < 0x20:
			// C0 controls in the 0x00–0x1F range. Allow only \n (0x0A)
			// and \t (0x09). Everything else is counted as injection
			// — backspace, NUL, BEL outside an OSC, vertical tab, etc.
			// 0x03 (Ctrl-C) is included in this group because the
			// AllowInterrupt carve-out is handled above and never
			// reaches this code path.
			if c == '\n' || c == '\t' {
				b.WriteByte(c)
			} else {
				injectionStripped++
			}
			i++
		default:
			// Multi-byte (or single-byte printable) run. Decode the
			// full rune, then check whether it's a C1 control point
			// (U+0080–U+009F). C1 controls can sneak in via the
			// 2-byte UTF-8 form 0xC2 0x80 .. 0xC2 0x9F even when the
			// raw byte was 0xC2 (which is not in the C1 range). We
			// strip these and count them as injection — they were
			// never legitimate user input.
			r, size := utf8.DecodeRuneInString(text[i:])
			if r >= 0x80 && r <= 0x9f {
				injectionStripped++
				i += size
				continue
			}
			b.WriteString(text[i : i+size])
			i += size
		}
	}

	if originalLen > 0 && injectionStripped > 0 {
		// Integer cross-multiply (5 % == 5 / 100 ≡ 100*x > 5*N) keeps
		// the comparison exact on the boundary.
		if 100*injectionStripped > int(sanitizeMaxStripRatio*100)*originalLen {
			return "", ErrSanitizeTooMuchStripped
		}
	}
	out := b.String()
	if !utf8.ValidString(out) {
		return "", ErrSanitizeInvalidUTF8
	}
	return out, nil
}

// skipANSIEscape returns the index one past the end of the ANSI
// escape sequence starting at text[i] (text[i] must be 0x1b).
// Handles:
//   - CSI (ESC [ params final): skip until a byte in 0x40–0x7E
//   - OSC (ESC ] data terminator): skip until BEL or ST
//   - Single-byte escapes (ESC X for X in the introducer set): skip
//     ESC + one more byte
//   - Truncated escapes (ESC at end of input): skip the ESC alone
//
// The function is deliberately tolerant of malformed sequences so an
// attacker can't keep the parser spinning on a synthetic
// "ESC [ … forever" — there's no inner length budget beyond the
// global sanitizeMaxBytes gate.
func skipANSIEscape(text string, i int) int {
	if i+1 >= len(text) {
		// Bare trailing ESC: consume it alone.
		return i + 1
	}
	intro := text[i+1]
	j := i + 2
	switch intro {
	case '[':
		// CSI: skip params (0x30–0x3F) and intermediates (0x20–0x2F),
		// land one byte past the first final byte we see (0x40–0x7E).
		for j < len(text) {
			c := text[j]
			j++
			if c >= 0x40 && c <= 0x7e {
				return j
			}
		}
		return j
	case ']':
		// OSC: terminated by BEL (0x07) or ST (ESC '\').
		for j < len(text) {
			c := text[j]
			if c == 0x07 {
				return j + 1
			}
			if c == 0x1b && j+1 < len(text) && text[j+1] == '\\' {
				return j + 2
			}
			j++
		}
		return j
	default:
		// Single-byte escape (e.g. ESC = / ESC > / ESC c). Consume
		// ESC + one introducer byte.
		return j
	}
}
