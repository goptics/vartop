package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/goptics/varmq/cmd/vartop/internal/config"
)

// switcherScreen is the server/pod switcher overlay. It is a singleton owned by
// the root model so its expanded-server state persists across open/close, as it
// did before the screen split.
type switcherScreen struct {
	switcher list.Model
	expanded string
}

func newSwitcherScreen() *switcherScreen {
	l := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	l.Title = "Switch"
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)
	return &switcherScreen{switcher: l}
}

func (s *switcherScreen) Update(m *Model, msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "ctrl+c", "q":
			return tea.Quit
		case "esc":
			if s.expanded != "" {
				s.expanded = ""
				s.rebuild(m)
				return nil
			}
			m.popScreen()
			return nil
		case "backspace", "h":
			if s.expanded != "" {
				s.expanded = ""
				s.rebuild(m)
			}
			return nil
		case "enter":
			return s.enter(m)
		}
	}

	var cmd tea.Cmd
	s.switcher, cmd = s.switcher.Update(msg)
	return cmd
}

// enter selects or expands the highlighted switcher item.
func (s *switcherScreen) enter(m *Model) tea.Cmd {
	switch item := s.switcher.SelectedItem().(type) {
	case switchServerItem:
		if item.podCount <= 1 {
			// Single pod (or kubernetes, which has no static endpoints):
			// select immediately.
			m.popScreen()
			m.resetPodView()
			return m.selectServer(item.name)
		}

		if s.expanded == item.name {
			// Already expanded: select merged view
			m.popScreen()
			m.resetPodView()
			return m.selectServer(item.name)
		}

		// Expand this server
		s.expanded = item.name
		s.rebuild(m)
		return nil

	case switchPodItem:
		m.popScreen()
		m.err = nil
		m.workerOrigins = make(map[string]string)
		m.selectedPodOrigin = item.origin
		m.showNodeColumn = false
		m.dashboardScreen().rebuildTable(m)
		return m.selectServer(item.server)
	}
	return nil
}

// rebuild reconstructs the switcher list. Servers are always shown. Pods are
// shown only under the currently expanded server.
func (s *switcherScreen) rebuild(m *Model) {
	var items []list.Item
	for name, srv := range m.cfg.Servers {
		endpoints := srv.GetEndpoints()
		podCount := len(endpoints)

		// Count healthy pods
		healthyPods := 0
		for _, ep := range endpoints {
			if m.podHealth[ep] {
				healthyPods++
			}
		}

		expanded := s.expanded == name

		items = append(items, switchServerItem{
			name:        name,
			podCount:    podCount,
			healthyPods: healthyPods,
			expanded:    expanded,
		})

		// Add pod rows only for the expanded server
		if expanded && podCount > 1 {
			for _, ep := range endpoints {
				items = append(items, switchPodItem{
					name:    config.ShortOrigin(ep),
					server:  name,
					origin:  ep,
					healthy: m.podHealth[ep],
				})
			}
		}
	}

	s.switcher.SetItems(items)
}

// View renders the server switcher with custom footer.
func (s *switcherScreen) View(m *Model) string {
	switcherContent := s.switcher.View()

	// Custom footer for switcher
	var footerKeys string
	if s.expanded != "" {
		footerKeys = lipgloss.JoinHorizontal(lipgloss.Center,
			HelpKeyStyle.Render("q"), HelpDescStyle.Render(" quit • "),
			HelpKeyStyle.Render("enter"), HelpDescStyle.Render(" select • "),
			HelpKeyStyle.Render("esc"), HelpDescStyle.Render(" collapse • "),
			HelpKeyStyle.Render("↑/↓"), HelpDescStyle.Render(" navigate"),
		)
	} else {
		footerKeys = lipgloss.JoinHorizontal(lipgloss.Center,
			HelpKeyStyle.Render("q"), HelpDescStyle.Render(" quit • "),
			HelpKeyStyle.Render("enter"), HelpDescStyle.Render(" expand/select • "),
			HelpKeyStyle.Render("↑/↓"), HelpDescStyle.Render(" navigate"),
		)
	}

	footer := FooterStyle.Width(m.width).Padding(0, 1).Render(footerKeys)

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
