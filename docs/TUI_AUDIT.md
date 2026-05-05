# UI Cleanup Report — TUI Layer (`internal/tui/`)

> **Status:** Internal refactor backlog. Items are triaged but not all resolved.
> For shipped behavior, see [CHANGELOG.md](../CHANGELOG.md).
> This file is kept as a working checklist for contributors — not user-facing docs.

Generated: 2026-03-31 | Last reviewed: 2026-04-24
Scope: all files under `internal/tui/` — `app.go`, `activity.go`,
`dedup.go`, `filter.go`, `render.go`, `stream.go`, `socket_stream.go`,
`styles.go`, `keys.go`, plus associated `*_test.go` and `*_bench_test.go` files.

---

## Quick Wins (< 30 min each)

### QW-1: Dead fields — `Stuck` and `outputDir` are allocated but never used

`AgentInfo.Stuck` (line 40 of `app.go`) is declared but never set and never read
in any render or logic path. The daemon response parser does not populate it, and
no UI element consumes it. Similarly, `App.outputDir` (line 54) is declared as a
struct field but is never written after construction — `pollDaemon` captures
`a.paths.OutputDir` into a local instead of reading the field. Dead fields inflate
the struct's memory footprint and mislead future contributors into thinking that
stuck-detection is implemented.

Fix: delete `Stuck bool` from `AgentInfo` and delete `outputDir string` from `App`.

### QW-2: `inputMode` is permanently `true` — the field is vestigial

`inputMode` is initialised to `true` in `NewApp` and is never set to `false`
anywhere in the codebase. Every guard that reads it (`if a.inputMode && ...`) is
therefore a tautology. The field name implies a two-state toggle that was never
wired up.

Fix: delete the field and inline `true` where the guards currently live, or
replace the guards with the actual condition they are proxying (checking
`a.mode != ViewAgentList`). Either removes a confusing invariant-that-is-not.

### QW-3: `agentIndex` zero-value silently means "first agent is selected"

`agentIndex` is an `int` that starts at `0` (Go zero value), so before the first
poll result arrives, it already points at a non-existent agents[0]. The code
handles this by checking `a.agentIndex >= 0` but the value is never initialised to
`-1` in `NewApp`. The sentinel value `-1` is used in `pollResultMsg` handling but
not at startup. This means if the user presses Enter before agents arrive, the
index check at line 527 (`if a.agentIndex >= 0 && a.agentIndex < len(a.agents)`)
is vacuously true for index 0 only when agents is empty — accidentally safe, but
semantically wrong and fragile.

Fix: add `agentIndex: -1` to the struct literal in `NewApp` to make the
"nothing selected" state explicit, matching the rest of the code's intent.

### QW-4: Hardcoded `30` passed to `renderAgentList` in `View()`

Line 635 passes a literal `30` for the agent list width. The layout budget
constant `agentListWidth = 30` is defined in `recalcLayout` (line 1025) but is a
local constant, not accessible from `View()`. The two values must stay in sync
manually. If someone changes the constant in `recalcLayout`, the render call will
silently produce a width-mismatched panel.

Fix: promote `agentListWidth` to a package-level constant so both sites share the
same value.

### QW-5: `renderAgentList` counts height by scanning the full string

Line 855 calls `strings.Count(b.String(), "\n")` to determine how many lines have
been rendered, then pads the rest. This forces an O(n) scan over a string that was
just built — and `b.String()` allocates a new string copy just for counting. The
renderer already knows how many agents are present and how many lines each
occupies (two lines of name+type plus a divider = 3 lines per agent for all but
the last). Replace the string scan with a simple integer counter that increments
as each line is written.

### QW-6: Duplicate color literal `#A78BFA` in `render.go` and `styles.go`

`styleCode` in `render.go` (line 39) is hardcoded to `lipgloss.Color("#A78BFA")`,
the same value as `colorBrand` in `styles.go` (line 15). If the brand color
changes, render.go will silently diverge. `render.go` should reference `colorBrand`
directly.

### QW-7: Duplicate color literal `#06B6D4` in `render.go` and `activity.go`

`styleToolCallLine` in `render.go` (line 13) and `agentColorPalette[0]` in
`activity.go` (line 23) both hardcode `#06B6D4`. `styleBullet` in `render.go`
(line 52) also hardcodes this value. All three should reference `colorSecondary`
from `styles.go`.

---

## Medium Effort (1–4 hours)

### ME-1: `app.go` is a 1633-line god file — split along natural seams

The file contains at least five distinct concerns that each warrant their own file:

1. **Bubbletea lifecycle** (`Init`, `Update`, `View`, `handleKey`) — the actual
   model implementation, currently ~600 lines.
2. **Panel renderers** (`renderStatusBar`, `renderAgentList`, `renderTokenBar`,
   `renderInputBar`, `renderHelp`) — all pure string-building functions, ~250 lines.
3. **Agent management** (`syncStreams`, `rebuildStreams`, `switchToAgent`,
   `sortAgents`, `agentTypePriority`, `isPrimaryAgent`) — ~150 lines.
4. **Async commands** (`pollDaemon`, `tick`, `streamTick`, `readStream`,
   `sendInput`, `interruptAgent`) — ~150 lines.
5. **Helper utilities** (`getString`, `getBool`, `getInt64`, `getInt`,
   `scrapeTokensFromLog`, `parseTokenString`, `formatTokenCount`, `estimateCost`,
   `thinkingIndicator`) — ~120 lines.

The helpers in category 5 have no dependency on the `App` struct (most are pure
functions); they could move to `helpers.go`. The panel renderers in category 2 are
all methods on `App` but depend only on read-only state — they could move to
`panels.go`. This split would reduce `app.go` to ~650 lines of pure control flow,
making it dramatically easier to reason about.

### ME-2: Multiple linear scans over `a.agents` per frame — add a lookup map

The `View()` call chain executes at least 6 separate `for _, ag := range a.agents`
loops in a single frame: one in `renderStatusBar` (to find the active agent's
type), one in `renderTokenBar` (to aggregate tokens), one in `renderAgentList`
(to render all agents), one in `thinkingIndicator` (to check if agent is alive),
one in `switchToAgent` (to find the agent's type for the placeholder), and one in
`modeForAgent`. For a workspace with 10–20 agents this is negligible, but the
pattern also means the active agent's `AgentInfo` is re-fetched from the slice on
every keypress and on every 50ms stream tick.

Fix: maintain a `agentsByName map[string]*AgentInfo` field that is updated
alongside `a.agents` in `pollResultMsg` handling. All single-agent lookups become
O(1) map accesses. The linear scans in the render path are already acceptable
since they visit all agents anyway.

### ME-3: `renderActivityLog` allocates three `lipgloss.NewStyle()` objects per call, including one per agent row

`renderActivityLog` in `activity.go` creates a fresh `lipgloss.NewStyle()` for
the title (line 218), a fresh one for the divider (line 226), and a fresh
`lipgloss.NewStyle().Foreground(agentColor).Bold(true)` per visible entry (line
278). This function is called every time the viewport renders when the activity
panel is visible. The title and divider styles never change — they should be
package-level variables in `styles.go`. The per-entry name style is
color-parameterised and cannot be fully pre-built, but the base `.Bold(true)` can
be separated from the `.Foreground()` call to reduce allocation.

The same issue exists in `View()` at lines 626–645 where `lipgloss.NewStyle()` is
called to build the divider and to wrap the viewport and activity views on every
render frame.

### ME-4: Double deduplication — `stream.go` and `DeduplicateAppend` do the same job

`LogStream.run()` implements its own progressive extension logic (lines 169–197 of
`stream.go`) using a `pendingLine`/`pendingTrimmed` state machine. The batch then
passes through `DeduplicateAppend` in `app.go` (line 318), which also implements
progressive extension with a 30-line lookback window.

The result is that progressive streaming fragments are deduped twice: once in the
goroutine and once in the Update handler. In practice the second pass is mostly a
no-op, but it means two complex dedup paths exist that must both be kept
consistent. If they diverge (different boundary-check logic, different prefix
minimums), they produce different results.

The `SocketStream` does not have stream-side dedup — it relies entirely on
`DeduplicateAppend`. This asymmetry means the socket and file paths have different
behaviour for the same content.

Fix: remove the progressive extension state machine from `LogStream.run()` and
rely solely on `DeduplicateAppend`. The blank-deferral logic (lines 154–166 of
`stream.go`) can be kept because it serves a different purpose (suppressing TUI
redraw artifact blanks at the stream level before they pollute the dedup window).
This eliminates ~30 lines of duplicated logic and gives both stream types identical
dedup behaviour.

### ME-5: `estimateCost` model pricing table is hardcoded in `app.go`

The `estimateCost` function at line 939 contains a hardcoded map of
model-to-pricing that is embedded inside the function body. Model names and their
prices change frequently. The table is not tested. It currently references models
from mid-2025 through early-2026, mixing Anthropic, OpenAI, and Google pricing
in an informal string-keyed map with no validation.

This is a maintainability trap: prices go stale, new models are added to the
system but not to the table, and the fallback rate of `$3/$10` will significantly
over-estimate costs for cheap models like `gemini-2.5-flash`. The function also
has a logic gap: when `totalAll > 0` but `totalIn == 0 && totalOut == 0` (a
common state before token data is populated), `renderTokenBar` sets `totalIn =
totalAll` at line 881 and then calls `estimateCost` with `inTok=totalAll,
outTok=0` — this uses the blended rate path regardless of model, producing a
rough estimate rather than the model-accurate calculation the code appears to
intend.

Fix: move the pricing table to a config file or a separate `pricing.go` with a
small test, and add a dedicated test for the token-data-unavailable code path.

### ME-6: `renderTokenBar` builds its content then discards parts in a loop

Lines 930–934 build `content`, then loop calling `lipgloss.Width(content)` (which
scans ANSI escape sequences) until the content fits. Each iteration of the
drop-last-part loop rebuilds the `strings.Join` and re-scans the width. This is
at most 3–4 iterations, but `lipgloss.Width` on a styled string is not trivial.
Pre-compute the width of each individual `part` when building the slice, then
greedily include parts until the budget is exhausted instead of building-then-
truncating.

---

## Refactors (schedule these)

### RF-1: State machine for view modes is tangled — three overlapping booleans

The TUI's current mode is expressed through three booleans that overlap:
`showAgentList`, `expandedView`, and `readOnly`, plus the `ViewMode` enum
(`ViewWorkspace`, `ViewAgent`, `ViewAgentList`). These are not independent: when
`showAgentList` is true, the mode is forced to `ViewAgentList`; when
`expandedView` is true, `showAgentList` is forced to false. This means any
transition must update multiple fields atomically, and there are already subtle
bugs: `TogglePanel` sets `mode = ViewAgentList` and `input.Blur()`, but
`ExpandView` sets `showAgentList = true` and `mode = ViewAgentList` while also
calling `input.Focus()` — the two paths contradict each other on input focus state
(lines 401–413 vs 466–479).

The `layoutShowAgentList` and `layoutShowActivity` computed fields add a second
layer: the visual layout can suppress panels that `showAgentList` says should be
visible, based on the terminal budget. `View()` reads `layoutShowAgentList` but
`handleKey` reads `showAgentList`, so keys respond to the user's *intent* while
rendering reflects *actual* layout — a subtle distinction that is not documented.

Fix: collapse `showAgentList` and `expandedView` into a single `panelState` enum
(`PanelHidden`, `PanelAgentList`, `PanelExpanded`) and make `ViewMode` the sole
authority for input routing. Remove `inputMode` (see QW-2). The layout budget
system should remain separate but only affect rendering, never state transitions.

### RF-2: `renderAgentList` mixes data formatting and string building

The `renderAgentList` method (lines 775–861) builds a formatted string by
interleaving label construction, style selection, truncation, and newline emission
all in one 85-line loop. The name-truncation at lines 798–801, the typeLine
composition at lines 804–822, and the cursor/active/normal style selection at
lines 827–845 are all interleaved with `b.WriteString` calls.

This makes it impossible to test the data-formatting logic (which agent gets which
label?) independently from the rendering logic (which style gets applied?). It
also means the padding calculation at lines 854–858 must count newlines in the
accumulated string rather than tracking a counter.

Fix: introduce an `agentListEntry` struct that captures the pre-formatted fields
(label, typeLine, style selection), build a slice of these in a first pass, then
render them in a second pass. The padding calculation becomes `height -
len(entries)*linesPerEntry`.

### RF-3: Token data pipeline is fragile — three paths, two fallbacks, no clear authority

Token data flows through three independent mechanisms:
1. The daemon reports `input_tokens`/`output_tokens`/`total_tokens` in the
   `list_agents` response (polled every 2s).
2. The filter classifies `[OAT_TOKENS]` lines as `CatChrome` so they are hidden,
   but there is no code path that actually *parses* `[OAT_TOKENS]` lines from the
   stream and updates token counts. The `observedTokens` map on `App` is
   allocated (line 74, 138) but never written to anywhere in the codebase.
3. `scrapeTokensFromLog` reads the last 2KB of the log file and regex-matches
   "N.NK tokens" lines emitted by the agent's own TUI chrome.

Paths 1 and 3 are active; path 2 (`observedTokens`) is dead code. Path 3 is a
polling fallback that re-opens and re-reads 2KB of the log file on every 2-second
daemon poll for every agent that has no daemon-reported tokens. With 10 agents
this is 10 file opens + 10 reads per poll cycle.

The result is that `renderTokenBar` shows token data that is 2 seconds stale
(daemon poll interval), or estimates scraped from TUI chrome that may be much
older, and both sources can coexist in the same render (per-agent display uses
daemon data; cost estimate uses the scrape fallback).

Fix:
- Delete `observedTokens` (dead code, QW-1 style fix).
- If `[OAT_TOKENS]` structured lines are a planned feature, add a parser in
  `streamBatchMsg` handling that writes to a token counter and document the wire
  format.
- Gate `scrapeTokensFromLog` behind a configurable flag or remove it entirely in
  favour of requiring agents to emit `[OAT_TOKENS]`. A file-scraping fallback in
  a hot poll loop is the wrong long-term architecture.

### RF-4: Overflow protection is inconsistent across render functions

Some render functions are carefully bounded; others are not:

- `renderActivityLog` uses `lipgloss.NewStyle().MaxHeight(height)` as a final
  guard (line 290) but *also* manually tracks `linesRendered` and pads to exactly
  `height` lines. The MaxHeight guard and the manual line counting are redundant.
  More importantly, MaxHeight clips content but does not prevent lipgloss from
  internally wrapping lines that exceed `width`, so a very long action string that
  is not pre-truncated could still produce two visual lines.

- `renderAgentList` uses `lipgloss.NewStyle().MaxHeight(height)` (line 860) but
  the typeLine truncation at lines 823–825 truncates to `width-5` characters.
  If `typeLine` contains multi-byte UTF-8 characters (agent names in non-Latin
  scripts are permitted), `len(typeLine)` returns byte length, not rune count,
  and the truncation will split a multibyte sequence.

- `renderStatusBar` uses `MaxHeight(1)` and `MaxWidth(w)` (line 772) — correct.

- The divider in `View()` (lines 626–632) is built with
  `strings.Repeat("│\n", vpHeight)` which produces exactly `vpHeight` newlines.
  If `vpHeight` is computed wrong (e.g. the chrome-line subtraction at line 1009
  is off by one after a window resize), the divider height will not match the
  viewport height, causing a visual misalignment between the agent list and the
  content pane.

Fix: audit all `len(string)` truncations and replace with `len([]rune(string))` or
use `lipgloss.Width()` for display-width-aware truncation. Consolidate the
`MaxHeight` guard and the manual line counter in `renderActivityLog` — keep only
one. Add a regression test for the divider height.

---

## Accessibility Violations

Terminal UIs have no DOM and no WCAG in the browser sense, but they have
equivalent principles. The following are ordered by severity.

### A11Y-1: No keyboard navigation between agents without entering the agent-list mode

The only way to switch to a different agent is to press Tab (enter agent-list
mode), navigate with arrow keys, then press Enter. There is no direct hotkey to
jump to agent N. Ctrl+N and Ctrl+P move through the list, but only when the mode
is already `ViewAgentList` — pressing Ctrl+N while typing in the input bar has no
effect (it is not bound to anything visible to the user).

The help bar (line 997–1003) does not show Ctrl+N/P because it lists only the
mode-relevant subset of bindings and omits these from all three help strings.
Users have no discoverability path to agent switching without reading documentation.

Fix: mention Ctrl+N/Ctrl+P in the `ViewWorkspace` and `ViewAgent` help strings, or
bind direct jump keys (e.g. Alt+1 through Alt+9 for the first 9 agents).

### A11Y-2: Thinking indicator uses braille spinner characters without a text fallback

The `thinkingIndicator` function (line 1495) renders braille characters
(`⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏`) as a spinner. In terminals that do not support Unicode
(Windows cmd.exe, some SSH connections, some screen readers), these render as
boxes or are omitted entirely. The indicator then reads as just
`"  processing... (Ns since last output)"` with no spinner glyph — acceptable —
but the code does not test for terminal capability.

More importantly, the braille spinner moves (frame changes every 200ms) but the
motion is driven by comparing elapsed time inside `thinkingIndicator()` which is
called from `renderContentForViewport` which is called from `updateViewport` which
is triggered by `streamTick`. The spinner only advances when the viewport
*content* is set, not when the viewport *renders*. If the viewport's bubbletea
model decides not to re-render (because the content string is identical), the
spinner freezes. The `streamTickMsg` handler at line 197–209 works around this by
calling `a.updateViewport()` every 4th tick when the indicator is visible — but
this is a fragile workaround that couples the tick rate to the spinner frame rate.

Fix: drive the spinner from a dedicated ticker with a known frame duration, and
use a visible ASCII fallback (`-\|/`) alongside the braille char for terminals
that cannot render it.

### A11Y-3: Status messages are ephemeral and have no persistence

`a.statusMsg` holds error and info messages (e.g., "Send failed: connection
refused", "Log file not found"). These are shown in the status bar only when they
fit in the remaining width after the left/right sections are laid out. On a
narrow terminal, the status message is silently truncated or dropped entirely
(line 740: `if maxMsg > 5`). There is no minimum visibility guarantee.

More critically, status messages are cleared on the next successful poll
(`a.statusMsg = ""` at line 233), so a send failure that appears during a 2-second
poll interval may only be visible for a fraction of that window before vanishing.
Users may miss errors entirely.

Fix: display status messages in the token bar row or in a dedicated
`statusMsg` row that appears only when there is a message. Clear the message on
the next keypress or after a minimum 5-second display duration, not on the next
poll.

### A11Y-4: Help line is truncated at `MaxWidth` without indication

`renderHelp` (line 996) renders a fixed help string with `MaxWidth(a.width)`.
On terminals narrower than the help string (~90 characters for the workspace mode
string), lipgloss hard-truncates the string with no ellipsis or wrapping. The
user sees a partial list of key bindings with no indication that more exist.

Fix: either use a shorter help string for narrow terminals (lipgloss.Width of each
mode's string is known at compile time), or wrap the help across two lines when
the terminal is narrow.

### A11Y-5: Agent list panel has no scroll indicator when agents exceed viewport

`renderAgentList` pads the list to `height` but does not indicate when there are
more agents below the visible area. With many agents (e.g., 20 workers), the list
silently clips at the bottom. The user has no way to know agents are hidden.

Fix: add a simple `▼ N more` indicator at the bottom of the panel when the agent
list overflows, similar to how the filter indicator `[f]` appears in the status bar.

---

## Pending Tokens Display Issue

The `"tokens pending"` string at line 926 is shown whenever `len(parts) == 0`,
which happens when all of `activeModel`, `activeIn`, `activeOut`, `totalAll`, and
`totalIn` are zero. This is the normal state for the first 2 seconds (before the
first poll completes) and remains the state for any agent that the daemon does not
report token data for.

The literal string `"tokens pending"` implies tokens are about to arrive, which
is accurate for fresh agents but misleading for stopped agents or agents the daemon
has no token data for (they will show "pending" forever). The scrape fallback
(`scrapeTokensFromLog`) runs inside `pollDaemon`, so if it finds data the display
will update on the next poll. But for agents using `CatChrome` to hide token
lines, the scrape may succeed but the value reported is the *last TUI chrome line*
seen before the log was closed, which could be stale by hours.

The root cause is RF-3: there is no canonical, authoritative token data source.
Until that is fixed, the immediate improvement is to change the placeholder to
`"no token data"` when the agent is not alive, and `"waiting for token data"` only
when the agent is alive, to avoid the misleading "pending" state for dead agents.

