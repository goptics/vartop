package tui

import "github.com/charmbracelet/bubbles/key"

// dashboardKeyMap holds the dashboard's keybindings. It implements
// help.KeyMap so the footer can render the keys for the current mode.
type dashboardKeyMap struct {
	inAction bool

	Move    key.Binding
	Detail  key.Binding
	Actions key.Binding
	Refresh key.Binding
	Switch  key.Binding
	Quit    key.Binding

	Back    key.Binding
	Pause   key.Binding
	Resume  key.Binding
	Stop    key.Binding
	Restart key.Binding
}

func newDashboardKeyMap() dashboardKeyMap {
	return dashboardKeyMap{
		// Move is handled by the embedded table; it is listed here only so the
		// footer documents it.
		Move:    key.NewBinding(key.WithKeys("up", "down", "j", "k"), key.WithHelp("↑/↓", "move")),
		Detail:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "view details")),
		Actions: key.NewBinding(key.WithKeys("a", "A"), key.WithHelp("a", "actions")),
		Refresh: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Switch:  key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "switch")),
		Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),

		Back:    key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		Pause:   key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "pause")),
		Resume:  key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "resume")),
		Stop:    key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "stop")),
		Restart: key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "restart")),
	}
}

// keyMap returns the keymap reflecting the dashboard's current mode.
func (d *dashboardScreen) keyMap() dashboardKeyMap {
	k := d.keys
	k.inAction = d.inActionSelection
	return k
}

func (k dashboardKeyMap) ShortHelp() []key.Binding {
	if k.inAction {
		return []key.Binding{k.Back, k.Pause, k.Resume, k.Stop, k.Restart}
	}
	return []key.Binding{k.Move, k.Detail, k.Actions, k.Refresh, k.Switch, k.Quit}
}

func (k dashboardKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Move, k.Detail, k.Actions},
		{k.Refresh, k.Switch, k.Quit},
		{k.Back, k.Pause, k.Resume, k.Stop, k.Restart},
	}
}

// detailKeyMap holds the worker detail screen's keybindings.
type detailKeyMap struct {
	inAction bool

	Quit     key.Binding
	Back     key.Binding
	Actions  key.Binding
	Refresh  key.Binding
	Up       key.Binding
	Down     key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Home     key.Binding
	End      key.Binding

	Pause   key.Binding
	Resume  key.Binding
	Stop    key.Binding
	Restart key.Binding
}

func newDetailKeyMap() detailKeyMap {
	return detailKeyMap{
		Quit:     key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Back:     key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		Actions:  key.NewBinding(key.WithKeys("a", "A"), key.WithHelp("a", "actions")),
		Refresh:  key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "scroll")),
		Down:     key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "scroll")),
		PageUp:   key.NewBinding(key.WithKeys("pgup", "b"), key.WithHelp("pgup/b", "page up")),
		PageDown: key.NewBinding(key.WithKeys("pgdown", " "), key.WithHelp("pgdn/space", "page down")),
		Home:     key.NewBinding(key.WithKeys("home", "g"), key.WithHelp("g", "top")),
		End:      key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("G", "bottom")),

		Pause:   key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "pause")),
		Resume:  key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "resume")),
		Stop:    key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "stop")),
		Restart: key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "restart")),
	}
}

// keyMap returns the keymap reflecting the detail screen's current mode.
func (d *detailScreen) keyMap() detailKeyMap {
	k := d.keys
	k.inAction = d.inAction
	return k
}

func (k detailKeyMap) ShortHelp() []key.Binding {
	if k.inAction {
		return []key.Binding{k.Back, k.Pause, k.Resume, k.Stop, k.Restart}
	}
	return []key.Binding{k.Back, k.Actions, k.Refresh, k.Up, k.Down, k.Home, k.End, k.Quit}
}

func (k detailKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Back, k.Actions, k.Refresh},
		{k.Up, k.Down, k.PageUp, k.PageDown},
		{k.Home, k.End, k.Quit},
		{k.Pause, k.Resume, k.Stop, k.Restart},
	}
}

// switcherKeyMap holds the server switcher's keybindings.
type switcherKeyMap struct {
	expanded bool

	Quit     key.Binding
	Close    key.Binding
	Collapse key.Binding
	Select   key.Binding
	Move     key.Binding
}

func newSwitcherKeyMap() switcherKeyMap {
	return switcherKeyMap{
		Quit:     key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Close:    key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "close")),
		Collapse: key.NewBinding(key.WithKeys("h", "backspace"), key.WithHelp("h", "collapse")),
		Select:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "expand/select")),
		Move:     key.NewBinding(key.WithKeys("up", "down", "j", "k"), key.WithHelp("↑/↓", "navigate")),
	}
}

// keyMap returns the keymap reflecting whether a server is expanded.
func (s *switcherScreen) keyMap() switcherKeyMap {
	k := s.keys
	if s.expanded != "" {
		k.expanded = true
		k.Select.SetHelp("enter", "select")
		k.Close.SetHelp("esc", "collapse")
	}
	return k
}

func (k switcherKeyMap) ShortHelp() []key.Binding {
	if k.expanded {
		return []key.Binding{k.Quit, k.Select, k.Collapse, k.Close, k.Move}
	}
	return []key.Binding{k.Quit, k.Select, k.Close, k.Move}
}

func (k switcherKeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Quit, k.Select},
		{k.Close, k.Collapse, k.Move},
	}
}
