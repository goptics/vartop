package tui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/goptics/varmq/cmd/vartop/internal/api"
	"github.com/goptics/varmq/cmd/vartop/internal/config"
)

type tickMsg time.Time
type workersMsg []api.WorkerSummary
type healthMsg struct{ ok bool }
type errMsg struct{ err error }

func (e errMsg) Error() string { return e.err.Error() }

// serverItem implements list.Item for the context switcher
type serverItem struct {
	name     string
	endpoint string
}

func (i serverItem) Title() string       { return i.name }
func (i serverItem) Description() string { return i.endpoint }
func (i serverItem) FilterValue() string { return i.name }

// Model is the main state of the TUI
type Model struct {
	cfg      *config.Config
	server   *config.Server
	client   *api.Client
	table    table.Model
	switcher list.Model

	err        error
	lastUpdate time.Time
	width      int
	height     int

	showSwitcher      bool
	inActionSelection bool
	isHealthy         bool
}

// NewModel initializes the application state.
func NewModel(cfg *config.Config, server *config.Server) *Model {
	client := api.NewClient(server)

	// Determine table columns
	columns := []table.Column{
		{Title: "Name", Width: 20},
		{Title: "Status", Width: 15},
		{Title: lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render("Concurrency"), Width: 12},
		{Title: lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render("Processing"), Width: 12},
		{Title: lipgloss.NewStyle().Width(10).Align(lipgloss.Center).Render("Pending"), Width: 10},
		{Title: lipgloss.NewStyle().Width(8).Align(lipgloss.Center).Render("Idle"), Width: 8},
	}

	t := table.New(
		table.WithColumns(columns),
		table.WithFocused(true),
		table.WithHeight(10),
	)

	t.SetStyles(TableStyles())

	// Init Switcher List
	var items []list.Item
	for n, srv := range cfg.Servers {
		items = append(items, serverItem{name: n, endpoint: srv.Endpoint})
	}

	l := list.New(items, list.NewDefaultDelegate(), 0, 0)
	l.Title = "Select VarMQ Server"
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)

	return &Model{
		cfg:       cfg,
		server:    server,
		client:    client,
		table:     t,
		switcher:  l,
		isHealthy: true, // Assume healthy initially
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.checkHealth(),
		m.tick(),
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.showSwitcher {
			return m.updateSwitcher(msg)
		}
		return m.updateTable(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		tHeight := msg.Height - 8

		// Responsive table columns
		// Total fixed width of other columns + generous padding/borders space
		fixedWidths := 15 + 12 + 12 + 10 + 8 + 10
		nameWidth := msg.Width - fixedWidths
		if nameWidth < 15 {
			nameWidth = 15 // Minimum width for name
		}

		columns := []table.Column{
			{Title: "Name", Width: nameWidth},
			{Title: "Status", Width: 15},
			{Title: lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render("Concurrency"), Width: 12},
			{Title: lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render("Processing"), Width: 12},
			{Title: lipgloss.NewStyle().Width(10).Align(lipgloss.Center).Render("Pending"), Width: 10},
			{Title: lipgloss.NewStyle().Width(8).Align(lipgloss.Center).Render("Idle"), Width: 8},
		}

		// Recreate table rather than mutating to avoid underlying viewport panics
		s := TableStyles()
		selected := m.table.Cursor()
		rows := m.table.Rows()

		t := table.New(
			table.WithColumns(columns),
			table.WithRows(rows),
			table.WithFocused(true),
		)
		if tHeight > 0 {
			t.SetHeight(tHeight)
		}
		t.SetStyles(s)
		t.SetCursor(selected)

		m.table = t

		lHeight := msg.Height - 6
		if lHeight > 0 {
			m.switcher.SetSize(msg.Width-2, lHeight)
		}

	case tickMsg:
		if !m.showSwitcher {
			cmds = append(cmds, m.checkHealth())
		}
		cmds = append(cmds, m.tick())

	case healthMsg:
		m.isHealthy = msg.ok
		if m.isHealthy && !m.showSwitcher {
			cmds = append(cmds, m.fetchWorkers())
		}

	case workersMsg:
		m.lastUpdate = time.Now()
		m.err = nil

		rows := make([]table.Row, len(msg))
		for i, w := range msg {
			rows[i] = table.Row{
				w.Name,
				w.Status,
				lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render(fmt.Sprint(w.Concurrency)),
				lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render(fmt.Sprint(w.NumProcessing)),
				lipgloss.NewStyle().Width(10).Align(lipgloss.Center).Render(fmt.Sprint(w.NumPending)),
				lipgloss.NewStyle().Width(8).Align(lipgloss.Center).Render(fmt.Sprint(w.NumIdleWorkers)),
			}
		}

		// Retain selection if possible
		selected := m.table.Cursor()
		m.table.SetRows(rows)
		if selected < len(rows) {
			m.table.SetCursor(selected)
		}

	case errMsg:
		m.err = msg.err
	}

	if m.showSwitcher {
		m.switcher, cmd = m.switcher.Update(msg)
		cmds = append(cmds, cmd)
	} else {
		m.table, cmd = m.table.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

func (m Model) updateTable(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.inActionSelection {
		switch msg.String() {
		case "esc":
			m.inActionSelection = false
			return m, nil
		case "p":
			m.inActionSelection = false
			return m, m.actionSelectedWorker("pause")
		case "r":
			m.inActionSelection = false
			return m, m.actionSelectedWorker("resume")
		case "s":
			m.inActionSelection = false
			return m, m.actionSelectedWorker("stop")
		case "R":
			m.inActionSelection = false
			return m, m.actionSelectedWorker("restart")
		}
		// If in action selection, don't let table keys through except navigation maybe?
		// Actually user said "options will be shows eaxt where the main menu exsit",
		// he probably wants navigation to still work if he wants to change worker.
		// Let's allow navigation.
	}

	switch msg.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "r":
		if m.inActionSelection {
			// In action selection 'r' is resume
			m.inActionSelection = false
			return m, m.actionSelectedWorker("resume")
		}
		m.err = nil
		return m, tea.Batch(m.checkHealth(), m.fetchWorkers())
	case "a", "A":
		if len(m.table.Rows()) > 0 {
			m.inActionSelection = !m.inActionSelection
		}
		return m, nil
	case "tab":
		m.showSwitcher = true
		return m, nil
	}

	if !m.isHealthy || len(m.table.Rows()) == 0 {
		return m, nil
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m Model) updateSwitcher(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "esc":
		m.showSwitcher = false
		return m, nil
	case "enter":
		if i, ok := m.switcher.SelectedItem().(serverItem); ok {
			m.showSwitcher = false
			m.err = nil

			if srv, err := m.cfg.GetActiveServer(i.name); err == nil {
				m.server = srv
				m.client = api.NewClient(srv)
				return m, tea.Batch(m.checkHealth(), m.fetchWorkers())
			} else {
				m.err = err
			}
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.switcher, cmd = m.switcher.Update(msg)
	return m, cmd
}

func (m Model) View() string {
	if m.showSwitcher {
		return BaseStyle.Render(
			lipgloss.NewStyle().Padding(1, 2).Render(m.switcher.View()),
		)
	}

	// 1. Header
	var statusIndicator string
	if m.isHealthy {
		statusIndicator = lipgloss.NewStyle().Foreground(ColorSuccess).Render(" ● Active ")
	} else {
		statusIndicator = lipgloss.NewStyle().Foreground(ColorError).Render(" ○ Inactive ")
	}

	serverName := TitleStyle.Render(fmt.Sprintf(" vartop [%s] ", m.server.Name))
	endpointInfo := fmt.Sprintf("→ %s", m.server.Endpoint)

	headerContent := lipgloss.JoinHorizontal(lipgloss.Center, serverName, statusIndicator, lipgloss.NewStyle().PaddingLeft(1).Render(endpointInfo))
	header := HeaderStyle.Width(m.width).Render(headerContent)

	// 2. Body / Table
	var bodyView string
	if !m.isHealthy {
		offlineMsg := lipgloss.NewStyle().
			Foreground(ColorSubtle).
			Padding(2, 2).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorError).
			Render(fmt.Sprintf("Server is unreachable at %s\nPolling in background...", m.server.Endpoint))

		bodyView = lipgloss.Place(m.width, m.table.Height()+2, lipgloss.Center, lipgloss.Center, offlineMsg)
	} else {
		// Only display errors if healthy. If unhealthy, we know it's failing
		var errorView string
		if m.err != nil {
			errorView = lipgloss.NewStyle().Foreground(ColorError).Padding(0, 1).Render(fmt.Sprintf("⚠️  Error: %v", m.err))
			errorView += "\n"
		}

		tableView := lipgloss.NewStyle().Padding(1, 1).Render(m.table.View())
		bodyView = lipgloss.JoinVertical(lipgloss.Left, errorView, tableView)
	}

	// 3. Footer
	lastUpdateStr := "Never"
	if !m.lastUpdate.IsZero() {
		lastUpdateStr = m.lastUpdate.Format("15:04:05")
	}

	pollInfo := fmt.Sprintf("Updated: %s (Polling: %ds)", lastUpdateStr, m.server.Interval)

	var keysView string
	if m.inActionSelection {
		keysView = lipgloss.JoinHorizontal(lipgloss.Center,
			HelpKeyStyle.Render("esc"), HelpDescStyle.Render(" back • "),
			HelpKeyStyle.Render("p"), HelpDescStyle.Render(" pause • "),
			HelpKeyStyle.Render("r"), HelpDescStyle.Render(" resume • "),
			HelpKeyStyle.Render("s"), HelpDescStyle.Render(" stop • "),
			HelpKeyStyle.Render("R"), HelpDescStyle.Render(" restart"),
		)
	} else {
		keysView = lipgloss.JoinHorizontal(lipgloss.Center,
			HelpKeyStyle.Render("q"), HelpDescStyle.Render(" quit • "),
			HelpKeyStyle.Render("↑/↓"), HelpDescStyle.Render(" move • "),
			HelpKeyStyle.Render("a"), HelpDescStyle.Render(" actions • "),
			HelpKeyStyle.Render("r"), HelpDescStyle.Render(" refresh • "),
			HelpKeyStyle.Render("tab"), HelpDescStyle.Render(" switch server"),
		)
	}

	// Create columns: keys on left, blank space, pollInfo on right
	leftCol := lipgloss.NewStyle().Width(m.width - lipgloss.Width(pollInfo) - 2).Render(keysView)
	footerContent := lipgloss.JoinHorizontal(lipgloss.Right, leftCol, pollInfo)

	footer := FooterStyle.Width(m.width).Padding(0, 1).Render(footerContent)

	return BaseStyle.Render(
		lipgloss.JoinVertical(lipgloss.Left,
			header,
			bodyView,
			footer,
		),
	)
}

func (m Model) checkHealth() tea.Cmd {
	return func() tea.Msg {
		err := m.client.GetHealth()
		return healthMsg{ok: err == nil}
	}
}

func (m Model) fetchWorkers() tea.Cmd {
	return func() tea.Msg {
		workers, err := m.client.ListWorkers()
		if err != nil {
			return errMsg{err}
		}
		return workersMsg(workers)
	}
}

func (m Model) actionSelectedWorker(action string) tea.Cmd {
	if !m.isHealthy || len(m.table.Rows()) == 0 {
		return nil
	}

	selectedRow := m.table.SelectedRow()
	if selectedRow == nil {
		return nil
	}
	workerName := selectedRow[0]

	return func() tea.Msg {
		err := m.client.Action(workerName, action)
		if err != nil {
			return errMsg{err}
		}
		// If action suceeded, immediately fetch new status
		return m.fetchWorkers()()
	}
}

func (m Model) tick() tea.Cmd {
	interval := m.server.Interval
	if interval <= 0 {
		interval = 5 // Default safe fallback
	}

	return tea.Tick(time.Duration(interval)*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}
