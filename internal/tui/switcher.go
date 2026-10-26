package tui

import (
	"maps"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/goptics/vartop/internal/fleet"
)

// switcherScreen is the server/unit switcher overlay. It is a singleton owned
// by the root model so its expanded-server state persists across open/close,
// as it did before the screen split. Its units and their health come from
// each server's newest stored snapshot, read while it is open.
type switcherScreen struct {
	switcher list.Model
	expanded string
	keys     switcherKeyMap
}

func newSwitcherScreen() *switcherScreen {
	l := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	l.Title = "Switch"
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)
	return &switcherScreen{switcher: l, keys: newSwitcherKeyMap()}
}

func (s *switcherScreen) Update(m *Model, msg tea.Msg) tea.Cmd {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		switch {
		case key.Matches(keyMsg, s.keys.Quit):
			return tea.Quit
		case key.Matches(keyMsg, s.keys.Close):
			if s.expanded != "" {
				s.expanded = ""
				s.rebuild(m)
				return nil
			}
			m.popScreen()
			return nil
		case key.Matches(keyMsg, s.keys.Collapse):
			if s.expanded != "" {
				s.expanded = ""
				s.rebuild(m)
			}
			return nil
		case key.Matches(keyMsg, s.keys.Select):
			return s.enter(m)
		}
	}

	var cmd tea.Cmd
	s.switcher, cmd = s.switcher.Update(msg)
	return cmd
}

// enter selects or expands the highlighted switcher item. Selecting shows
// the server at once, from its feed: nothing to connect to.
func (s *switcherScreen) enter(m *Model) tea.Cmd {
	switch item := s.switcher.SelectedItem().(type) {
	case switchServerItem:
		if item.units <= 1 || s.expanded == item.name {
			// One unit (or none stored yet), or already expanded: select
			// the merged view.
			m.popScreen()
			m.resetPodView()
			return m.show(item.name)
		}

		// Expand this server
		s.expanded = item.name
		s.rebuild(m)
		return nil

	case switchPodItem:
		m.popScreen()
		m.err = nil
		m.selectedLabel = item.label
		m.showNodeColumn = false
		m.dashboardScreen().rebuildTable(m)
		return m.show(item.server)
	}
	return nil
}

// rebuild reconstructs the switcher list from the config and what the
// switcher last read of the store (see Model.switcherCmd). Servers are
// always shown; units only under the expanded server.
func (s *switcherScreen) rebuild(m *Model) {
	var items []list.Item
	// Sorted: map order is random and rebuild runs on every tick, so an
	// unsorted list would reshuffle under the cursor.
	for _, name := range slices.Sorted(maps.Keys(m.cfg.Servers)) {
		latest := m.switchLatest[name]
		h := fleet.Health(latest)
		expanded := s.expanded == name

		items = append(items, switchServerItem{
			name:     name,
			noun:     fleet.NounFor(m.cfg.Servers[name]),
			units:    h.Total,
			healthy:  h.Healthy,
			expanded: expanded,
		})

		// Add unit rows only for the expanded server
		if expanded && h.Total > 1 {
			for _, p := range latest.Pods {
				items = append(items, switchPodItem{label: p.Label, server: name, healthy: p.Err == ""})
			}
		}
	}

	s.switcher.SetItems(items)
}

// View renders the server switcher with custom footer.
func (s *switcherScreen) View(m *Model) string {
	switcherContent := s.switcher.View()

	// Custom footer for switcher
	keysView := helpFooter(s.keyMap(), m.width-4)

	footer := FooterStyle.Width(m.width).Padding(0, 1).Render(keysView)

	// Calculate spacing
	switcherHeight := lipgloss.Height(switcherContent)
	footerHeight := lipgloss.Height(footer)
	spacing := createSpacing(m.height, switcherHeight, footerHeight, 2)

	// Join with padding
	mainContent := lipgloss.NewStyle().Padding(1, 2).Render(switcherContent)
	fullContent := lipgloss.JoinVertical(
		lipgloss.Left,
		mainContent,
		strings.Repeat("\n", spacing),
		footer,
	)

	return BaseStyle.Render(fullContent)
}
