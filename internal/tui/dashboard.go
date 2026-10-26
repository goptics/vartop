package tui

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/goptics/vartop/internal/fleet"
)

// dashboardScreen is the worker list view and the base of the screen stack.
type dashboardScreen struct {
	table             table.Model
	labels            []string // the label of each table row's unit, same order
	inActionSelection bool
	keys              dashboardKeyMap
}

// center renders v centred in a cell of the given width. A value as wide as
// the cell or wider is left as it is, for the table to truncate: centring
// would wrap it onto a second line.
func center(width int, v any) string {
	s := fmt.Sprint(v)
	if lipgloss.Width(s) >= width {
		return s
	}
	return lipgloss.NewStyle().Width(width).Align(lipgloss.Center).Render(s)
}

// dashboardColumns returns the table columns; the name column takes whatever
// width the fixed columns and cell padding leave over.
func dashboardColumns(width int, showNode bool) []table.Column {
	cols := []table.Column{
		{Title: "Status", Width: 12},
		{Title: center(12, "Concurrency"), Width: 12},
		{Title: center(12, "Processing"), Width: 12},
		{Title: center(10, "Pending"), Width: 10},
		{Title: center(8, "Idle"), Width: 8},
	}
	if showNode {
		cols = slices.Insert(cols, 0, table.Column{Title: "Node", Width: 12})
	}
	// fixed widths plus cell padding (2 per column, name included)
	fixed := 2 * (len(cols) + 1)
	for _, c := range cols {
		fixed += c.Width
	}
	return slices.Insert(cols, 0, table.Column{Title: "Name", Width: max(12, width-fixed)})
}

func newDashboardScreen() *dashboardScreen {
	// Width 98 gives the name column its initial 18 until the first resize.
	columns := dashboardColumns(98, true)

	t := table.New(
		table.WithColumns(columns),
		table.WithFocused(true),
		table.WithHeight(6), // Reduced from 10 to leave more room for header
	)
	t.SetStyles(TableStyles())

	return &dashboardScreen{table: t, keys: newDashboardKeyMap()}
}

func (d *dashboardScreen) Update(m *Model, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		d.table, cmd = d.table.Update(msg)
		return cmd
	}
	return d.handleKey(m, keyMsg)
}

func (d *dashboardScreen) handleKey(m *Model, msg tea.KeyMsg) tea.Cmd {
	if d.inActionSelection {
		switch {
		case key.Matches(msg, d.keys.Back):
			d.inActionSelection = false
			return nil
		case key.Matches(msg, d.keys.Pause):
			d.inActionSelection = false
			return m.actCmd(d.selectedWorkerName(), d.selectedLabel(), "pause")
		case key.Matches(msg, d.keys.Resume):
			d.inActionSelection = false
			return m.actCmd(d.selectedWorkerName(), d.selectedLabel(), "resume")
		case key.Matches(msg, d.keys.Stop):
			d.inActionSelection = false
			return m.actCmd(d.selectedWorkerName(), d.selectedLabel(), "stop")
		case key.Matches(msg, d.keys.Restart):
			d.inActionSelection = false
			return m.actCmd(d.selectedWorkerName(), d.selectedLabel(), "restart")
		}
		// Unhandled keys fall through so navigation still works while the
		// action menu is open.
	}

	switch {
	case key.Matches(msg, d.keys.Quit):
		return tea.Quit
	case key.Matches(msg, d.keys.Refresh):
		return m.refreshCmd()
	case key.Matches(msg, d.keys.Actions):
		if len(d.table.Rows()) > 0 {
			d.inActionSelection = !d.inActionSelection
		}
		return nil
	case key.Matches(msg, d.keys.Switch):
		m.pushScreen(m.switcher)
		return m.switcherCmd()
	case key.Matches(msg, d.keys.Detail):
		// Navigate to the selected row's worker on its unit
		if name := d.selectedWorkerName(); name != "" {
			m.pushScreen(newDetailScreen(name, d.selectedLabel()))
		}
		return nil
	}

	if len(d.table.Rows()) == 0 {
		return nil
	}

	var cmd tea.Cmd
	d.table, cmd = d.table.Update(msg)
	return cmd
}

// selectedWorkerName returns the name of the currently selected table row.
func (d *dashboardScreen) selectedWorkerName() string {
	row := d.table.SelectedRow()
	if row == nil {
		return ""
	}
	return row[0]
}

// selectedLabel returns the label of the currently selected table row's
// unit.
func (d *dashboardScreen) selectedLabel() string {
	i := d.table.Cursor()
	if i < 0 || i >= len(d.labels) {
		return ""
	}
	return d.labels[i]
}

// setRows replaces the table rows (and their units' labels) while retaining
// the current selection.
func (d *dashboardScreen) setRows(rows []table.Row, labels []string) {
	selected := d.table.Cursor()
	d.labels = labels
	d.table.SetRows(rows)
	if selected < len(rows) {
		d.table.SetCursor(selected)
	}
}

// refresh builds the rows again from the newest snapshot of the server
// shown: one per unit × worker, units in the snapshot's order (or only the
// one selected), workers by name, the Node cell the unit's label. A unit
// that answered nothing has no rows: the header lists it. A worker its unit
// lists but did not answer for is not reached; a stale snapshot's values
// are not current: both show —.
func (d *dashboardScreen) refresh(m *Model) {
	latest := m.latest()
	stale := m.stale(time.Now())
	var rows []table.Row
	var labels []string
	for _, p := range fleet.PodsOf(latest, m.selectedLabel) {
		if p.Err != "" {
			continue
		}
		for _, name := range workersOf(p) {
			row := unitRow(p, name, stale)
			if m.showNodeColumn {
				row = slices.Insert(row, 1, center(12, p.Label))
			}
			rows = append(rows, row)
			labels = append(labels, p.Label)
		}
	}
	d.setRows(rows, labels)
}

// workersOf is the names of the workers p lists, answered or not, sorted.
func workersOf(p fleet.PodSnapshot) []string {
	names := append(slices.Collect(maps.Keys(p.Workers)), p.Unreached...)
	slices.Sort(names)
	return slices.Compact(names)
}

// unitRow is the row of worker name on unit p, Node cell aside.
func unitRow(p fleet.PodSnapshot, name string, stale bool) table.Row {
	sm := p.Workers[name]
	switch {
	case stale:
		return table.Row{name, "—", center(12, "—"), center(12, "—"), center(10, "—"), center(8, "—")}
	case sm == nil:
		return table.Row{name, "not reached", center(12, "—"), center(12, "—"), center(10, "—"), center(8, "—")}
	}
	return table.Row{
		name,
		sm.Status,
		center(12, sm.Concurrency),
		center(12, sm.NumProcessing),
		center(10, sm.NumPending),
		center(8, sm.NumIdleWorkers),
	}
}

// rebuildTable reconstructs the table with the current width and showNodeColumn setting.
func (d *dashboardScreen) rebuildTable(m *Model) {
	tHeight := m.height - 12

	columns := dashboardColumns(m.width, m.showNodeColumn)

	s := TableStyles()
	selected := d.table.Cursor()
	rows := d.table.Rows()

	// If existing rows have a different cell count than new columns,
	// clear them to avoid a panic in table.New (rows will be repopulated by the next sync).
	if len(rows) > 0 && len(rows[0]) != len(columns) {
		rows = nil
		d.labels = nil
		selected = 0
	}

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

	d.table = t
}

// View renders the dashboard.
func (d *dashboardScreen) View(m *Model) string {
	// Header
	header := d.renderHeader(m)

	// Body
	bodyView := d.renderBody(m)

	// Footer
	footer := d.renderFooter(m)

	// Build main content - reduced gap
	mainContent := lipgloss.JoinVertical(lipgloss.Left, header, bodyView)

	// Calculate spacing for footer positioning
	footerHeight := lipgloss.Height(footer)
	bodyHeight := lipgloss.Height(mainContent)
	spacing := createSpacing(m.height, bodyHeight, footerHeight, 0)

	// Create full view with footer anchored at bottom
	fullContent := lipgloss.JoinVertical(
		lipgloss.Left,
		mainContent,
		strings.Repeat("\n", spacing),
		footer,
	)

	return BaseStyle.Render(fullContent)
}

// renderHeader renders the dashboard status bar: server name, unit health,
// what it reaches (kube context/namespace, dns host, endpoint, or the unit
// shown), the balancers' instance counts and when the newest snapshot was
// taken; under it, when that snapshot is stale, how old and why, and the
// units that answered nothing.
func (d *dashboardScreen) renderHeader(m *Model) string {
	now := time.Now()
	latest := m.latest()
	stale := m.stale(now)
	subtle := lipgloss.NewStyle().Foreground(ColorSubtle)

	title := TitleStyle.Render(fmt.Sprintf(" vartop [%s] ", m.key))
	parts := []string{title}
	if m.server() != nil {
		parts = append(parts, renderHealth(latest, stale, m.noun()), "  ", d.renderConnection(m))
	}
	if note := fleet.BalancerNote(latest); note != "" {
		parts = append(parts, "  ", subtle.Render(note))
	}
	leftContent := lipgloss.JoinHorizontal(lipgloss.Left, parts...)

	// When the newest snapshot was taken, top-right
	var updatedInfo string
	if latest != nil {
		style := subtle
		if stale {
			style = lipgloss.NewStyle().Foreground(ColorWarning)
		}
		updatedInfo = style.Render("updated " + fleet.PointTime(latest))
	}

	// Proper justification: calculate spacing between left and right
	leftWidth := lipgloss.Width(leftContent)
	rightWidth := lipgloss.Width(updatedInfo)
	spacing := m.width - leftWidth - rightWidth - 4 // -4 for padding

	lines := []string{lipgloss.JoinHorizontal(lipgloss.Left,
		leftContent,
		strings.Repeat(" ", max(0, spacing)),
		updatedInfo,
	)}
	wrap := lipgloss.NewStyle().Width(max(1, m.width-4))
	if stale {
		lines = append(lines, wrap.Foreground(ColorWarning).Render(staleMsg(m.key, latest, now, m.down, m.srvErr)))
	}
	if down := unitErrors(latest, m.selectedLabel); down != "" {
		lines = append(lines, wrap.Foreground(ColorError).Render(down))
	}

	return StatusBarStyle.Render(lipgloss.JoinVertical(lipgloss.Left, lines...))
}

// renderHealth is the health of latest's units: healthy of all, with a
// dot, or — when latest is stale: its health is not current.
func renderHealth(latest *fleet.Snapshot, stale bool, n fleet.Noun) string {
	subtle := lipgloss.NewStyle().Foreground(ColorSubtle)
	if stale {
		return subtle.Render("— " + n.Many)
	}
	h := fleet.Health(latest)
	dot := StatusStyleStopped.Render("○")
	switch h.Dot {
	case "●":
		dot = StatusStyleRunning.Render("●")
	case "◐":
		dot = StatusStyleWarning.Render("◐")
	}
	return lipgloss.JoinHorizontal(lipgloss.Left, dot, " ", subtle.Render(fmt.Sprintf("%d/%d %s", h.Healthy, h.Total, n.Many)))
}

// unitErrors lists the units of latest (or only the one labelled label)
// that answered nothing, each "{label}: {error}".
func unitErrors(latest *fleet.Snapshot, label string) string {
	var down []string
	for _, p := range fleet.PodsOf(latest, label) {
		if p.Err != "" {
			down = append(down, p.Label+": "+p.Err)
		}
	}
	return strings.Join(down, " · ")
}

// renderConnection describes how the active server is reached: kube
// context/namespace, dns host, or an endpoint.
func (d *dashboardScreen) renderConnection(m *Model) string {
	return lipgloss.NewStyle().Foreground(ColorSubtle).Render("→ " + m.connectionLabel())
}

// connectionLabel names what the dashboard shows: the unit selected, else
// how the server is reached.
func (m *Model) connectionLabel() string {
	srv := m.server()
	switch {
	case m.selectedLabel != "":
		return m.selectedLabel
	case srv == nil:
		return ""
	case srv.Kubernetes != nil:
		return fmt.Sprintf("%s/%s", srv.Kubernetes.Context, srv.Kubernetes.Namespace)
	case srv.DNS != nil:
		return fmt.Sprintf("dns:%s:%d", srv.DNS.Host, srv.DNS.Port)
	default:
		return srv.DisplayEndpoint()
	}
}

// daemonDownHint is the waiting notice's second line when no daemon runs.
const daemonDownHint = "the vartop daemon is not running — press r to start it"

// notice is what the body shows instead of the table, if anything: the
// server is not configured (bad), has no snapshot yet, or the unit selected
// is no longer reported (bad).
func (m *Model) notice() (text string, bad bool) {
	latest := m.latest()
	switch {
	case m.server() == nil:
		return notConfiguredMsg(m.key), true
	case latest == nil:
		text = waitingMsg(m.key)
		switch {
		case m.down:
			text += "\n" + daemonDownHint
		case m.srvErr != "":
			text += "\n" + m.key + ": " + m.srvErr
		}
		return text, false
	case m.selectedLabel != "" && len(fleet.PodsOf(latest, m.selectedLabel)) == 0:
		return goneLabelMsg(m.selectedLabel, m.noun()), true
	}
	return "", false
}

// errorLines is what the error line shows: the feed could not be read, the
// server could not be polled (its newest snapshot's error, when fresh: a
// stale one's is in the header), and what the last refresh or action said.
// None for a server that is not configured.
func (m *Model) errorLines() []string {
	if m.server() == nil {
		return nil
	}
	var lines []string
	if m.syncErr != nil {
		lines = append(lines, syncErrMsg(m.key, m.syncErr))
	}
	if latest := m.latest(); latest != nil && latest.Err != "" && !m.stale(time.Now()) {
		lines = append(lines, m.key+": "+latest.Err)
	}
	if m.err != nil {
		lines = append(lines, m.err.Error())
	}
	return lines
}

// renderErrors renders the error lines, wrapped to the screen's width.
func renderErrors(m *Model) string {
	var view string
	style := lipgloss.NewStyle().Foreground(ColorError).Padding(0, 1).Width(max(1, m.width-2))
	for _, line := range m.errorLines() {
		view += style.Render("⚠️  "+line) + "\n"
	}
	return view
}

// renderBody renders the dashboard body content.
func (d *dashboardScreen) renderBody(m *Model) string {
	errorView := renderErrors(m)
	if text, bad := m.notice(); text != "" {
		box := lipgloss.NewStyle().
			Foreground(ColorSubtle).
			Padding(2, 2).
			Border(lipgloss.RoundedBorder())
		if bad {
			box = box.BorderForeground(ColorError)
		}
		notice := lipgloss.Place(m.width, d.table.Height()+2, lipgloss.Center, lipgloss.Center, box.Render(text))
		return lipgloss.JoinVertical(lipgloss.Left, errorView, notice)
	}

	tableView := lipgloss.NewStyle().Padding(1, 1).Render(d.table.View())
	return lipgloss.JoinVertical(lipgloss.Left, errorView, tableView)
}

// renderFooter renders the dashboard footer.
func (d *dashboardScreen) renderFooter(m *Model) string {
	// How often the daemon polls the server (the updated time is in the status bar)
	pollInfo := fmt.Sprintf("Polling: %ds", int(m.interval()/time.Second))

	keysView := helpFooter(d.keyMap(), m.width-lipgloss.Width(pollInfo)-4)

	// Create footer layout
	leftCol := lipgloss.NewStyle().Width(m.width - lipgloss.Width(pollInfo) - 2).Render(keysView)
	footerContent := lipgloss.JoinHorizontal(lipgloss.Right, leftCol, pollInfo)

	return FooterStyle.Width(m.width).Padding(0, 1).Render(footerContent)
}
