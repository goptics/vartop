package tui

import (
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/goptics/varmq/cmd/vartop/internal/api"
	"github.com/goptics/varmq/cmd/vartop/internal/config"
	"github.com/goptics/varmq/cmd/vartop/internal/discovery"
)

// Model is the main state of the TUI
type Model struct {
	cfg      *config.Config
	server   *config.Server
	pool     *api.Pool
	table    table.Model
	switcher list.Model

	err        error
	lastUpdate time.Time
	width      int
	height     int

	showSwitcher      bool
	inActionSelection bool
	isHealthy         bool

	// Multi-endpoint state
	workerOrigins          map[string]string // workerName -> origin endpoint (for action routing)
	podHealth              map[string]bool   // per-endpoint health status
	selectedPodOrigin      string            // empty = merged view; non-empty = single pod view
	switcherExpandedServer string            // which server is expanded in the switch view
	showNodeColumn         bool              // false when viewing a single pod (Node column hidden)

	// Worker detail view state
	showWorkerDetail      bool
	workerDetails         *api.WorkerDetails
	currentWorkerName     string // Track which worker we're viewing for polling
	inWorkerDetailAction  bool   // Action mode for worker detail view
	workerDetailScrollY   int    // Vertical scroll offset for worker detail view
	workerDetailMaxScroll int    // Maximum scroll value (0 if no overflow)
	currentWorkerOrigin   string // Origin of the worker being viewed (for action routing)
}

// sourceFor builds the discovery source matching the server configuration:
// Kubernetes EndpointSlices for kubernetes servers, a fixed list otherwise.
func sourceFor(server *config.Server) (discovery.Source, error) {
	if server.Kubernetes != nil {
		return discovery.NewKubernetes(*server.Kubernetes)
	}
	return discovery.NewStatic(server.GetEndpoints()), nil
}

// NewModel initializes the application state.
func NewModel(cfg *config.Config, server *config.Server) *Model {
	// Construction errors (e.g. a failing Kubernetes watcher) are surfaced
	// through the TUI's existing error display instead of aborting startup.
	source, err := sourceFor(server)
	var pool *api.Pool
	if err == nil {
		pool = api.NewPool(source, server.Token)
	}

	// Determine table columns
	columns := []table.Column{
		{Title: "Name", Width: 18},
		{Title: "Node", Width: 12},
		{Title: "Status", Width: 12},
		{Title: lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render("Concurrency"), Width: 12},
		{Title: lipgloss.NewStyle().Width(12).Align(lipgloss.Center).Render("Processing"), Width: 12},
		{Title: lipgloss.NewStyle().Width(10).Align(lipgloss.Center).Render("Pending"), Width: 10},
		{Title: lipgloss.NewStyle().Width(8).Align(lipgloss.Center).Render("Idle"), Width: 8},
	}

	t := table.New(
		table.WithColumns(columns),
		table.WithFocused(true),
		table.WithHeight(6), // Reduced from 10 to leave more room for header
	)

	t.SetStyles(TableStyles())

	// Init Switcher List (will be populated by rebuildSwitcherItems)
	l := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	l.Title = "Switch"
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)

	m := &Model{
		cfg:                    cfg,
		server:                 server,
		pool:                   pool,
		err:                    err,
		table:                  t,
		switcher:               l,
		isHealthy:              true, // Assume healthy initially
		workerOrigins:          make(map[string]string),
		podHealth:              make(map[string]bool),
		selectedPodOrigin:      "",
		switcherExpandedServer: "",
		showNodeColumn:         true,
		showWorkerDetail:       false,
		workerDetails:          nil,
		currentWorkerName:      "",
		currentWorkerOrigin:    "",
		inWorkerDetailAction:   false,
		width:                  80, // Default width, will be updated by WindowSizeMsg
		height:                 24, // Default height, will be updated by WindowSizeMsg
		workerDetailScrollY:    0,
		workerDetailMaxScroll:  0,
	}
	m.rebuildSwitcherItems()
	return m
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

	case serverSwitchMsg:
		cmd = m.handleServerSwitch(msg)
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
			m.currentWorkerOrigin = ""
			m.inWorkerDetailAction = false
			m.workerDetailScrollY = 0 // Reset scroll
			return m, tea.Batch(m.checkHealthCmd(), m.fetchWorkersCmd())
		case "a", "A":
			if m.currentWorkerName != "" {
				m.inWorkerDetailAction = !m.inWorkerDetailAction
			}
			return m, nil
		case "r":
			if m.currentWorkerName != "" {
				m.workerDetails = nil // Clear to show loading state
				m.workerDetailScrollY = 0
				m.workerDetailMaxScroll = 0
				return m, m.fetchWorkerDetailsCmd(m.currentWorkerName, m.currentWorkerOrigin)
			}
			return m, nil
		case "up", "k":
			if m.workerDetailScrollY > 0 {
				m.workerDetailScrollY--
			}
			return m, nil
		case "down", "j":
			if m.workerDetailScrollY < m.workerDetailMaxScroll {
				m.workerDetailScrollY++
			}
			return m, nil
		case "pgup", "b":
			m.workerDetailScrollY -= 10
			if m.workerDetailScrollY < 0 {
				m.workerDetailScrollY = 0
			}
			return m, nil
		case "pgdown", " ":
			m.workerDetailScrollY += 10
			if m.workerDetailScrollY > m.workerDetailMaxScroll {
				m.workerDetailScrollY = m.workerDetailMaxScroll
			}
			return m, nil
		case "home", "g":
			m.workerDetailScrollY = 0
			return m, nil
		case "end", "G":
			m.workerDetailScrollY = m.workerDetailMaxScroll
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
			return m, m.actionWorkerCmd(m.selectedWorkerName(), m.selectedWorkerOrigin(), "pause")
		case "r":
			m.inActionSelection = false
			return m, m.actionWorkerCmd(m.selectedWorkerName(), m.selectedWorkerOrigin(), "resume")
		case "s":
			m.inActionSelection = false
			return m, m.actionWorkerCmd(m.selectedWorkerName(), m.selectedWorkerOrigin(), "stop")
		case "R":
			m.inActionSelection = false
			return m, m.actionWorkerCmd(m.selectedWorkerName(), m.selectedWorkerOrigin(), "restart")
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
			return m, m.actionWorkerCmd(m.selectedWorkerName(), m.selectedWorkerOrigin(), "resume")
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
				origin := m.workerOrigins[workerName]
				m.showWorkerDetail = true
				m.workerDetails = nil            // Clear previous details
				m.currentWorkerName = workerName // Store for polling
				m.currentWorkerOrigin = origin   // Store for action routing
				return m, m.fetchWorkerDetailsCmd(workerName, origin)
			}
		}
		return m, nil
	case "esc":
		// Return to dashboard from detail view
		if m.showWorkerDetail {
			m.showWorkerDetail = false
			m.workerDetails = nil
			m.currentWorkerName = ""   // Clear worker name
			m.currentWorkerOrigin = "" // Clear origin
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
		if m.switcherExpandedServer != "" {
			m.switcherExpandedServer = ""
			m.rebuildSwitcherItems()
			return m, nil
		}
		m.showSwitcher = false
		return m, nil
	case "backspace", "h":
		if m.switcherExpandedServer != "" {
			m.switcherExpandedServer = ""
			m.rebuildSwitcherItems()
		}
		return m, nil
	case "enter":
		switch item := m.switcher.SelectedItem().(type) {
		case switchServerItem:
			if item.podCount <= 1 {
				// Single pod: select immediately
				m.showSwitcher = false
				m.resetPodView()
				return m.selectServer(item.name)
			}

			if m.switcherExpandedServer == item.name {
				// Already expanded: select merged view
				m.showSwitcher = false
				m.resetPodView()
				return m.selectServer(item.name)
			}

			// Expand this server
			m.switcherExpandedServer = item.name
			m.rebuildSwitcherItems()
			return m, nil

		case switchPodItem:
			m.showSwitcher = false
			m.err = nil
			m.workerOrigins = make(map[string]string)
			m.selectedPodOrigin = item.origin
			m.showNodeColumn = false
			m.rebuildTable()
			return m.selectServer(item.server)
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

// selectedWorkerOrigin returns the origin endpoint for the currently selected table row.
func (m Model) selectedWorkerOrigin() string {
	if len(m.table.Rows()) == 0 {
		return ""
	}
	name := m.table.SelectedRow()[0]
	return m.workerOrigins[name]
}

// selectedWorkerName returns the name of the currently selected table row.
func (m Model) selectedWorkerName() string {
	if len(m.table.Rows()) == 0 {
		return ""
	}
	return m.table.SelectedRow()[0]
}

// rebuildSwitcherItems reconstructs the switcher list. Servers are always shown.
// Pods are shown only under the currently expanded server.
func (m *Model) rebuildSwitcherItems() {
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

		expanded := m.switcherExpandedServer == name

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

	m.switcher.SetItems(items)
}

// resetPodView returns to the merged multi-pod view for a server.
func (m *Model) resetPodView() {
	m.err = nil
	m.workerOrigins = make(map[string]string)
	m.selectedPodOrigin = ""
	m.showNodeColumn = true
	m.rebuildTable()
}

// selectServer looks up the named server and returns a command that builds its
// pool in the background; the result arrives as a serverSwitchMsg.
func (m *Model) selectServer(serverName string) (tea.Model, tea.Cmd) {
	srv, err := m.cfg.GetActiveServer(serverName)
	if err != nil {
		m.err = err
		return m, nil
	}
	return m, m.switchServerCmd(srv)
}

// Close releases resources held by the model, i.e. the pool's discovery source
// (closing a Kubernetes source stops its informer).
func (m *Model) Close() error {
	if m.pool == nil {
		return nil
	}
	return m.pool.Close()
}
