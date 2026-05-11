package tui

import "github.com/charmbracelet/bubbles/key"

// keyMap defines all keybindings for the TUI.
type keyMap struct {
	Quit           key.Binding
	TogglePanel    key.Binding
	Workspace      key.Binding
	NextAgent      key.Binding
	PrevAgent      key.Binding
	SelectAgent    key.Binding
	ToggleFilter   key.Binding
	ScrollUp       key.Binding
	ScrollDown     key.Binding
	PageUp         key.Binding
	PageDown       key.Binding
	GoToTop        key.Binding
	GoToBottom     key.Binding
	Interrupt      key.Binding
	NewWorker      key.Binding
	ToggleReadOnly key.Binding
	ExpandView     key.Binding
	OpenLog        key.Binding
	OpenPlanner    key.Binding
}

var keys = keyMap{
	Quit: key.NewBinding(
		key.WithKeys("ctrl+c"),
		key.WithHelp("ctrl+c", "quit"),
	),
	TogglePanel: key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", "agent list"),
	),
	Workspace: key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "workspace"),
	),
	NextAgent: key.NewBinding(
		key.WithKeys("ctrl+n"),
		key.WithHelp("ctrl+n", "next agent"),
	),
	PrevAgent: key.NewBinding(
		key.WithKeys("ctrl+p"),
		key.WithHelp("ctrl+p", "prev agent"),
	),
	SelectAgent: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "select"),
	),
	ToggleFilter: key.NewBinding(
		key.WithKeys("ctrl+f"),
		key.WithHelp("ctrl+f", "toggle filter"),
	),
	ScrollUp: key.NewBinding(
		key.WithKeys("up"),
		key.WithHelp("up", "scroll up"),
	),
	ScrollDown: key.NewBinding(
		key.WithKeys("down"),
		key.WithHelp("down", "scroll down"),
	),
	PageUp: key.NewBinding(
		key.WithKeys("pgup"),
		key.WithHelp("pgup", "page up"),
	),
	PageDown: key.NewBinding(
		key.WithKeys("pgdown"),
		key.WithHelp("pgdn", "page down"),
	),
	GoToTop: key.NewBinding(
		key.WithKeys("home"),
		key.WithHelp("home", "top"),
	),
	GoToBottom: key.NewBinding(
		key.WithKeys("end"),
		key.WithHelp("end", "bottom"),
	),
	Interrupt: key.NewBinding(
		key.WithKeys("ctrl+x"),
		key.WithHelp("ctrl+x", "cancel thinking"),
	),
	NewWorker: key.NewBinding(
		key.WithKeys("ctrl+w"),
		key.WithHelp("ctrl+w", "new worker"),
	),
	ToggleReadOnly: key.NewBinding(
		key.WithKeys("ctrl+r"),
		key.WithHelp("ctrl+r", "toggle input"),
	),
	ExpandView: key.NewBinding(
		key.WithKeys("ctrl+e"),
		key.WithHelp("ctrl+e", "expand view"),
	),
	OpenLog: key.NewBinding(
		key.WithKeys("ctrl+o"),
		key.WithHelp("ctrl+o", "open log"),
	),
	OpenPlanner: key.NewBinding(
		key.WithKeys("ctrl+l"),
		key.WithHelp("ctrl+l", "open planner"),
	),
}
