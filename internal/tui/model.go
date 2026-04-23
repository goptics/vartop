package tui

import (
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/goptics/varmq/cmd/vartop/internal/api"
	"github.com/goptics/varmq/cmd/vartop/internal/config"
)

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

	// Worker detail view state
	showWorkerDetail     bool
	workerDetails        *api.WorkerDetails
	currentWorkerName    string // Track which worker we're viewing for polling
	inWorkerDetailAction bool   // Action mode for worker detail view
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
		table.WithHeight(6),  // Reduced from 10 to leave more room for header
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
		cfg:                  cfg,
		server:               server,
		client:               client,
		table:                t,
		switcher:             l,
		isHealthy:            true, // Assume healthy initially
		showWorkerDetail:     false,
		workerDetails:        nil,
		currentWorkerName:    "",
		inWorkerDetailAction: false,
		width:                80,  // Default width, will be updated by WindowSizeMsg
		height:               24,  // Default height, will be updated by WindowSizeMsg
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.checkHealthCmd(),
		m.tickCmd(),
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
		m.handleWindowSize(msg)

	case tickMsg:
		cmd = m.tickCmd()
		if !m.showSwitcher {
			cmds = append(cmds, m.checkHealthCmd())
		}
		cmds = append(cmds, cmd)

	case healthMsg:
		cmd = m.handleHealth(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}

	case workersMsg:
		m.handleWorkers(msg)

	case workerDetailsMsg:
		cmd = m.handleWorkerDetails(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}

	case errMsg:
		cmd = m.handleError(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	if m.showSwitcher {
		m.switcher, cmd = m.switcher.Update(msg)
		cmds = append(cmds, cmd)
	} else if !m.showWorkerDetail {
		m.table, cmd = m.table.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

func (m Model) updateTable(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Handle worker detail view keys
	if m.showWorkerDetail {
		if m.inWorkerDetailAction {
			// Action mode in detail view
			switch msg.String() {
			case "ctrl+c", "q":
				return m, tea.Quit
			case "esc":
				m.inWorkerDetailAction = false
				return m, nil
			case "p":
				m.inWorkerDetailAction = false
				return m, m.actionCurrentWorkerCmd("pause")
			case "r":
				m.inWorkerDetailAction = false
				return m, m.actionCurrentWorkerCmd("resume")
			case "s":
				m.inWorkerDetailAction = false
				return m, m.actionCurrentWorkerCmd("stop")
			case "R":
				m.inWorkerDetailAction = false
				return m, m.actionCurrentWorkerCmd("restart")
			}
			return m, nil
		}

		// Normal detail view mode
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "esc":
			m.showWorkerDetail = false
			m.workerDetails = nil
			m.currentWorkerName = ""
			m.inWorkerDetailAction = false
			return m, tea.Batch(m.checkHealthCmd(), m.fetchWorkersCmd())
		case "a", "A":
			if m.currentWorkerName != "" {
				m.inWorkerDetailAction = !m.inWorkerDetailAction
			}
			return m, nil
		case "r":
			if m.currentWorkerName != "" {
				m.workerDetails = nil // Clear to show loading state
				return m, m.fetchWorkerDetailsCmd(m.currentWorkerName)
			}
			return m, nil
		}
		return m, nil
	}

	if m.inActionSelection {
		switch msg.String() {
		case "esc":
			m.inActionSelection = false
			return m, nil
		case "p":
			m.inActionSelection = false
			return m, m.actionWorkerCmd(m.table.SelectedRow()[0], "pause")
		case "r":
			m.inActionSelection = false
			return m, m.actionWorkerCmd(m.table.SelectedRow()[0], "resume")
		case "s":
			m.inActionSelection = false
			return m, m.actionWorkerCmd(m.table.SelectedRow()[0], "stop")
		case "R":
			m.inActionSelection = false
			return m, m.actionWorkerCmd(m.table.SelectedRow()[0], "restart")
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
			return m, m.actionWorkerCmd(m.table.SelectedRow()[0], "resume")
		}
		m.err = nil
		return m, tea.Batch(m.checkHealthCmd(), m.fetchWorkersCmd())
	case "a", "A":
		if len(m.table.Rows()) > 0 {
			m.inActionSelection = !m.inActionSelection
		}
		return m, nil
	case "tab":
		m.showSwitcher = true
		return m, nil
	case "enter":
		// Navigate to worker detail view
		if m.isHealthy && len(m.table.Rows()) > 0 {
			selectedRow := m.table.SelectedRow()
			if selectedRow != nil {
				workerName := selectedRow[0]
				m.showWorkerDetail = true
				m.workerDetails = nil            // Clear previous details
				m.currentWorkerName = workerName // Store for polling
				return m, m.fetchWorkerDetailsCmd(workerName)
			}
		}
		return m, nil
	case "esc":
		// Return to dashboard from detail view
		if m.showWorkerDetail {
			m.showWorkerDetail = false
			m.workerDetails = nil
			m.currentWorkerName = "" // Clear worker name
			return m, tea.Batch(m.checkHealthCmd(), m.fetchWorkersCmd())
		}
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
				return m, tea.Batch(m.checkHealthCmd(), m.fetchWorkersCmd())
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
		return m.renderSwitcherView()
	}

	if m.showWorkerDetail {
		return m.renderWorkerDetailView()
	}

	return m.renderDashboardView()
}
