package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/store"
)

// detailScreen is the worker detail overlay: one worker on one unit, named
// by its label. It shows the worker's sample in the newest snapshot of the
// server shown, and charts of its last hour (see detailSeries), and owns the
// scroll/action state.
type detailScreen struct {
	workerName string
	label      string // the unit's

	inAction bool // action mode
	vp       viewport.Model

	// series are those of the charts, built by the sync command (see detailFor)
	// and taken in by install; drawn is what they were last drawn as.
	series detailSeries
	drawn  drawnCharts

	keys detailKeyMap
}

func newDetailScreen(workerName, label string) *detailScreen {
	return &detailScreen{workerName: workerName, label: label, keys: newDetailKeyMap()}
}

// sample is the worker's sample on the screen's unit in latest, with the
// unit; or, when there is none, why: the unit answered nothing (its
// error), did not answer for the worker, lacks it, or is no longer
// reported.
func (d *detailScreen) sample(latest *fleet.Snapshot) (sm *fleet.Sample, u fleet.PodSnapshot, missing string) {
	units := fleet.PodsOf(latest, d.label)
	if len(units) == 0 {
		return nil, u, "no longer reported"
	}
	u = units[0]
	switch sm = u.Workers[d.workerName]; {
	case u.Err != "":
		return nil, u, u.Err
	case sm != nil:
		return sm, u, ""
	case u.IsUnreached(d.workerName):
		return nil, u, "not reached"
	}
	return nil, u, "no worker " + d.workerName
}

func (d *detailScreen) Update(m *Model, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	if d.inAction {
		return d.handleActionKey(m, keyMsg)
	}
	return d.handleKey(m, keyMsg)
}

// handleActionKey processes keys while the action menu is open.
func (d *detailScreen) handleActionKey(m *Model, msg tea.KeyMsg) tea.Cmd {
	switch {
	case key.Matches(msg, d.keys.Quit):
		return tea.Quit
	case key.Matches(msg, d.keys.Back):
		d.inAction = false
	case key.Matches(msg, d.keys.Pause):
		d.inAction = false
		return m.actCmd(d.workerName, d.label, "pause")
	case key.Matches(msg, d.keys.Resume):
		d.inAction = false
		return m.actCmd(d.workerName, d.label, "resume")
	case key.Matches(msg, d.keys.Stop):
		d.inAction = false
		return m.actCmd(d.workerName, d.label, "stop")
	case key.Matches(msg, d.keys.Restart):
		d.inAction = false
		return m.actCmd(d.workerName, d.label, "restart")
	}
	return nil
}

// handleKey processes keys in the normal (non-action) mode.
func (d *detailScreen) handleKey(m *Model, msg tea.KeyMsg) tea.Cmd {
	switch {
	case key.Matches(msg, d.keys.Quit):
		return tea.Quit
	case key.Matches(msg, d.keys.Back):
		m.popScreen()
		d.workerName = ""
		d.label = ""
		d.inAction = false
		d.vp.GotoTop()
	case key.Matches(msg, d.keys.Actions):
		if d.workerName != "" {
			d.inAction = !d.inAction
		}
	case key.Matches(msg, d.keys.Refresh):
		return m.refreshCmd()
	case key.Matches(msg, d.keys.Up):
		d.vp.LineUp(1)
	case key.Matches(msg, d.keys.Down):
		d.vp.LineDown(1)
	case key.Matches(msg, d.keys.PageUp):
		d.vp.PageUp()
	case key.Matches(msg, d.keys.PageDown):
		d.vp.PageDown()
	case key.Matches(msg, d.keys.Home):
		d.vp.GotoTop()
	case key.Matches(msg, d.keys.End):
		d.vp.GotoBottom()
	}
	return nil
}

// View renders the complete worker detail view.
func (d *detailScreen) View(m *Model) string {
	footer := d.renderFooter(m, d.vp.YOffset)
	footerHeight := lipgloss.Height(footer)

	latest := m.latest()
	sm, u, missing := d.sample(latest)
	if text, bad := d.notice(m, latest, missing); text != "" {
		box := lipgloss.NewStyle().
			Foreground(ColorSubtle).
			Padding(2, 2).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorHighlight)
		if bad {
			box = box.BorderForeground(ColorError)
		}

		bodyView := lipgloss.Place(m.width, m.height-footerHeight-2, lipgloss.Center, lipgloss.Center, box.Render(text))
		spacing := createSpacing(m.height, lipgloss.Height(bodyView), footerHeight, 0)
		fullContent := lipgloss.JoinVertical(lipgloss.Left, bodyView, strings.Repeat("\n", spacing), footer)
		return BaseStyle.Render(fullContent)
	}

	w := sm.WorkerDetails
	stale := m.stale(time.Now())

	// Header
	header := d.renderHeader(m, latest, u, stale)

	// Content sections
	configSection := d.renderModernConfigSection(w, stale)
	metricsSection := d.renderModernMetricsSection(w, stale)
	charts := d.renderCharts(m, stale)

	// Responsive layout
	minWidthForHorizontal := 100
	var bodyContent string

	if m.width >= minWidthForHorizontal {
		// Side-by-side layout
		halfWidth := (m.width - 8) / 2
		configSection = lipgloss.NewStyle().Width(halfWidth).Render(configSection)
		metricsSection = lipgloss.NewStyle().Width(halfWidth).Render(metricsSection)
		bodyContent = lipgloss.JoinHorizontal(lipgloss.Top, configSection, "", metricsSection)
	} else {
		// Vertical layout
		bodyContent = lipgloss.JoinVertical(lipgloss.Left, configSection, "", metricsSection)
	}
	if charts != "" { // under the metrics, as wide as the screen has room for
		bodyContent = lipgloss.JoinVertical(lipgloss.Left, bodyContent, charts)
	}

	bodyContent = lipgloss.NewStyle().Padding(0, 1).Render(bodyContent)
	fullBodyContent := lipgloss.JoinVertical(lipgloss.Left, "", header, renderErrors(m), bodyContent)

	// The scroll indicator can wrap the footer onto another line, so size the
	// viewport for that case to keep its height (and the scroll limits) stable.
	footerHeight = max(footerHeight, lipgloss.Height(d.renderFooter(m, 1)))
	d.vp.Width, d.vp.Height = m.width, m.height-footerHeight-2
	d.vp.SetContent(fullBodyContent)
	d.vp.SetYOffset(d.vp.YOffset) // re-clamp after a resize
	footer = d.renderFooter(m, d.vp.YOffset)
	footerHeight = lipgloss.Height(footer)
	visibleContent := d.vp.View()

	// Anchor footer at bottom with proper spacing
	bodyViewHeight := lipgloss.Height(visibleContent)
	spacing := createSpacing(m.height, bodyViewHeight, footerHeight, 0)
	fullContent := lipgloss.JoinVertical(lipgloss.Left, visibleContent, strings.Repeat("\n", spacing), footer)

	return BaseStyle.Render(fullContent)
}

// notice is what the screen shows instead of the worker's values, if
// anything: what the dashboard would (the server is not configured, or has
// no snapshot yet, and why), or that the unit has no sample of the worker
// ("{label}: {why}", bad), with the error lines above.
func (d *detailScreen) notice(m *Model, latest *fleet.Snapshot, missing string) (text string, bad bool) {
	switch {
	case m.server() == nil:
		return m.notice()
	case latest == nil:
		text, bad = m.notice()
	case missing != "":
		text, bad = d.label+": "+missing, true
	default:
		return "", false
	}
	if errs := m.errorLines(); len(errs) > 0 {
		text = strings.Join(errs, "\n") + "\n\n" + text
	}
	return text, bad
}

// renderHeader renders the worker detail view header: the worker, its
// server and unit (label, and the instance and address behind it), and when
// the snapshot shown was taken.
func (d *detailScreen) renderHeader(m *Model, latest *fleet.Snapshot, u fleet.PodSnapshot, stale bool) string {
	updatedStyle := lipgloss.NewStyle().Foreground(ColorSubtle)
	if stale {
		updatedStyle = lipgloss.NewStyle().Foreground(ColorWarning)
	}
	updatedInfo := updatedStyle.Render("updated " + fleet.PointTime(latest))
	var staleLine string
	if stale {
		// Wrapped to the screen: the compact header is narrow.
		staleLine = lipgloss.NewStyle().Foreground(ColorWarning).Width(max(1, m.width-8)).
			Render(fleet.StaleMsg(m.key, latest, time.Now(), m.down, m.srvErr))
	}
	unitInfo := u.Label
	if tip := u.Tip(); tip != "" && tip != u.Label {
		unitInfo += " (" + tip + ")"
	}

	// Responsive header layout
	minWidthForCompact := 80
	if m.width < minWidthForCompact {
		// Compact layout for narrow screens - stack vertically
		title := lipgloss.NewStyle().
			Foreground(ColorHighlight).
			Bold(true).
			Render("Worker Details")

		workerInfo := lipgloss.NewStyle().
			Foreground(ColorFg).
			Bold(true).
			Render(d.workerName)

		serverInfo := fmt.Sprintf("Server: %s", m.key)
		endpointInfo := fmt.Sprintf("→ %s", unitInfo)

		rows := []string{
			title,
			workerInfo,
			lipgloss.NewStyle().Foreground(ColorSubtle).Render(serverInfo),
			lipgloss.NewStyle().Foreground(ColorSubtle).Render(endpointInfo),
			updatedInfo,
		}
		if stale {
			rows = append(rows, staleLine)
		}
		headerContent := lipgloss.JoinVertical(lipgloss.Left, rows...)

		return lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorHighlight).
			Padding(1, 1).
			Render(headerContent)
	}

	// Normal layout for wider screens
	title := lipgloss.NewStyle().
		Foreground(ColorHighlight).
		Bold(true).
		Render("Worker Details")

	workerInfo := lipgloss.NewStyle().
		Foreground(ColorFg).
		Bold(true).
		Render(d.workerName)

	// Server and unit info
	serverLabel := lipgloss.NewStyle().Foreground(ColorSubtle).Render("Server:")
	serverValue := lipgloss.NewStyle().Foreground(ColorFg).Render(m.key)
	unitLabel := lipgloss.NewStyle().Foreground(ColorSubtle).Render(m.noun().Cap() + ":")
	unitValue := lipgloss.NewStyle().Foreground(ColorSubtle).Render(unitInfo)

	// Build header rows
	row1 := lipgloss.JoinHorizontal(lipgloss.Left, title, " • ", workerInfo)
	row2Left := lipgloss.JoinHorizontal(lipgloss.Left,
		serverLabel, " ", serverValue, "  |  ",
		unitLabel, " ", unitValue,
	)
	separator := lipgloss.NewStyle().Foreground(ColorSubtle).Render(" | ")
	row2 := lipgloss.JoinHorizontal(lipgloss.Left, row2Left, separator, updatedInfo)

	rows := []string{row1, row2}
	if stale {
		rows = append(rows, staleLine)
	}
	headerContent := lipgloss.JoinVertical(lipgloss.Left, rows...)

	// Worker detail keeps border for visual separation
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorHighlight).
		Padding(1, 2).
		Render(headerContent)
}

// renderFooter renders the worker detail view footer.
func (d *detailScreen) renderFooter(m *Model, scrollY int) string {
	// How often the daemon polls the server (the updated time is in the header)
	interval := int(m.interval() / time.Second)

	// Add scroll indicator if scrolling is active
	pollInfo := fmt.Sprintf("Polling: %ds", interval)
	if scrollY > 0 {
		pollInfo = fmt.Sprintf("↓ %d | Polling: %ds", scrollY, interval)
	}

	keysView := helpFooter(d.keyMap(), m.width-lipgloss.Width(pollInfo)-4)

	// Create footer layout
	leftCol := lipgloss.NewStyle().Width(m.width - lipgloss.Width(pollInfo) - 2).Render(keysView)
	footerContent := lipgloss.JoinHorizontal(lipgloss.Right, leftCol, pollInfo)

	return FooterStyle.Width(m.width).Padding(0, 1).Render(footerContent)
}

// renderModernConfigSection renders the configuration section with modern styling
func (d *detailScreen) renderModernConfigSection(w api.WorkerDetails, stale bool) string {
	title := lipgloss.NewStyle().
		Foreground(ColorHighlight).
		Bold(true).
		Render("⚙️  Configuration")

	// Status indicator with color
	statusStyle := getStatusStyle(w.Status)
	statusDot := statusStyle.Render("●")
	statusText := statusStyle.Render(w.Status)

	// Build rows with proper spacing
	rows := []string{
		buildDataRow("Status", lipgloss.JoinHorizontal(lipgloss.Left, statusDot, " ", statusText)),
		buildDataRow("Concurrency", fmt.Sprintf("%d workers", w.Concurrency)),
		buildDataRow("Processing", fmt.Sprintf("%d jobs", w.NumProcessing)),
		buildDataRow("Pending", fmt.Sprintf("%d jobs", w.NumPending)),
		buildDataRow("Idle Workers", fmt.Sprintf("%d available", w.NumIdleWorkers)),
	}
	if stale { // not current: none shown
		for i, label := range []string{"Status", "Concurrency", "Processing", "Pending", "Idle Workers"} {
			rows[i] = buildDataRow(label, "—")
		}
	}

	// Add separator
	separator := lipgloss.NewStyle().
		Foreground(ColorSubtle).
		Render(strings.Repeat("─", 30))

	content := lipgloss.JoinVertical(lipgloss.Left,
		"",
		rows[0], separator, rows[1], rows[2], rows[3], rows[4],
	)

	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(ColorHighlight).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left, title, content))
}

// renderModernMetricsSection renders the performance metrics section with modern styling
func (d *detailScreen) renderModernMetricsSection(w api.WorkerDetails, stale bool) string {
	title := lipgloss.NewStyle().
		Foreground(ColorHighlight).
		Bold(true).
		Render("📊 Performance Metrics")

	// Calculate success rate based on completed attempts (successful + failed)
	var successRate float64
	totalAttempts := w.Metrics.Successful + w.Metrics.Failed
	if totalAttempts > 0 {
		successRate = float64(w.Metrics.Successful) / float64(totalAttempts) * 100
	}

	// Build metric rows with icons and colors
	metricsData := []struct {
		label string
		value string
		color lipgloss.Color
		icon  string
	}{
		{"Submitted", fmt.Sprintf("%d", w.Metrics.Submitted), ColorHighlight, "📤"},
		{"Completed", fmt.Sprintf("%d", w.Metrics.Completed), ColorSuccess, "✅"},
		{"Successful", fmt.Sprintf("%d", w.Metrics.Successful), ColorSuccess, "✓"},
		{"Failed", fmt.Sprintf("%d", w.Metrics.Failed), ColorError, "✗"},
		{"Success Rate", fmt.Sprintf("%.1f%%", successRate), ColorWarning, "📈"},
	}

	var rows []string
	for _, metric := range metricsData {
		icon := lipgloss.NewStyle().Foreground(metric.color).Render(metric.icon)
		value := lipgloss.NewStyle().
			Foreground(metric.color).
			Bold(true).
			Render(metric.value)

		if stale { // not current
			value = "—"
		}
		row := buildDataRow(metric.label, lipgloss.JoinHorizontal(lipgloss.Left, icon, " ", value))
		rows = append(rows, row)
	}

	// Add separator
	separator := lipgloss.NewStyle().
		Foreground(ColorSubtle).
		Render(strings.Repeat("─", 30))

	content := lipgloss.JoinVertical(lipgloss.Left,
		"",
		rows[0], separator, rows[1], rows[2], rows[3], rows[4],
	)

	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(ColorHighlight).
		Padding(1, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left, title, content))
}

// detailRows is the height of each of the detail screen's charts, its axis
// aside; chartMargin how much narrower than the screen they are: the body's
// padding, and as much again. Under blockMin columns there are none.
const (
	detailRows  = 5
	chartMargin = 4
)

// detailKey says what a detail screen's series were built from: the feed, its
// newest snapshot, and the unit and worker the screen shows.
type detailKey struct {
	feed          *store.Feed
	latest        *fleet.Snapshot
	label, worker string
}

// detailSeries is what the detail screen's charts draw, of the hour of
// snapshots a feed holds: the completed/s and failed/s of its worker on its
// unit, one point to a snapshot, so the times are those of the snapshots.
// The zero value has none.
type detailSeries struct {
	key               detailKey
	at                []time.Time // when the snapshots of the series were taken, oldest first
	completed, failed []fleet.Point
}

// newDetailSeries builds the series of snaps, the snapshots of a server
// (oldest first), for the worker on the unit labelled label. fleet builds
// them, as it does the web charts', without their tooltips: nothing here
// draws one.
func newDetailSeries(snaps []*fleet.Snapshot, label, worker string) detailSeries {
	s := detailSeries{
		at:        make([]time.Time, len(snaps)),
		completed: fleet.CompletedRates1h(snaps, label, worker),
		failed:    fleet.FailedRates1h(snaps, label, worker),
	}
	for i, snap := range snaps {
		s.at[i] = snap.At
	}
	return s
}

// install takes in series built by a sync command, if they are of what the
// screen shows now: its feed, its newest snapshot, its unit and worker. Those
// of another unit, another feed or an older snapshot — a sync that was under
// way when the view changed, or another sync's — are left out, and the ones
// the screen has stay.
func (d *detailScreen) install(m *Model, s detailSeries) {
	if s.key.feed != nil && s.key == (detailKey{m.feed, m.latest(), d.label, d.workerName}) {
		d.series = s
		d.redraw(m)
	}
}

// drawnKey says what a drawing of the charts is of: the series, the width
// of the chart, the poll interval (it cuts the hour into columns) and the
// levels it is drawn in.
type drawnKey struct {
	series   detailKey
	width    int
	interval time.Duration
	levels   string
}

// plot is a chart drawn, plain: its lines (see chart.block), and which
// columns of its plot are partial.
type plot struct {
	lines   []string
	partial []bool
}

// drawnCharts is the Completed and the Failed chart, drawn for key.
type drawnCharts struct {
	key   drawnKey
	plots [2]plot
}

// charts is the drawing of the charts of the series the screen holds, for
// width columns: the one it has when it is of that, else drawn again. A drawing
// is cheap — the series are built; this only cuts them into columns — and
// Update makes it (see install, handleWindowSize, and enter, which opens the
// screen) whenever the series or the width change, so View, which prints it,
// finds it made; View draws only where nothing has, as in a test that pushes a
// screen.
func (d *detailScreen) charts(m *Model, width int) [2]plot {
	held := d.series
	if held.key.feed != m.feed || held.key.label != d.label || held.key.worker != d.workerName {
		held = detailSeries{} // another view's: not drawn
	}
	c := newChart(m.interval())
	key := drawnKey{held.key, width, m.interval(), string(c.levels)}
	if d.drawn.key == key {
		return d.drawn.plots
	}
	d.drawn.key = key
	for i, pts := range [][]fleet.Point{held.completed, held.failed} {
		lines, partial := c.history(series{held.at, pts}, width, detailRows)
		d.drawn.plots[i] = plot{lines, partial}
	}
	return d.drawn.plots
}

// redraw makes the drawing of the charts, if there are any at this width.
func (d *detailScreen) redraw(m *Model) {
	if width := m.width - chartMargin; width >= blockMin {
		d.charts(m, width)
	}
}

// renderCharts renders the Completed /s (1h) and Failed /s (1h) charts, under
// the metrics: each its title and 5 rows over the axis, m.width − 4 columns
// wide, the hour up to the newest snapshot; blank until the series of the
// charts arrive, the history stays drawn (dimmed) while the snapshot is
// stale. "" where the screen is under 20 columns wide for them.
func (d *detailScreen) renderCharts(m *Model, stale bool) string {
	width := m.width - chartMargin
	if width < blockMin {
		return ""
	}
	plots := d.charts(m, width)

	title := lipgloss.NewStyle().Foreground(ColorHighlight).Bold(true)
	dim := lipgloss.NewStyle().Foreground(ColorSubtle)
	var parts []string
	for i, c := range []struct {
		title string
		color lipgloss.Color
	}{{"Completed /s (1h)", ColorSuccess}, {"Failed /s (1h)", ColorError}} {
		solid := lipgloss.NewStyle().Foreground(c.color)
		if stale {
			solid = dim // the history is shown, but dimmed
		}
		parts = append(parts, "", title.Render(c.title), plots[i].paint(solid, dim))
	}
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// paint colours a chart: its gutter and axis dim, its plot solid, but dim
// where its columns are partial. Runs of columns are painted at once.
func (p plot) paint(solid, dim lipgloss.Style) string {
	axis := len(p.lines) - 1
	out := make([]string, len(p.lines))
	for r, line := range p.lines {
		if r == axis {
			out[r] = dim.Render(line)
			continue
		}
		runes := []rune(line)
		var b strings.Builder
		b.WriteString(dim.Render(string(runes[:gutterW])))
		cells := runes[gutterW:]
		for from := 0; from < len(cells); {
			to := from
			for to < len(cells) && p.partial[to] == p.partial[from] {
				to++
			}
			style := solid
			if p.partial[from] {
				style = dim
			}
			b.WriteString(style.Render(string(cells[from:to])))
			from = to
		}
		out[r] = b.String()
	}
	return strings.Join(out, "\n")
}
