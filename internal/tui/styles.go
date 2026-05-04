package tui

import "github.com/charmbracelet/lipgloss"

var (
	// Colors
	colorPrimary   = lipgloss.Color("#7C3AED") // purple
	colorSecondary = lipgloss.Color("#06B6D4") // cyan
	colorSuccess   = lipgloss.Color("#22C55E") // green
	colorWarning   = lipgloss.Color("#EAB308") // yellow
	colorDanger    = lipgloss.Color("#EF4444") // red
	colorMuted     = lipgloss.Color("#6B7280") // gray
	colorText      = lipgloss.Color("#E5E7EB") // light gray
	colorBorder    = lipgloss.Color("#374151") // border gray
	colorBrand     = lipgloss.Color("#A78BFA") // light purple for brand accent

	// Status bar
	styleStatusBar = lipgloss.NewStyle().
			Background(lipgloss.Color("#1F2937")).
			Foreground(colorText).
			Padding(0, 1)

	// OAT brand tag in status bar
	styleBrandTag = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#1F2937")).
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

	// Input bar
	styleInputPrompt = lipgloss.NewStyle().
				Foreground(colorPrimary).
				Bold(true)

	// Agent list panel
	styleAgentListTitle = lipgloss.NewStyle().
				Foreground(colorPrimary).
				Bold(true).
				Padding(0, 1)

	// Cursor highlight when navigating the agent list (bright, distinct)
	styleAgentCursor = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#FFFFFF")).
				Background(lipgloss.Color("#4C1D95")).
				Padding(0, 1).
				Bold(true)

	// Active agent (currently being viewed) — subtle highlight
	styleAgentActive = lipgloss.NewStyle().
				Foreground(colorText).
				Background(lipgloss.Color("#374151")).
				Padding(0, 1).
				Bold(true)

	styleAgentNormal = lipgloss.NewStyle().
				Foreground(colorText).
				Padding(0, 1)

	styleAgentNormalType = lipgloss.NewStyle().
				Foreground(colorMuted).
				Padding(0, 1)

	styleAgentListDivider = lipgloss.NewStyle().
				Foreground(colorBorder).
				Padding(0, 1)

	styleAgentAlive = lipgloss.NewStyle().
			Foreground(colorSuccess)

	styleAgentDead = lipgloss.NewStyle().
			Foreground(colorDanger)

	styleAgentWaiting = lipgloss.NewStyle().
				Foreground(colorSecondary)

	// Help text
	styleHelp = lipgloss.NewStyle().
			Foreground(colorMuted)

	// Filter indicator
	styleFilterActive = lipgloss.NewStyle().
				Foreground(colorWarning).
				Bold(true)

	// Token/cost bar (persistent footer)
	styleTokenBar = lipgloss.NewStyle().
			Background(lipgloss.Color("#111827")).
			Foreground(colorMuted).
			Padding(0, 1)

	styleTokenBarModel = lipgloss.NewStyle().
				Foreground(colorSecondary)

	styleTokenBarCost = lipgloss.NewStyle().
				Foreground(colorWarning).
				Bold(true)
)
