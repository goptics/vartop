package tui

import (
	"math"
	"math/rand/v2"
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
