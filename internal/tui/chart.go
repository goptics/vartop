package tui

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/goptics/vartop/internal/fleet"
)

const (
	// blockLevels are the levels of a sparkline, lowest first.
	blockLevels = "▁▂▃▄▅▆▇█"
	// asciiLevels are its levels where block runes are wide.
	asciiLevels = "_.-=#"
)

// asciiBlocks reports whether block runes are wider than one cell in the
// terminal's width rules (RUNEWIDTH_EASTASIAN=1, an East Asian locale), so
// that a sparkline drawn with them would not line up: it is drawn in
// asciiLevels instead.
//
// It asks each time. runewidth reads the environment into its default
// condition once, when the program starts, and NewCondition only copies
// that; the rules are read here again, as runewidth reads them.
func asciiBlocks() bool {
	c := runewidth.NewCondition()
	if env := os.Getenv("RUNEWIDTH_EASTASIAN"); env != "" {
		c.EastAsianWidth = env == "1"
	} else {
		c.EastAsianWidth = runewidth.IsEastAsian()
	}
	return c.RuneWidth('█') != 1
}

// hasData reports whether p has a value to draw: not a gap, and a number.
func hasData(p fleet.Point) bool {
	return !p.Gap && !math.IsNaN(p.V) && !math.IsInf(p.V, 0)
}

// bucketed folds pts into width points: the mean of the data points that
// column puts in each — column gives the index of a point's column, and
// false when it has none — and a gap where it puts none. Partial points are
// data, and make the point of their column partial. It returns nothing for a
// width under 1.
func bucketed(pts []fleet.Point, width int, column func(i int) (int, bool)) []fleet.Point {
	if width < 1 {
		return nil
	}
	sum := make([]float64, width)
	n := make([]int, width)
	partial := make([]bool, width)
	for i, p := range pts {
		if c, ok := column(i); ok && hasData(p) {
			sum[c] += p.V
			n[c]++
			partial[c] = partial[c] || p.Partial
		}
	}
	out := make([]fleet.Point, width)
	for c := range out {
		// Finite values can still sum to more than a float holds.
		if mean := sum[c] / float64(max(n[c], 1)); n[c] == 0 || math.IsInf(mean, 0) {
			out[c].Gap = true
		} else {
			out[c].V, out[c].Partial = mean, partial[c]
		}
	}
	return out
}

// sparkLevels are the runes sparklines are drawn with now: blockLevels, or
// asciiLevels where block runes are wide. It asks the terminal's width
// rules, so a build of many sparklines asks once (see chart).
func sparkLevels() []rune {
	if asciiBlocks() {
		return []rune(asciiLevels)
	}
	return []rune(blockLevels)
}

// sparkline draws pts as width columns, one rune each: the points, taken to
// be evenly spaced, are folded into the columns (see bucketed), and each
// column is a level of blockLevels — asciiLevels where block runes are wide —
// scaled so that the highest column is the top level. A column of data is
// never blank, whatever its value: a space means none.
func sparkline(pts []fleet.Point, width int) string {
	return draw(bucketed(pts, width, func(i int) (int, bool) { return i * width / len(pts), true }), sparkLevels())
}

// draw writes cols, one rune each, as levels scaled to the highest of them;
// a gap is a space.
func draw(cols []fleet.Point, levels []rune) string {
	var peak float64
	for _, c := range cols {
		if !c.Gap {
			peak = max(peak, c.V)
		}
	}
	var b strings.Builder
	for _, c := range cols {
		if c.Gap {
			b.WriteByte(' ')
			continue
		}
		level := 0
		if peak > 0 && c.V > 0 {
			if f := math.Round(c.V / peak * float64(len(levels)-1)); !math.IsNaN(f) {
				level = min(max(int(f), 0), len(levels)-1)
			}
		}
		b.WriteRune(levels[level])
	}
	return b.String()
}

// byTime folds pts into width points by time. at holds the times the
// points were taken, oldest first; a series that is of the last snapshots
// only (as fleet's are, of the newest window) has fewer points than there
// are times, or the other way round, and lines up at its end. The window up
// to the newest time is split into width equal spans, and each point goes
// to the span its time falls in — a span with no snapshots, say while no
// daemon ran, comes out a gap, not squeezed away by the polls around it.
// A time outside the window is left out; the newest is in the last span.
func byTime(at []time.Time, pts []fleet.Point, window time.Duration, width int) []fleet.Point {
	if len(pts) > len(at) {
		pts = pts[len(pts)-len(at):]
	} else {
		at = at[len(at)-len(pts):]
	}
	if len(at) == 0 || window <= 0 {
		return bucketed(nil, width, nil)
	}
	end := at[len(at)-1]
	start := end.Add(-window)
	return bucketed(pts, width, func(i int) (int, bool) {
		since := at[i].Sub(start)
		if since < 0 || at[i].After(end) {
			return 0, false
		}
		return min(int(since*time.Duration(width)/window), width-1), true
	})
}

// series is what a sparkline charts: points, each with the time of the
// snapshot it was taken from.
type series struct {
	at  []time.Time
	pts []fleet.Point
}

// chart draws sparklines, and block charts of the history, of series polled
// every interval. Its levels are picked when it is made, so that the many
// sparklines of one build ask the terminal's width rules once.
type chart struct {
	levels   []rune
	interval time.Duration
}

func newChart(interval time.Duration) chart {
	return chart{levels: sparkLevels(), interval: interval}
}

// spark draws s over the trend window, up to its newest time, in a cell
// width columns wide: the window is cut into slices (see columns), a column
// each, and the drawing is right-aligned in the cell.
func (c chart) spark(s series, width int) string {
	if width < 1 {
		return ""
	}
	cols := c.columns(fleet.TrendWindow, width)
	return strings.Repeat(" ", width-cols) + draw(byTime(s.at, s.pts, fleet.TrendWindow, cols), c.levels)
}

// columns is how many columns of a plot width wide a window of history is
// cut into: as many as fit, but each at least two polls long, so that polls
// a little late still leave one in every column and no hole reads as no data
// — only polls missed do. Where the polls are minutes apart that is fewer
// columns than the plot has, and the drawing is right-aligned in it.
func (c chart) columns(window time.Duration, width int) int {
	if c.interval <= 0 {
		return width
	}
	return min(width, max(1, int(window/(2*c.interval))))
}

// kpiValueW is the width of the field each KPI's value is in, right-aligned:
// the widest a value is, near enough (99999.9; 9999999 pending).
const kpiValueW = 7

// kpiLine is the line under the dashboard header: the completed/s,
// failed/min and pending of k, each its current value and a sparkline of
// the series (of the snapshots taken at at). The sparklines take a third
// each of the width the text leaves, at most 12 columns, and are left out
// below 4; the text is sized for the widest values, so that the sparklines
// keep their width as the values change, or turn to —. A current value is —
// while stale, and when the newest point of its series has no data (the web
// pages' rule); the sparklines are drawn all the same. It is never wider than
// width.
func (c chart) kpiLine(width int, k fleet.KPIView, at []time.Time, stale bool) string {
	if width < 1 {
		return ""
	}
	labels := [3]string{"completed/s", "failed/min", "pending"}
	values := [3]string{fmt.Sprintf("%.1f", k.CompletedPerSec), fmt.Sprintf("%.1f", k.FailedPerMin), strconv.Itoa(k.PendingMax)}
	pts := [3][]fleet.Point{k.Completed, k.Failed, k.Pending}
	for i := range values {
		// As on the web pages: a value is current only while the snapshot is,
		// and when the newest point has data.
		if stale || len(pts[i]) == 0 || pts[i][len(pts[i])-1].Gap {
			values[i] = "—"
		}
	}

	// The text is all there is but the sparklines: 2 columns of margin, 2
	// between each KPI, 1 before each sparkline, and each label and value
	// field.
	text := 2 + 2*2 + 3
	for i := range labels {
		text += lipgloss.Width(labels[i]) + 1 + max(kpiValueW, lipgloss.Width(values[i]))
	}
	spark := min((width-text)/3, 12)
	if spark < 4 {
		spark = 0
	}

	label := lipgloss.NewStyle().Foreground(ColorSubtle)
	value := lipgloss.NewStyle().Foreground(ColorFg).Bold(true)
	line := lipgloss.NewStyle().Foreground(ColorHighlight)
	if stale {
		line = label // the history is shown, but dimmed
	}
	parts := make([]string, len(labels))
	for i := range labels {
		style := value
		if values[i] == "—" {
			style = label
		}
		pad := strings.Repeat(" ", max(0, kpiValueW-lipgloss.Width(values[i])))
		parts[i] = label.Render(labels[i]) + " " + pad + style.Render(values[i])
		if spark > 0 {
			parts[i] += " " + line.Render(c.spark(series{at, pts[i]}, spark))
		}
	}
	return lipgloss.NewStyle().MaxWidth(width).Render("  " + strings.Join(parts, "  "))
}

const (
	// gutterW is the width of the gutter a block chart's rows start with: the
	// peak is printed in it, right-aligned, on the top row, and its last
	// column is left blank, between the label and the plot.
	gutterW = 8
	// axisMin is the width of the axis' text, -60m … now.
	axisMin = 10
	// blockMin is the width under which there is no block chart: the gutter and
	// a plot of 12 columns, room for the axis' text.
	blockMin = 20
	// historyWindow is how far back a block chart of the history reaches, up to
	// the newest snapshot.
	historyWindow = time.Hour
)

// rateLabel is a rate in at most 7 columns, for the gutter of a block chart:
// 12.3/s, with the digits that tell it from its neighbours — under 100 one
// decimal, under 1000 none — then 1.2k/s and 1.2M/s; a rate of nothing is
// 0/s, one under a hundredth <0.01/s, one past 999M/s >999M/s.
func rateLabel(v float64) string {
	switch {
	case !(v > 0): // zero, negative, or no number
		return "0/s"
	case v < 0.01:
		return "<0.01/s"
	case v < 0.1:
		return fmt.Sprintf("%.2f/s", v)
	}
	for _, unit := range []string{"", "k", "M"} {
		switch {
		case v < 99.95: // 99.95 would round to 100.0: too wide with a unit
			return fmt.Sprintf("%.1f%s/s", v, unit)
		case v < 999.5:
			return fmt.Sprintf("%.0f%s/s", v, unit)
		}
		v /= 1000
	}
	return ">999M/s"
}

// axisLine is the axis of a chart whose columns are span wide: -60m under the
// oldest, now under the newest, and … between them, in the middle.
func axisLine(span int, ascii bool) string {
	mid := "…"
	if ascii {
		mid = "." // an ellipsis is as wide as a block rune: 2, where those are
	}
	inner := span - axisMin + 3 // the blanks around the …, at least 3
	left := (inner - 1) / 2
	return "-60m" + strings.Repeat(" ", left) + mid + strings.Repeat(" ", inner-1-left) + "now"
}

// ascii reports whether c draws in asciiLevels.
func (c chart) ascii() bool { return len(c.levels) == len(asciiLevels) }

// plotColumns is pts as the columns of a plot of width plot: folded into them
// by their mean when there are more points than columns (see bucketed), else
// a column each, the points evenly spaced and the last one the newest. A
// value that is not a number is a gap.
func plotColumns(pts []fleet.Point, plot int) []fleet.Point {
	if len(pts) > plot {
		return bucketed(pts, plot, func(i int) (int, bool) { return i * plot / len(pts), true })
	}
	cols := make([]fleet.Point, len(pts))
	for i, p := range pts {
		if hasData(p) {
			cols[i] = fleet.Point{V: p.V, Partial: p.Partial}
		} else {
			cols[i].Gap = true
		}
	}
	return cols
}

// block draws pts as a chart width columns wide and rows tall, with the
// axis under it: rows lines of the plot and then the axis line, each width
// columns wide; and which columns of the plot — not of the line — are partial
// (see fleet.Point), for the caller to dim. It returns nothing under blockMin
// columns or for no rows.
//
// A line starts with the gutter (gutterW columns), the peak in it on the top
// row, blank on the others; the plot is the rest. The points, taken to be
// evenly spaced, are folded into the plot's columns (see plotColumns); fewer
// than it has take a column each, at its right, the axis under them. Each
// column is drawn from the bottom in cells of the levels of c, rows of them
// high in all, scaled so that the highest column — the peak, of the columns
// drawn — fills the top row. A column of data is never blank, whatever its
// value: zero is the lowest level on the bottom row, a baseline. A gap is
// blank in every row.
func (c chart) block(pts []fleet.Point, width, rows int) (lines []string, partial []bool) {
	if width < blockMin || rows < 1 {
		return nil, nil
	}
	plot := width - gutterW
	cols := plotColumns(pts, plot)
	per := len(c.levels)

	var peak float64
	data := false
	for _, p := range cols {
		if !p.Gap {
			peak, data = max(peak, p.V), true
		}
	}
	// height is a column's in levels; 0 is blank.
	height := make([]int, len(cols))
	for i, p := range cols {
		switch {
		case p.Gap:
		case peak > 0 && p.V > 0:
			height[i] = min(max(int(math.Round(p.V/peak*float64(rows*per))), 1), rows*per)
		default:
			height[i] = 1
		}
	}

	off := plot - len(cols)
	lines = make([]string, 0, rows+1)
	for r := range rows {
		var b strings.Builder
		if r == 0 && data {
			fmt.Fprintf(&b, "%*s ", gutterW-1, rateLabel(peak))
		} else {
			b.WriteString(strings.Repeat(" ", gutterW))
		}
		b.WriteString(strings.Repeat(" ", off))
		below := (rows - 1 - r) * per // the levels of the cells under this row
		for _, h := range height {
			switch fill := h - below; {
			case fill <= 0:
				b.WriteByte(' ')
			case fill >= per:
				b.WriteRune(c.levels[per-1])
			default:
				b.WriteRune(c.levels[fill-1])
			}
		}
		lines = append(lines, b.String())
	}
	span := min(plot, max(len(cols), axisMin))
	lines = append(lines, strings.Repeat(" ", gutterW+plot-span)+axisLine(span, c.ascii()))

	partial = make([]bool, plot)
	for i, p := range cols {
		partial[off+i] = p.Partial && !p.Gap
	}
	return lines, partial
}

// blockPlot is block of a chart of the levels the terminal draws now: what
// blockChart is, and which columns of its plot are partial.
func blockPlot(pts []fleet.Point, width, rows int) (lines []string, partial []bool) {
	return newChart(0).block(pts, width, rows)
}

// blockChart draws pts as rows lines of width columns, and the axis under
// them (see chart.block): a block chart of 8 levels to a cell — fewer where
// block runes are wide. The caller dims the partial points' columns (see
// blockPlot).
func blockChart(pts []fleet.Point, width, rows int) []string {
	lines, _ := blockPlot(pts, width, rows)
	return lines
}

// history draws s, the series of the history of a worker a feed holds, over
// the last hour up to its newest time, as a block chart width columns wide
// and rows tall (see block): the hour is cut into slices (see columns) — a
// stretch with no snapshots, say while no daemon ran, comes out blank, not
// squeezed away by the polls around it.
func (c chart) history(s series, width, rows int) ([]string, []bool) {
	if width < blockMin {
		return nil, nil
	}
	return c.block(byTime(s.at, s.pts, historyWindow, c.columns(historyWindow, width-gutterW)), width, rows)
}
