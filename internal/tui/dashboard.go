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
	"github.com/goptics/vartop/internal/store"
)

// dashboardScreen is the worker list view and the base of the screen stack.
type dashboardScreen struct {
	table  table.Model
	labels []string // the label of each table row's unit, same order
	// trendW is the width of the table's Trend column, 0 when it has none:
	// the rows are built with a cell for it, or not.
	trendW int
	// kpiLine is the line under the header, built with the rows (not by
	// View); "" when there is nothing to chart.
	kpiLine string
	// trends are the series of the sparklines, built for trendsOf: again only
	// when a snapshot came, not at every sync or resize. built counts them.
	trends            trends
	trendsOf          trendsKey
	built             int
	inActionSelection bool
	keys              dashboardKeyMap
}

const (
	nameMin = 12 // the Name column is at least this wide
	cellPad = 2  // a table cell's padding, in all
	// trendMin and trendMax bound the Trend column: it is left out when
	// less than trendMin is spare.
	trendMin, trendMax = 8, 20
)

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

// fixedColumns returns the table columns of a fixed width, the Name column
// (which takes what is left over) and the Trend column (which takes a part of
// it) aside.
func fixedColumns(showNode bool) []table.Column {
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
	return cols
}

// columnsWidth is the width the columns take, cell padding and all, and the
// Name column's padding.
func columnsWidth(cols []table.Column) int {
	w := cellPad // the Name column's
	for _, c := range cols {
		w += c.Width + cellPad
	}
	return w
}

// trendWidth is the width of the Trend column in a table width columns wide:
// what is spare once the fixed columns have theirs, the Name column its
// minimum and the Trend column its padding, at most trendMax; none (0) when
// under trendMin is spare.
func trendWidth(width int, showNode bool) int {
	spare := width - columnsWidth(fixedColumns(showNode)) - nameMin - cellPad
	if spare < trendMin {
		return 0
	}
	return min(spare, trendMax)
}

// dashboardColumns returns the table columns; the name column takes whatever
// width the other columns and cell padding leave over.
func dashboardColumns(width int, showNode bool) []table.Column {
	cols := fixedColumns(showNode)
	if w := trendWidth(width, showNode); w > 0 {
		cols = append(cols, table.Column{Title: "Trend", Width: w})
	}
	return slices.Insert(cols, 0, table.Column{Title: "Name", Width: max(nameMin, width-columnsWidth(cols))})
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

	return &dashboardScreen{table: t, trendW: trendWidth(98, true), keys: newDashboardKeyMap()}
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

// selection is where the table's cursor is: its index, and the unit and
// worker of the row it is on.
type selection struct {
	cursor        int
	label, worker string
}

// selection returns where the cursor is now.
func (d *dashboardScreen) selection() selection {
	return selection{d.table.Cursor(), d.selectedLabel(), d.selectedWorkerName()}
}

// setRows replaces the table rows (and their units' labels), keeping the
// selection on the same unit and worker wherever its row moved: units
// coming and going before it must not slide another under the cursor. When
// that row is gone, the action menu closes — its next key would act on
// another unit — and the cursor stays where it was, as near as the rows
// allow.
func (d *dashboardScreen) setRows(rows []table.Row, labels []string, was selection) {
	d.labels = labels
	d.table.SetRows(rows)
	if was.worker != "" {
		for i, row := range rows {
			if labels[i] == was.label && row[0] == was.worker {
				d.table.SetCursor(i)
				return
			}
		}
		d.inActionSelection = false
	}
	d.table.SetCursor(min(max(was.cursor, 0), len(rows)-1))
}

// refresh builds the rows again from the newest snapshot of the server
// shown: one per unit × worker, units in the snapshot's order (or only the
// one selected), workers by name, the Node cell the unit's label. A unit
// that answered nothing has no rows: the header lists it. A worker its unit
// lists but did not answer for is not reached; a stale snapshot's values
// are not current: both show —.
//
// The sparklines are built here too: the Trend cell of each row, and the KPI
// line, from the feed's last 10 minutes of snapshots. The history stays
// drawn when the newest snapshot is stale.
func (d *dashboardScreen) refresh(m *Model) { d.rebuild(m, d.selection()) }

// rebuild is refresh, keeping the selection was.
func (d *dashboardScreen) rebuild(m *Model, was selection) {
	latest := m.latest()
	stale := m.stale(time.Now())
	pods := fleet.PodsOf(latest, m.selectedLabel)
	var units []string
	for _, p := range pods {
		if p.Err == "" {
			units = append(units, p.Label)
		}
	}
	var tr trends
	if m.feed != nil {
		// The series only change with a new snapshot (a reset feed's are new
		// too): a sync that brings none, or a resize, charts the same ones.
		if key := (trendsKey{m.feed, latest, m.selectedLabel}); key != d.trendsOf {
			d.trends, d.trendsOf = newTrends(m.feed.Snapshots(), m.selectedLabel, units, m.interval(), m.noun()), key
			d.built++
		}
		tr = d.trends
	}

	var rows []table.Row
	var labels []string
	for _, p := range pods {
		if p.Err != "" {
			continue
		}
		for _, name := range workersOf(p) {
			row := unitRow(p, name, stale)
			if m.showNodeColumn {
				row = slices.Insert(row, 1, center(12, p.Label))
			}
			if d.trendW > 0 {
				row = append(row, tr.worker(p.Label, name).spark(d.trendW))
			}
			rows = append(rows, row)
			labels = append(labels, p.Label)
		}
	}

	d.kpiLine = ""
	if text, _ := m.notice(); text == "" && latest != nil {
		d.kpiLine = kpiLine(m.width, tr.kpis, tr.at, stale)
	}
	d.setRows(rows, labels, was)
}

// unitWorker names a worker on a unit.
type unitWorker struct{ label, worker string }

// trendsKey says what a set of trends was built from: the feed, its newest
// snapshot, and the unit viewed ("" is every unit).
type trendsKey struct {
	feed   *store.Feed
	latest *fleet.Snapshot
	label  string
}

// trends are the series the dashboard's sparklines chart, of the last 10
// minutes of snapshots: the completed/s of each worker on each unit shown,
// and the KPIs of the view. fleet builds them, as it does the web charts';
// each series is of the last snapshots, one point to a snapshot, so the
// times are those of the last snapshots too.
type trends struct {
	at      []time.Time // when the snapshots of the series were taken, oldest first
	workers map[unitWorker][]fleet.Point
	kpis    fleet.KPIView
}

// newTrends builds the trends of snaps, the snapshots of a server (oldest
// first), for units, the labels of the units shown, in the view of the unit
// labelled label ("" is every unit).
func newTrends(snaps []*fleet.Snapshot, label string, units []string, interval time.Duration, n fleet.Noun) trends {
	tr := trends{workers: make(map[unitWorker][]fleet.Point)}
	if len(snaps) == 0 {
		return tr
	}
	for _, u := range units {
		v := fleet.BuildDashboard(snaps, u, interval, n)
		for _, r := range v.Rows {
			tr.workers[unitWorker{u, r.Name}] = r.Spark
		}
		if u == label {
			tr.kpis = v.KPIs
		}
	}
	if !slices.Contains(units, label) {
		// Every unit, or the one viewed though it answered nothing: its
		// history is still there to draw.
		tr.kpis = fleet.BuildDashboard(snaps, label, interval, n).KPIs
	}
	// Every series covers the same snapshots: those within the window of the
	// newest.
	last := snaps[len(snaps)-min(len(tr.kpis.Completed), len(snaps)):]
	tr.at = make([]time.Time, len(last))
	for i, s := range last {
		tr.at[i] = s.At
	}
	return tr
}

// worker is the completed/s series of worker on the unit labelled label.
func (tr trends) worker(label, worker string) series {
	return series{tr.at, tr.workers[unitWorker{label, worker}]}
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

// rebuildTable reconstructs the table with the current width and
// showNodeColumn setting, its rows and sparklines with it, keeping the
// selection: the columns, the Trend column among them, come and go with the
// width, and rows built for others would not fit.
func (d *dashboardScreen) rebuildTable(m *Model) {
	// The KPI line is a line of the screen too.
	tHeight := m.height - 13

	was := d.selection()
	d.trendW = trendWidth(m.width, m.showNodeColumn)
	t := table.New(
		table.WithColumns(dashboardColumns(m.width, m.showNodeColumn)),
		table.WithFocused(true),
	)
	if tHeight > 0 {
		t.SetHeight(tHeight)
	}
	t.SetStyles(TableStyles())

	d.table, d.labels = t, nil
	d.rebuild(m, was)
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
	parts := []string{header}
	if d.kpiLine != "" {
		parts = append(parts, d.kpiLine)
	}
	mainContent := lipgloss.JoinVertical(lipgloss.Left, append(parts, bodyView)...)

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
		lines = append(lines, wrap.Foreground(ColorWarning).Render(fleet.StaleMsg(m.key, latest, now, m.down, m.srvErr)))
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
		return fleet.NotConfiguredMsg(m.key), true
	case latest == nil:
		text = fleet.WaitingMsg(m.key)
		switch {
		case m.down:
			text += "\n" + daemonDownHint
		case m.srvErr != "":
			text += "\n" + m.key + ": " + m.srvErr
		}
		return text, false
	case m.selectedLabel != "" && len(fleet.PodsOf(latest, m.selectedLabel)) == 0:
		return fleet.GoneLabelMsg(m.selectedLabel, m.noun()), true
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
		lines = append(lines, fleet.SyncErrMsg(m.key, m.syncErr))
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
