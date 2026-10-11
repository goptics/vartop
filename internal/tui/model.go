package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/goptics/varmq/cmd/vartop/internal/api"
	"github.com/goptics/varmq/cmd/vartop/internal/config"
	"github.com/goptics/varmq/cmd/vartop/internal/discovery"
)

// Model is the root TUI state: shared state plus a stack of screens. The
// dashboard is always at the base of the stack; overlays (switcher, worker
// detail) are pushed on top of it. Each screen owns its own state and key
// handling and is given the root model to read and mutate shared state.
type Model struct {
	cfg    *config.Config
	server *config.Server
	pool   *api.Pool

	err        error
	sourceErr  bool // source/pool construction failed; keep err until pool replaced
	lastUpdate time.Time
	width      int
	height     int
	isHealthy  bool

	// Multi-endpoint state
	workerOrigins     map[string]string // workerName -> origin endpoint (for action routing)
	podHealth         map[string]bool   // per-endpoint health status
	selectedPodOrigin string            // empty = merged view; non-empty = single pod view
	showNodeColumn    bool              // false when viewing a single pod (Node column hidden)

	screens  []screen
	switcher *switcherScreen
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

	m := &Model{
		cfg:               cfg,
		server:            server,
		pool:              pool,
		err:               err,
		sourceErr:         err != nil,
		isHealthy:         true, // Assume healthy initially
		workerOrigins:     make(map[string]string),
		podHealth:         make(map[string]bool),
		selectedPodOrigin: "",
		showNodeColumn:    true,
		width:             80, // Default width, will be updated by WindowSizeMsg
		height:            24, // Default height, will be updated by WindowSizeMsg
	}

	// The dashboard is the base of the stack; the switcher is a singleton so
	// its expanded-server state persists across open/close.
	m.screens = []screen{newDashboardScreen()}
	m.switcher = newSwitcherScreen()
	m.switcher.rebuild(m)

	return m
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.checkHealthCmd(),
		m.tickCmd(),
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Key messages are handled entirely by the active screen, which also
	// forwards navigation keys to its embedded component (table/list).
	if key, ok := msg.(tea.KeyMsg); ok {
		return m, m.activeScreen().Update(&m, key)
	}

	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.handleWindowSize(msg)

	case tickMsg:
		cmds = append(cmds, m.tickCmd())
		if !m.isSwitcherActive() {
			cmds = append(cmds, m.checkHealthCmd())
		}

	case healthMsg:
		cmds = append(cmds, m.handleHealth(msg))

	case workersMsg:
		m.handleWorkers(msg)

	case workerDetailsMsg:
		cmds = append(cmds, m.handleWorkerDetails(msg))

	case errMsg:
		cmds = append(cmds, m.handleError(msg))

	case serverSwitchMsg:
		cmds = append(cmds, m.handleServerSwitch(msg))
	}

	// Non-key messages are forwarded to the active screen's embedded
	// component, matching the old per-view update block.
	cmds = append(cmds, m.activeScreen().Update(&m, msg))

	return m, tea.Batch(cmds...)
}

func (m Model) View() string {
	return m.activeScreen().View(&m)
}

// selectServer looks up the named server and returns a command that builds its
// pool in the background; the result arrives as a serverSwitchMsg.
func (m *Model) selectServer(serverName string) tea.Cmd {
	srv, err := m.cfg.GetActiveServer(serverName)
	if err != nil {
		m.err = err
		return nil
	}
	return m.switchServerCmd(srv)
}

// resetPodView returns to the merged multi-pod view for a server.
func (m *Model) resetPodView() {
	m.err = nil
	m.workerOrigins = make(map[string]string)
	m.selectedPodOrigin = ""
	m.showNodeColumn = true
	m.dashboardScreen().rebuildTable(m)
}

// Close releases resources held by the model, i.e. the pool's discovery source
// (closing a Kubernetes source stops its informer).
func (m *Model) Close() error {
	if m.pool == nil {
		return nil
	}
	return m.pool.Close()
}
