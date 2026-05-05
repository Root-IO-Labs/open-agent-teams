package tui

import "github.com/charmbracelet/lipgloss"

// Semantic colors. Each is an AdaptiveColor so lipgloss automatically picks
// the variant appropriate for the terminal's detected background. Users can
// override detection with OAT_THEME=dark|light (see runUI).
var (
	colorPrimary   = lipgloss.AdaptiveColor{Dark: "#7C3AED", Light: "#6D28D9"} // purple
	colorSecondary = lipgloss.AdaptiveColor{Dark: "#06B6D4", Light: "#0891B2"} // cyan
	colorSuccess   = lipgloss.AdaptiveColor{Dark: "#22C55E", Light: "#15803D"} // green
	colorWarning   = lipgloss.AdaptiveColor{Dark: "#EAB308", Light: "#A16207"} // yellow
	colorDanger    = lipgloss.AdaptiveColor{Dark: "#EF4444", Light: "#DC2626"} // red
	colorMuted     = lipgloss.AdaptiveColor{Dark: "#6B7280", Light: "#6B7280"} // gray (works on both)
	colorText      = lipgloss.AdaptiveColor{Dark: "#E5E7EB", Light: "#111827"} // body text
	colorBorder    = lipgloss.AdaptiveColor{Dark: "#374151", Light: "#D1D5DB"} // borders/dividers
	colorBrand     = lipgloss.AdaptiveColor{Dark: "#A78BFA", Light: "#7C3AED"} // brand accent purple

	// Backgrounds
	colorBg        = lipgloss.AdaptiveColor{Dark: "#0F172A", Light: "#FFFFFF"} // main viewport / input bar
	colorBgPanel   = lipgloss.AdaptiveColor{Dark: "#111827", Light: "#F9FAFB"} // side panels (agent list, etc.)
	colorBgBar     = lipgloss.AdaptiveColor{Dark: "#1F2937", Light: "#F3F4F6"} // status bar
	colorBgBarDim  = lipgloss.AdaptiveColor{Dark: "#111827", Light: "#E5E7EB"} // token bar (deeper)
	colorBgActive  = lipgloss.AdaptiveColor{Dark: "#374151", Light: "#D1D5DB"} // active agent highlight
	colorBgCursor  = lipgloss.AdaptiveColor{Dark: "#4C1D95", Light: "#DDD6FE"} // cursor highlight bg
	colorFgCursor  = lipgloss.AdaptiveColor{Dark: "#FFFFFF", Light: "#1F2937"} // cursor text
	colorFgOnBrand = lipgloss.AdaptiveColor{Dark: "#1F2937", Light: "#FFFFFF"} // text on brand-colored bg
)

var (
	// Status bar
	styleStatusBar = lipgloss.NewStyle().
			Background(colorBgBar).
			Foreground(colorText).
			Padding(0, 1)

	// OAT brand tag in status bar
	styleBrandTag = lipgloss.NewStyle().
			Foreground(colorFgOnBrand).
			Background(colorBrand).
			Bold(true).
			Padding(0, 1)

	styleStatusRepo = lipgloss.NewStyle().
			Foreground(colorPrimary).
			Bold(true)

	styleStatusAgent = lipgloss.NewStyle().
				Foreground(colorSecondary)

	styleStatusOK = lipgloss.NewStyle().
			Foreground(colorSuccess)

	styleStatusWarn = lipgloss.NewStyle().
			Foreground(colorWarning)

	// Input bar (explicit dark bg so the prompt is legible on any terminal)
	styleInputBar = lipgloss.NewStyle().
			Background(colorBg).
			Foreground(colorText)

	styleInputPrompt = lipgloss.NewStyle().
				Foreground(colorPrimary).
				Background(colorBg).
				Bold(true)

	// Agent list panel
	styleAgentListTitle = lipgloss.NewStyle().
				Foreground(colorPrimary).
				Background(colorBgPanel).
				Bold(true).
				Padding(0, 1)

	// Cursor highlight when navigating the agent list (bright, distinct)
	styleAgentCursor = lipgloss.NewStyle().
				Foreground(colorFgCursor).
				Background(colorBgCursor).
				Padding(0, 1).
				Bold(true)

	// Active agent (currently being viewed) — subtle highlight
	styleAgentActive = lipgloss.NewStyle().
				Foreground(colorText).
				Background(colorBgActive).
				Padding(0, 1).
				Bold(true)

	styleAgentNormal = lipgloss.NewStyle().
				Foreground(colorText).
				Background(colorBgPanel).
				Padding(0, 1)

	styleAgentNormalType = lipgloss.NewStyle().
				Foreground(colorMuted).
				Background(colorBgPanel).
				Padding(0, 1)

	styleAgentListDivider = lipgloss.NewStyle().
				Foreground(colorBorder).
				Background(colorBgPanel).
				Padding(0, 1)

	styleAgentAlive = lipgloss.NewStyle().
			Foreground(colorSuccess)

	styleAgentDead = lipgloss.NewStyle().
			Foreground(colorDanger)

	styleAgentWaiting = lipgloss.NewStyle().
				Foreground(colorSecondary)

	// Help text
	styleHelp = lipgloss.NewStyle().
			Foreground(colorMuted).
			Background(colorBg)

	// Viewport background fill — applied to the main content area so the
	// dark theme covers gaps under the agent output (light-terminal fix).
	styleViewport = lipgloss.NewStyle().
			Background(colorBg).
			Foreground(colorText)

	// Filter indicator
	styleFilterActive = lipgloss.NewStyle().
				Foreground(colorWarning).
				Bold(true)

	// Token/cost bar (persistent footer)
	styleTokenBar = lipgloss.NewStyle().
			Background(colorBgBarDim).
			Foreground(colorMuted).
			Padding(0, 1)

	styleTokenBarModel = lipgloss.NewStyle().
				Foreground(colorSecondary)

	//nolint:unused // reserved for upcoming cost-bar UI in the token panel
	styleTokenBarCost = lipgloss.NewStyle().
				Foreground(colorWarning).
				Bold(true)
)
