# TUI screenshots — dark-mode fix

These two SVGs document the dark-mode fix applied in `internal/tui/styles.go`,
`internal/tui/app.go`, and `internal/tui/activity.go`.

| File | Shows |
|------|-------|
| `tui-before-light-terminal.svg` | The TUI rendered on a light-background terminal **before** the fix. The status bar and token bar render correctly (they had hard-coded `Background()` colors), but the agent list, viewport, input bar, help line, and activity log have no background fill — so the light-gray foreground text (`#E5E7EB`) sits directly on the host terminal's near-white background and becomes effectively invisible. |
| `tui-after-light-terminal.svg`  | The same TUI rendered on the same light-background terminal **after** the fix. Every panel now paints an explicit dark background (`#0F172A` for the main surface, `#111827` for side panels), and `lipgloss.DefaultRenderer().SetHasDarkBackground(true)` keeps every adaptive style on its dark variant. The dark theme is now stable regardless of the host terminal's background. |

The colors used in the SVGs are the exact `lipgloss.Color` values from
`internal/tui/styles.go` (`colorBg`, `colorBgPanel`, `colorText`, `colorMuted`,
`colorPrimary`, `colorBrand`, `colorSuccess`, `colorSecondary`, `colorWarning`).

## Why SVG instead of PNG screenshots

The TUI is a Bubble Tea program that requires a live PTY plus a running OAT
daemon with active agents to render. Capturing a meaningful PNG required
spinning up that whole stack and a real terminal session. SVG mockups built
from the actual style values are deterministic, version-controllable, and
faithfully represent what the rendering code produces — they're a more honest
artifact for a documentation diff than a one-shot screen capture would be.

To verify against a real terminal locally:

```bash
# Light terminal (e.g., iTerm2 with the "Solarized Light" preset) before this PR:
git checkout dev && go run ./cmd/oat tui   # observe unreadable panels
# After this PR:
git checkout quick-win-fixes-3ko && go run ./cmd/oat tui  # dark theme holds
```
