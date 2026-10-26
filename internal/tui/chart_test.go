package tui

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"

	"github.com/goptics/vartop/internal/fleet"
)

// vals are data points, one per value; a NaN is a gap.
func vals(vs ...float64) []fleet.Point {
	pts := make([]fleet.Point, len(vs))
	for i, v := range vs {
		if math.IsNaN(v) {
			pts[i] = fleet.Point{Gap: true}
			continue
		}
		pts[i] = fleet.Point{V: v}
	}
	return pts
}

var gap = math.NaN()

// TestSparklineBuckets: the points fall into width columns, evenly; a
// column is the mean of the points that fell into it, leaving out gaps, and
// a column with none is a space.
func TestSparklineBuckets(t *testing.T) {
	partial := []fleet.Point{{V: 3, Partial: true}, {V: 7}}

	for _, tc := range []struct {
		name  string
		pts   []fleet.Point
		width int
		want  string
	}{
		{"width 0", vals(1, 2, 3), 0, ""},
		{"negative width", vals(1, 2, 3), -4, ""},
		{"no points", nil, 4, "    "},
		{"one point per column", vals(0, 1, 2, 3, 4, 5, 6, 7), 8, "▁▂▃▄▅▆▇█"},
		{"gaps are spaces", vals(7, gap, 7, gap), 4, "█ █ "},
		{"only gaps", vals(gap, gap, gap), 3, "   "},
		{"a partial point is data", partial, 2, "▄█"},
		{"width 1 is one bucket", vals(1, 2, 3, 4, 5, 6, 7, 8), 1, "█"},
		{"width 1, one point", vals(5), 1, "█"},
		{"odd width: 7 points in 3 columns", vals(1, 1, 1, 4, 4, 8, 8), 3, "▂▅█"},
		{"odd width: 5 points in 3 columns", vals(0, 7, 7, 0, 7), 3, "▅▅█"},
		{"fewer points than columns leave spaces", vals(7, 7), 4, "█ █ "},
		{"one point, wide", vals(7), 3, "█  "},
		{"a bucket means its points, leaving gaps out", vals(2, gap, 6, 6), 2, "▃█"},
		{"a bucket of gaps and a point", vals(gap, 4, gap, gap), 2, "█ "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sparkline(tc.pts, tc.width); got != tc.want {
				t.Errorf("sparkline(%v, %d) = %q, want %q", tc.pts, tc.width, got, tc.want)
			}
		})
	}
}

// TestSparklinePeakScaling: the levels are scaled to the peak of the
// columns drawn, not to any absolute: whatever the magnitude the peak is the
// top level, and any data — zero too — is at least the lowest, never the
// space that means none.
func TestSparklinePeakScaling(t *testing.T) {
	for _, tc := range []struct {
		name string
		pts  []fleet.Point
		want string
	}{
		{"peak is the top level", vals(1, 2, 4), "▃▅█"},
		{"scale does not matter", vals(100, 200, 400), "▃▅█"},
		{"a tiny scale does not matter", vals(1e-9, 2e-9, 4e-9), "▃▅█"},
		{"a lone value is the peak", vals(1e-9), "█"},
		{"zero is data", vals(0, 0, 0), "▁▁▁"},
		{"zero beside a peak", vals(0, 5), "▁█"},
		{"a tiny value beside a huge one", vals(0.001, 1000), "▁█"},
		{"gaps are not data", vals(gap, 0, gap), " ▁ "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sparkline(tc.pts, len(tc.pts)); got != tc.want {
				t.Errorf("sparkline(%v) = %q, want %q", tc.pts, got, tc.want)
			}
		})
	}

	// The peak is the peak of the columns, not of the points they hold.
	if got := sparkline(vals(10, 0, 0, 0), 2); got != "█▁" {
		t.Errorf("a spike averaged into its column: sparkline = %q, want %q", got, "█▁")
	}

	// A value that is not a number is no data, and does not flatten the rest.
	nan := []fleet.Point{{V: math.NaN()}, {V: math.Inf(1)}, {V: 3}, {V: 6}}
	if got := sparkline(nan, 4); got != "  ▅█" {
		t.Errorf("sparkline with NaN and Inf = %q, want %q", got, "  ▅█")
	}
}

// TestASCIIFallback: where block runes are wide (RUNEWIDTH_EASTASIAN=1, an
// East Asian locale) the columns are drawn in _.-=# instead. It is asked
// each time: not fixed when the program starts.
func TestASCIIFallback(t *testing.T) {
	pts := vals(0, 1, 2, 3, 4, gap)

	t.Setenv("RUNEWIDTH_EASTASIAN", "1")
	if !asciiBlocks() {
		t.Fatal("asciiBlocks() = false with RUNEWIDTH_EASTASIAN=1, want true")
	}
	if got, want := sparkline(pts, 6), "_.-=# "; got != want {
		t.Errorf("ASCII sparkline = %q, want %q", got, want)
	}
	for _, r := range sparkline(vals(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), 10) {
		if r > 0x7f {
			t.Errorf("ASCII sparkline has %q, a rune that is not ASCII", r)
		}
	}
	// any data is at least the lowest level, as with the blocks
	if got := sparkline(vals(0, 0), 2); got != "__" {
		t.Errorf("zero data = %q, want %q", got, "__")
	}

	t.Setenv("RUNEWIDTH_EASTASIAN", "0")
	if asciiBlocks() {
		t.Fatal("asciiBlocks() = true with RUNEWIDTH_EASTASIAN=0, want false")
	}
	if got, want := sparkline(vals(0, 1, 2, 3, 4, 5, 6, 7), 8), "▁▂▃▄▅▆▇█"; got != want {
		t.Errorf("block sparkline = %q, want %q", got, want)
	}
}

// timesAt are the times t0 + each offset.
func timesAt(t0 time.Time, offsets ...time.Duration) []time.Time {
	at := make([]time.Time, len(offsets))
	for i, d := range offsets {
		at[i] = t0.Add(d)
	}
	return at
}

// TestTimeBuckets: the 10 minutes up to the newest snapshot are split into
// width equal spans, and each point goes where its snapshot's time is, so a
// stretch with no snapshots is blank — not squeezed away by the polls
// around it.
func TestTimeBuckets(t *testing.T) {
	end := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	m := time.Minute
	const window = fleet.TrendWindow

	// width 4: spans of 2m30s, [−10m, −7m30s) [−7m30s, −5m) [−5m, −2m30s) [−2m30s, 0].
	at := timesAt(end, -10*m, -9*m, -4*m, -3*m, 0)
	pts := vals(2, 4, 8, 6, 5)
	got := byTime(at, pts, window, 4)
	if len(got) != 4 {
		t.Fatalf("byTime returned %d buckets, want 4", len(got))
	}
	for i, want := range []struct {
		gap bool
		v   float64
	}{{false, 3}, {true, 0}, {false, 7}, {false, 5}} {
		if got[i].Gap != want.gap || !want.gap && got[i].V != want.v {
			t.Errorf("bucket %d = %+v, want gap %v, value %v", i, got[i], want.gap, want.v)
		}
	}
	// The same points as the columns of a series: the blank stretch shows.
	if line := newChart(5*time.Second).spark(series{at: at, pts: pts}, 4); line != "▄ █▆" {
		t.Errorf("series.spark(4) = %q, want %q", line, "▄ █▆")
	}

	t.Run("the newest point is in the last bucket", func(t *testing.T) {
		got := byTime(timesAt(end, 0), vals(5), window, 5)
		for i, b := range got {
			if wantGap := i != 4; b.Gap != wantGap {
				t.Errorf("bucket %d gap = %v, want %v", i, b.Gap, wantGap)
			}
		}
	})
	t.Run("a short history leaves the left blank", func(t *testing.T) {
		at := timesAt(end, -4*m, -3*m, -2*m, -m, 0)
		if line := newChart(5*time.Second).spark(series{at: at, pts: vals(1, 2, 3, 4, 5)}, 10); !strings.HasPrefix(line, "      ") || strings.HasSuffix(line, " ") {
			t.Errorf("5 minutes of history in a 10-minute window = %q, want its left half blank, its right drawn", line)
		}
	})
	t.Run("gaps do not count in a bucket", func(t *testing.T) {
		at := timesAt(end, -30*time.Second, -20*time.Second, -10*time.Second)
		got := byTime(at, []fleet.Point{{V: 4}, {Gap: true}, {V: 8}}, window, 1)
		if len(got) != 1 || got[0].Gap || got[0].V != 6 {
			t.Errorf("byTime = %+v, want one bucket of 6", got)
		}
		got = byTime(at, []fleet.Point{{Gap: true}, {Gap: true}, {Gap: true}}, window, 1)
		if len(got) != 1 || !got[0].Gap {
			t.Errorf("a bucket of gaps = %+v, want a gap", got)
		}
	})
	t.Run("a point before the window is left out", func(t *testing.T) {
		got := byTime(timesAt(end, -11*m, 0), vals(100, 1), window, 2)
		if !got[0].Gap || got[1].Gap || got[1].V != 1 {
			t.Errorf("byTime = %+v, want the early point left out", got)
		}
	})
	t.Run("a series of another length lines up at its end", func(t *testing.T) {
		// 3 times, 2 points: the points are those of the last 2 snapshots,
		// the ones at -7m and 0.
		got := byTime(timesAt(end, -9*m, -7*m, 0), vals(1, 2), window, 2)
		if got[0].Gap || got[0].V != 1 || got[1].Gap || got[1].V != 2 {
			t.Errorf("byTime = %+v, want 1 then 2: the points on the last two times", got)
		}
		if got := byTime(timesAt(end, -m, 0), vals(1, 2, 3, 4), window, 2); len(got) != 2 {
			t.Errorf("byTime with more points than times = %d buckets, want 2", len(got))
		}
	})
	t.Run("nothing", func(t *testing.T) {
		if got := byTime(nil, nil, window, 3); len(got) != 3 || !got[0].Gap || !got[1].Gap || !got[2].Gap {
			t.Errorf("byTime of nothing = %+v, want 3 gaps", got)
		}
		if got := byTime(timesAt(end, 0), vals(1), window, 0); len(got) != 0 {
			t.Errorf("byTime at width 0 = %+v, want none", got)
		}
		if line := newChart(5*time.Second).spark(series{}, 5); line != "     " {
			t.Errorf("an empty series = %q, want 5 spaces", line)
		}
		if line := newChart(5*time.Second).spark(series{at: timesAt(end, 0), pts: vals(1)}, 0); line != "" {
			t.Errorf("width 0 = %q, want nothing", line)
		}
	})
}

// TestKPILineFits: the KPI line is never wider than the screen, whatever its
// values, fresh or stale.
func TestKPILineFits(t *testing.T) {
	end := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	at := timesAt(end, -2*time.Minute, -time.Minute, 0)
	k := fleet.KPIView{
		CompletedPerSec: 1234.5, FailedPerMin: 99999.9, PendingMax: 1234567,
		Completed: vals(1, 2, 3), Failed: vals(3, gap, 1), Pending: vals(5, 6, 7),
	}
	for width := range 140 {
		for _, stale := range []bool{false, true} {
			line := newChart(5*time.Second).kpiLine(width, k, at, stale)
			if w := lipgloss.Width(line); w > width {
				t.Errorf("kpiLine(%d, stale %v) is %d wide: %q", width, stale, w, line)
			}
			if width == 0 && line != "" {
				t.Errorf("kpiLine(0) = %q, want nothing", line)
			}
		}
	}
	if line := newChart(5*time.Second).kpiLine(140, k, at, false); !strings.Contains(line, "completed/s  1234.5") || !strings.Contains(line, "failed/min 99999.9") || !strings.Contains(line, "pending 1234567") {
		t.Errorf("kpiLine = %q, want the values whole, in fields 7 wide", line)
	}
}

// TestSparklineNonFinite: a column whose sum overflows has no level to be: it
// is no data, and draws no panic.
func TestSparklineNonFinite(t *testing.T) {
	huge := math.MaxFloat64
	for _, tc := range []struct {
		name  string
		pts   []fleet.Point
		width int
		want  string
	}{
		{"a column that overflows", vals(huge, huge), 1, " "},
		{"beside one that does not", vals(huge, huge, 1, 2), 2, " █"},
		{"values near the limit that do not", vals(huge, huge/2), 2, "█▅"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sparkline(tc.pts, tc.width); got != tc.want {
				t.Errorf("sparkline = %q, want %q", got, tc.want)
			}
		})
	}
}

// everyInterval is a series of one snapshot each interval over the window up
// to end, each up to jitter of an interval early or late (the newest on
// time), with the value 1; those within the gap (offsets from end) missing.
func everyInterval(end time.Time, interval time.Duration, jitter float64, gapFrom, gapTo time.Duration) series {
	rng := rand.New(rand.NewPCG(1, 2))
	var s series
	for k := int(fleet.TrendWindow / interval); k >= 0; k-- {
		off := time.Duration(k) * interval
		if k > 0 {
			off += time.Duration((rng.Float64()*2 - 1) * jitter * float64(interval))
		}
		if off >= fleet.TrendWindow {
			off = fleet.TrendWindow
		}
		if off >= gapFrom && off < gapTo {
			continue
		}
		s.at = append(s.at, end.Add(-off))
		s.pts = append(s.pts, fleet.Point{V: 1})
	}
	return s
}

// TestSparklineLongIntervals: a slice of the window is at least two polls
// long, whatever the interval, so jitter in the polls does not leave a hole
// that reads as no data; only polls really missed do. The drawing is
// right-aligned in its cell.
func TestSparklineLongIntervals(t *testing.T) {
	end := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	const width = 20
	for _, tc := range []struct {
		interval time.Duration
		cols     int // slices of the window
	}{
		{time.Second, 20}, {5 * time.Second, 20}, {10 * time.Second, 20}, {20 * time.Second, 15},
		{30 * time.Second, 10}, {45 * time.Second, 6}, {60 * time.Second, 5}, {120 * time.Second, 2}, {5 * time.Minute, 1}, {20 * time.Minute, 1},
	} {
		t.Run(tc.interval.String(), func(t *testing.T) {
			c := newChart(tc.interval)
			full := c.spark(everyInterval(end, tc.interval, 0.2, -1, -1), width)
			if got, want := utf8.RuneCountInString(full), width; got != want {
				t.Fatalf("spark is %d columns, want the cell's %d: %q", got, want, full)
			}
			if want := strings.Repeat(" ", width-tc.cols) + strings.Repeat("█", tc.cols); full != want {
				t.Errorf("one poll each interval, up to 20%% early or late: %q, want %q", full, want)
			}

			// Polls missed over a stretch of 3 slices leave a space.
			span := fleet.TrendWindow / time.Duration(tc.cols)
			if tc.cols >= 4 {
				gapped := c.spark(everyInterval(end, tc.interval, 0.2, 2*span, 5*span), width)
				if drawn := strings.TrimLeft(gapped, " "); !strings.Contains(drawn, " ") {
					t.Errorf("polls missed for 3 slices: %q, want a space", gapped)
				}
			}
		})
	}
}

// TestChartLevelsPickedOnce: a chart takes the levels it draws with when it
// is made, so that a build of many sparklines asks the terminal once.
func TestChartLevelsPickedOnce(t *testing.T) {
	t.Setenv("RUNEWIDTH_EASTASIAN", "1")
	c := newChart(5 * time.Second)
	t.Setenv("RUNEWIDTH_EASTASIAN", "0")
	s := series{at: timesAt(time.Now(), 0), pts: vals(3)}
	if got := c.spark(s, 1); got != "#" {
		t.Errorf("a chart made where blocks are wide draws %q, want #", got)
	}
	if got := newChart(5*time.Second).spark(s, 1); got != "█" {
		t.Errorf("a chart made where they are not draws %q, want █", got)
	}
}

// TestKPILineWidthIsSteady: the sparklines are sized from the room the
// values can take, not the room they take now: the line does not change
// width, nor its sparklines, when the values turn to — and back.
func TestKPILineWidthIsSteady(t *testing.T) {
	end := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	at := timesAt(end, -2*time.Minute, -time.Minute, 0)
	for _, k := range []fleet.KPIView{
		{CompletedPerSec: 3, FailedPerMin: 0, PendingMax: 7, Completed: vals(1, 2, 3), Failed: vals(0, 0, 0), Pending: vals(5, 6, 7)},
		{CompletedPerSec: 1234.5, FailedPerMin: 99.9, PendingMax: 1234567, Completed: vals(1, 2, 3), Failed: vals(0, 0, 0), Pending: vals(5, 6, 7)},
		{Completed: vals(1, gap, gap), Failed: vals(gap, gap, gap), Pending: vals(5, 6, 7)}, // no current rate
	} {
		for width := 40; width < 130; width++ {
			c := newChart(5 * time.Second)
			fresh, stale := c.kpiLine(width, k, at, false), c.kpiLine(width, k, at, true)
			if lipgloss.Width(fresh) != lipgloss.Width(stale) {
				t.Fatalf("%d columns: the line is %d wide with values, %d without:\n%s\n%s", width, lipgloss.Width(fresh), lipgloss.Width(stale), fresh, stale)
			}
			if strings.ContainsAny(fresh, "▁▂▃▄▅▆▇█") != strings.ContainsAny(stale, "▁▂▃▄▅▆▇█") {
				t.Fatalf("%d columns: sparklines drawn %v with values, %v without", width, strings.ContainsAny(fresh, "▁▂▃▄▅▆▇█"), strings.ContainsAny(stale, "▁▂▃▄▅▆▇█"))
			}
		}
	}
}

// plain is lines as one string, a line to a row, for a failure message.
func plain(lines []string) string { return strings.Join(lines, "\n") }

// gutter is the 8 columns a block chart's rows start with, blank.
const gutter = "        "

// TestBlockChartZeroVsGap: a column that has data, zero too, is never blank —
// an all-zero series draws a baseline, the lowest eighth on the bottom row —
// and a gap is blank in every row. Zero and no data must not look the same.
func TestBlockChartZeroVsGap(t *testing.T) {
	const width, rows = 20, 5 // 12 columns of plot

	// All zero: the baseline, at the right (3 points, right-aligned), and
	// the peak, 0, in the gutter.
	got := blockChart(vals(0, 0, 0), width, rows)
	if len(got) != rows+1 {
		t.Fatalf("blockChart returned %d lines, want %d rows and the axis:\n%s", len(got), rows, plain(got))
	}
	for r, line := range got[:rows] {
		want := gutter + strings.Repeat(" ", 12)
		if r == 0 {
			want = "    0/s " + strings.Repeat(" ", 12)
		}
		if r == rows-1 {
			want = gutter + strings.Repeat(" ", 9) + "▁▁▁"
		}
		if line != want {
			t.Errorf("all-zero row %d = %q, want %q", r, line, want)
		}
	}

	// Nothing but gaps: every row blank, the gutter too — no data has no peak.
	got = blockChart(vals(gap, gap, gap), width, rows)
	for r, line := range got[:rows] {
		if line != strings.Repeat(" ", width) {
			t.Errorf("all-gap row %d = %q, want it blank", r, line)
		}
	}

	// Zero beside a gap: the zero draws, the gap does not, in any row.
	got = blockChart(vals(0, gap, 0), width, rows)
	for r, line := range got[:rows-1] {
		if strings.TrimSpace(line[len(gutter):]) != "" {
			t.Errorf("row %d = %q, want its plot blank above the baseline", r, line)
		}
	}
	if bottom := got[rows-1]; bottom != gutter+strings.Repeat(" ", 9)+"▁ ▁" {
		t.Errorf("bottom row = %q, want a baseline on both sides of the gap", bottom)
	}
	zero, gapped := blockChart(vals(0), width, rows), blockChart(vals(gap), width, rows)
	if zero[rows-1] == gapped[rows-1] {
		t.Errorf("zero and no data draw the same bottom row: %q", zero[rows-1])
	}

	// Zero beside a peak is the baseline still, not blank.
	if bottom := blockChart(vals(0, 5), width, rows)[rows-1]; bottom != gutter+strings.Repeat(" ", 10)+"▁█" {
		t.Errorf("bottom row of 0 and 5 = %q, want the baseline beside the peak", bottom)
	}

	// A value that is not a number is no data.
	nan := []fleet.Point{{V: math.NaN()}, {V: math.Inf(1)}, {V: 2}}
	if bottom := blockChart(nan, width, rows)[rows-1]; bottom != gutter+strings.Repeat(" ", 9)+"  █" {
		t.Errorf("bottom row with NaN and Inf = %q, want them blank", bottom)
	}
}

// TestBlockChartPeak: the rows are scaled to the peak of the series: the
// peak's column is full to the top row, and the peak itself — whatever its
// magnitude — is printed in the gutter of the top row, the other rows'
// gutters blank.
func TestBlockChartPeak(t *testing.T) {
	const width = 20

	// 2 rows of 8 levels, 16: 0 is the baseline, 6 of 12 is half, 12 the top.
	got := blockChart(vals(0, 6, 12), width, 2)
	want := []string{
		" 12.0/s " + strings.Repeat(" ", 9) + "  █",
		gutter + strings.Repeat(" ", 9) + "▁██",
	}
	if !slices.Equal(got[:2], want) {
		t.Errorf("blockChart(0, 6, 12) =\n%s\nwant\n%s", plain(got[:2]), plain(want))
	}

	// 3 rows, 24 levels: the peak 24 is 24 levels, 17 is the bottom two rows
	// and the lowest level of the third, 9 the bottom row and the lowest of
	// the second, 1 the lowest level of the bottom.
	got = blockChart(vals(1, 9, 17, 24), width, 3)
	want = []string{
		" 24.0/s " + strings.Repeat(" ", 8) + "  ▁█",
		gutter + strings.Repeat(" ", 8) + " ▁██",
		gutter + strings.Repeat(" ", 8) + "▁███",
	}
	if !slices.Equal(got[:3], want) {
		t.Errorf("blockChart(1, 9, 17, 24) =\n%s\nwant\n%s", plain(got[:3]), plain(want))
	}

	// The scale does not matter: the peak is the top, its value the label.
	for _, tc := range []struct {
		scale float64
		label string
	}{{1e-4, "<0.01/s"}, {0.005, "0.06/s"}, {1, "12.0/s"}, {1e3, "12.0k/s"}, {1e5, "1.2M/s"}} {
		got := blockChart(vals(0, 6*tc.scale, 12*tc.scale), width, 2)
		if g := strings.TrimSpace(got[0][:8]); g != tc.label {
			t.Errorf("peak %v labelled %q, want %q", 12*tc.scale, g, tc.label)
		}
		if got[0][8:] != strings.Repeat(" ", 11)+"█" || got[1][8:] != strings.Repeat(" ", 9)+"▁██" {
			t.Errorf("peak %v drawn\n%s\nwant the same shape at any scale", 12*tc.scale, plain(got[:2]))
		}
		for r := 1; r < 2; r++ {
			if got[r][:8] != gutter {
				t.Errorf("peak %v: row %d gutter = %q, want it blank", 12*tc.scale, r, got[r][:8])
			}
		}
	}

	// The peak is the peak of the columns drawn, not of the points they
	// hold (as for a sparkline): a spike of 24 shares its column with a 0.
	pts := make([]fleet.Point, 24)
	pts[0].V = 24
	if got := blockChart(pts, width, 1); got[0] != " 12.0/s █"+strings.Repeat("▁", 11) {
		t.Errorf("blockChart of a spike = %q, want the column's mean, 12, the peak", got[0])
	}

	// It is the peak of the whole series, wherever it is.
	for _, pts := range [][]fleet.Point{vals(9, 1, 1), vals(1, 9, 1), vals(1, 1, 9)} {
		if got := blockChart(pts, width, 5); got[0][:8] != "  9.0/s " {
			t.Errorf("peak of %v labelled %q, want 9.0/s", pts, got[0][:8])
		}
	}
}

// TestRateLabel: the peak is at most 7 columns wide, whatever it is — the
// gutter is 8 and keeps a column between its text and the plot — and keeps
// its significant digits.
func TestRateLabel(t *testing.T) {
	for _, tc := range []struct {
		v    float64
		want string
	}{
		{0, "0/s"}, {-1, "0/s"}, {1e-9, "<0.01/s"}, {0.0099, "<0.01/s"}, {0.01, "0.01/s"}, {0.05, "0.05/s"}, {0.099, "0.10/s"}, {0.1, "0.1/s"},
		{1, "1.0/s"}, {12.34, "12.3/s"}, {99.9, "99.9/s"}, {99.95, "100/s"}, {100, "100/s"}, {999.4, "999/s"}, {999.9, "1.0k/s"},
		{1000, "1.0k/s"}, {9999, "10.0k/s"}, {12345, "12.3k/s"}, {99999.9, "100k/s"}, {999e3, "999k/s"},
		{1e6, "1.0M/s"}, {123e6, "123M/s"}, {1e9, ">999M/s"}, {math.MaxFloat64, ">999M/s"},
	} {
		got := rateLabel(tc.v)
		if got != tc.want {
			t.Errorf("rateLabel(%v) = %q, want %q", tc.v, got, tc.want)
		}
		if w := lipgloss.Width(got); w > 7 {
			t.Errorf("rateLabel(%v) = %q is %d wide, want at most 7", tc.v, got, w)
		}
	}
}

// TestBlockChartWidths: every line is exactly width columns, whatever the
// peak (the gutter's text never widens a line), at the widths the detail
// screen has — and rows of plot, then the axis.
func TestBlockChartWidths(t *testing.T) {
	for _, width := range []int{20, 21, 80, 200} {
		for _, peak := range []float64{0, 1e-12, 0.05, 1.5, 99999.9, 1e9, math.MaxFloat64} {
			for _, n := range []int{0, 1, 5, width - 8, width, 3 * width} {
				pts := make([]fleet.Point, n)
				for i := range pts {
					pts[i].V = peak * float64(i%7) / 6
				}
				got := blockChart(pts, width, 5)
				if len(got) != 6 {
					t.Fatalf("width %d peak %v, %d points: %d lines, want 5 rows and the axis", width, peak, n, len(got))
				}
				for i, line := range got {
					if w := lipgloss.Width(line); w != width {
						t.Errorf("width %d peak %v, %d points: line %d is %d wide: %q", width, peak, n, i, w, line)
					}
					if i > 0 && i < 5 && line[:8] != gutter {
						t.Errorf("width %d: row %d's gutter = %q, want it blank: only the top row has the peak", width, i, line[:8])
					}
				}
			}
		}
	}

	// Under 20 columns there is no chart: its gutter, the axis and a plot
	// worth drawing do not fit.
	for _, width := range []int{19, 12, 1, 0, -5} {
		if got := blockChart(vals(1, 2, 3), width, 5); got != nil {
			t.Errorf("blockChart at width %d = %q, want none", width, got)
		}
	}
	if got := blockChart(vals(1, 2, 3), 80, 0); got != nil {
		t.Errorf("blockChart of no rows = %q, want none", got)
	}
}

// TestBlockChartAxis: -60m … now goes under the plot — not under the gutter —
// at its left and right edges, under the columns drawn.
func TestBlockChartAxis(t *testing.T) {
	// 12 points fill the 12 columns of plot at 20.
	got := blockChart(make([]fleet.Point, 12), 20, 5)
	if axis := got[5]; axis != gutter+"-60m  …  now" {
		t.Errorf("axis = %q, want -60m … now across the plot, after the gutter", axis)
	}
	// 10 columns: the text as it is.
	if axis := blockChart(make([]fleet.Point, 10), 80, 5)[5]; axis != strings.Repeat(" ", 70)+"-60m … now" {
		t.Errorf("axis under 10 columns = %q, want -60m … now at the right of the plot", axis)
	}
	// Fewer columns drawn than the text is wide: it keeps its width, right-aligned.
	if axis := blockChart(make([]fleet.Point, 3), 80, 5)[5]; axis != strings.Repeat(" ", 70)+"-60m … now" {
		t.Errorf("axis under 3 columns = %q, want the text, right-aligned", axis)
	}
	// Under the columns drawn: -60m is under the oldest, now the newest.
	axis := blockChart(make([]fleet.Point, 30), 200, 5)[5]
	if !strings.HasPrefix(axis, strings.Repeat(" ", 192-30+8)+"-60m") || !strings.HasSuffix(axis, "now") || lipgloss.Width(axis) != 200 {
		t.Errorf("axis under 30 of 192 columns = %q, want -60m under the oldest and now under the newest", axis)
	}
	// More points than the plot has columns: the whole plot.
	axis = blockChart(make([]fleet.Point, 500), 200, 5)[5]
	if !strings.HasPrefix(axis, gutter+"-60m") || !strings.HasSuffix(axis, "now") || lipgloss.Width(axis) != 200 {
		t.Errorf("axis over the whole plot = %q", axis)
	}
}

// TestBlockChartColumns: more points than the plot has columns are folded
// into them by their mean, as a sparkline folds them; fewer take a column
// each, at the right.
func TestBlockChartColumns(t *testing.T) {
	// 24 points of 0 and 10, in pairs: every column is 5, the peak.
	pts := make([]fleet.Point, 24)
	for i := range pts {
		pts[i].V = float64(i%2) * 10
	}
	if got := blockChart(pts, 20, 1)[0]; got != "  5.0/s "+strings.Repeat("█", 12) {
		t.Errorf("24 points in 12 columns = %q, want the mean, 5, in each", got)
	}
	// Halves: the first 12 points 0, the rest 10: 6 columns of baseline, 6 of peak.
	for i := range pts {
		pts[i].V = float64(i/12) * 10
	}
	if got := blockChart(pts, 20, 1)[0]; got != " 10.0/s "+strings.Repeat("▁", 6)+strings.Repeat("█", 6) {
		t.Errorf("24 points in halves = %q, want 6 columns of baseline, then 6 of peak", got)
	}
	// Fewer points: a column each, at the right, not stretched.
	if got := blockChart(vals(0, 4, 8), 20, 1)[0]; got != "  8.0/s "+strings.Repeat(" ", 9)+"▁▄█" {
		t.Errorf("3 points = %q, want one column each at the right", got)
	}
	// A gap between points is a blank column.
	if got := blockChart(vals(8, gap, gap, 8), 20, 1)[0]; got != "  8.0/s "+strings.Repeat(" ", 8)+"█  █" {
		t.Errorf("points with a gap = %q", got)
	}
}

// TestBlockChartPartial: the columns of partial points are told, in a mask
// that lines up with the plot, for the caller to dim — a column folded from
// points is partial when any is; a gap, and the blank at the left, are not.
func TestBlockChartPartial(t *testing.T) {
	pts := []fleet.Point{{V: 1}, {V: 2, Partial: true}, {V: 3}, {Gap: true, Partial: true}}
	lines, partial := blockPlot(pts, 20, 2)
	if len(partial) != 12 {
		t.Fatalf("the mask is %d columns, want the plot's 12", len(partial))
	}
	for i, p := range partial {
		if want := i == 9; p != want {
			t.Errorf("column %d partial = %v, want %v", i, p, want)
		}
	}
	if want := blockChart(pts, 20, 2); !slices.Equal(lines, want) {
		t.Errorf("blockPlot draws\n%s\nbut blockChart\n%s", plain(lines), plain(want))
	}

	// Folded: 24 points into 12 columns, the sixth of them partial: column 2.
	many := make([]fleet.Point, 24)
	many[5].Partial = true
	_, partial = blockPlot(many, 20, 2)
	for i, p := range partial {
		if want := i == 2; p != want {
			t.Errorf("folded: column %d partial = %v, want %v", i, p, want)
		}
	}

	if _, partial := blockPlot(vals(1, 2), 19, 2); partial != nil {
		t.Errorf("a mask for no chart = %v", partial)
	}
}

// TestBlockChartASCII: where block runes are wide the chart is drawn in
// _.-=# — a cell has fewer levels, a partial one the lowest that holds it — and
// the axis with a plain dot: every line ASCII, the same width.
func TestBlockChartASCII(t *testing.T) {
	t.Setenv("RUNEWIDTH_EASTASIAN", "1")
	got := blockChart(vals(0, 6, 12), 20, 2)
	want := []string{
		" 12.0/s " + strings.Repeat(" ", 9) + "  #",
		gutter + strings.Repeat(" ", 9) + "_##",
		strings.Repeat(" ", 10) + "-60m . now",
	}
	if !slices.Equal(got, want) {
		t.Errorf("ASCII chart =\n%s\nwant\n%s", plain(got), plain(want))
	}
	got = blockChart(vals(1, 3, 5, 7, 9, 10), 20, 2)
	for _, line := range got {
		for _, r := range line {
			if r > 0x7f {
				t.Errorf("ASCII chart has %q, a rune that is not ASCII: %q", r, line)
			}
		}
		if w := lipgloss.Width(line); w != 20 {
			t.Errorf("ASCII line is %d wide: %q", w, line)
		}
	}

	t.Setenv("RUNEWIDTH_EASTASIAN", "0")
	if got := blockChart(vals(0, 6, 12), 20, 2); got[1] != gutter+strings.Repeat(" ", 9)+"▁██" {
		t.Errorf("block chart = %q, want the blocks again", got[1])
	}
}

// hourEvery is a series of one snapshot each interval over the hour up to
// end, each up to jitter of an interval early or late (the newest on time),
// of the value 1.
func hourEvery(end time.Time, interval time.Duration, jitter float64) series {
	rng := rand.New(rand.NewPCG(3, 4))
	var s series
	for k := int(time.Hour / interval); k >= 0; k-- {
		off := time.Duration(k) * interval
		if k > 0 {
			off += time.Duration((rng.Float64()*2 - 1) * jitter * float64(interval))
		}
		s.at = append(s.at, end.Add(-min(off, time.Hour)))
		s.pts = append(s.pts, fleet.Point{V: 1})
	}
	return s
}

// TestHistoryLongIntervals: the hour is cut into as many columns as fit, but
// each at least two polls long (as for a sparkline), so a poll a little late
// leaves no hole that reads as no data — and, where polls are minutes
// apart, fewer columns than the plot has, right-aligned, the axis under
// them.
func TestHistoryLongIntervals(t *testing.T) {
	end := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for _, width := range []int{20, 80, 200} {
		plot := width - 8
		for _, tc := range []struct {
			interval time.Duration
			cols     int // of the hour
		}{
			{time.Second, 1800}, {5 * time.Second, 360}, {10 * time.Second, 180}, {30 * time.Second, 60},
			{time.Minute, 30}, {2 * time.Minute, 15}, {5 * time.Minute, 6}, {30 * time.Minute, 1},
		} {
			t.Run(fmt.Sprintf("%d/%s", width, tc.interval), func(t *testing.T) {
				cols := min(plot, tc.cols)
				if got := newChart(tc.interval).columns(time.Hour, plot); got != cols {
					t.Fatalf("columns(1h, %d) = %d, want %d", plot, got, cols)
				}
				lines, _ := newChart(tc.interval).history(hourEvery(end, tc.interval, 0.2), width, 3)
				if len(lines) != 4 {
					t.Fatalf("%d lines, want 3 rows and the axis", len(lines))
				}
				for r, line := range lines[:3] {
					if r > 0 && line[:8] != gutter {
						t.Errorf("row %d's gutter = %q", r, line[:8])
					}
					if got, want := line[8:], strings.Repeat(" ", plot-cols)+strings.Repeat("█", cols); got != want {
						t.Errorf("row %d = %q, want %d columns of a poll each, right-aligned, no hole", r, got, cols)
					}
				}
				if axis := lines[3]; lipgloss.Width(axis) != width || !strings.HasSuffix(axis, "now") ||
					!strings.HasPrefix(strings.TrimLeft(axis, " "), "-60m") || len(axis)-len(strings.TrimLeft(axis, " ")) != 8+plot-max(cols, 10) {
					t.Errorf("axis = %q, want -60m under the oldest of the %d columns, now the newest", axis, cols)
				}
			})
		}
	}

	// Polls missed over a stretch are blank, not squeezed away.
	s := hourEvery(end, 5*time.Second, 0)
	keep := 0
	for i := range s.at {
		if off := end.Sub(s.at[i]); off < 20*time.Minute || off >= 40*time.Minute {
			s.at[keep], s.pts[keep] = s.at[i], s.pts[i]
			keep++
		}
	}
	s.at, s.pts = s.at[:keep], s.pts[:keep]
	lines, _ := newChart(5*time.Second).history(s, 128, 1) // 120 columns of 30s
	// Columns of 30s: those of the polls missed are blank (the 39 whose
	// spans have none), the rest drawn.
	if row, want := lines[0][8:], strings.Repeat("█", 41)+strings.Repeat(" ", 39)+strings.Repeat("█", 40); row != want {
		t.Errorf("an hour with 20 minutes missing = %q, want %q", row, want)
	}

	// No history: the rows are blank, the axis is there.
	lines, partial := newChart(5*time.Second).history(series{}, 80, 5)
	for r, line := range lines[:5] {
		if line != strings.Repeat(" ", 80) {
			t.Errorf("no history: row %d = %q, want it blank", r, line)
		}
	}
	if len(partial) != 72 || !strings.HasSuffix(lines[5], "now") {
		t.Errorf("no history: %d mask columns, axis %q", len(partial), lines[5])
	}
}
