package fleet

import (
	"fmt"
	"maps"
	"slices"
	"time"
)

const (
	// DayWindow is how far back the 24h charts reach: the shortest history,
	// and how long the store keeps snapshots.
	DayWindow = 24 * time.Hour
	// DayBucket is the span a stored bucket sums up: 288 a day. A point of
	// a longer chart merges several (see StepFor).
	DayBucket = 5 * time.Minute
)

// steps are the spans a point of a chart longer than the hour may sum up,
// shortest first: each a whole number of DayBuckets, dividing the hour.
var steps = []time.Duration{DayBucket, 10 * time.Minute, 15 * time.Minute, 20 * time.Minute, 30 * time.Minute, time.Hour}

// StepFor is the span one point of a chart of span sums up: the shortest
// step giving it at most 720 points, a point per pixel of its 720 px —
// 24h 5m, 7d 15m, 30d (the longest history) 1h.
func StepFor(span time.Duration) time.Duration {
	for _, step := range steps {
		if span/step <= 720 {
			return step
		}
	}
	return steps[len(steps)-1]
}

// DayPoint is what one snapshot adds to one worker's day, merged (Label
// "") or of the unit labelled Label: its completed/s and failed/s, and how
// many of the units expected to give a rate did (see the file comment).
type DayPoint struct {
	Worker, Label          string
	Completed, Failed      float64
	Contributing, Expected int
}

// DayPoints is what s adds to the day of each worker on it: a point merged
// and one per unit label (its first unit's, as PodsOf takes), none for a
// view no unit is expected in. The workers are those its healthy units
// list: a snapshot no unit answered names none, and adds nothing.
func DayPoints(s *Snapshot) []DayPoint {
	workers := map[string]bool{}
	for _, p := range s.Pods {
		for name := range p.Workers {
			workers[name] = true
		}
		for _, name := range p.Unreached {
			workers[name] = true
		}
	}
	var out []DayPoint
	for _, name := range slices.Sorted(maps.Keys(workers)) {
		merged := DayPoint{Worker: name}
		seen := make(map[string]bool, len(s.Pods))
		for _, p := range s.Pods {
			c, n, e := unitRate(p, name, completedRate)
			f, _, _ := unitRate(p, name, failedRate)
			merged.Completed, merged.Failed = merged.Completed+c, merged.Failed+f
			merged.Contributing, merged.Expected = merged.Contributing+n, merged.Expected+e
			if seen[p.Label] || e == 0 {
				seen[p.Label] = true
				continue
			}
			seen[p.Label] = true
			out = append(out, DayPoint{Worker: name, Label: p.Label, Completed: c, Failed: f, Contributing: n, Expected: e})
		}
		if merged.Expected > 0 {
			out = append(out, merged)
		}
	}
	return out
}

// DayStat sums one view's points over a bucket.
type DayStat struct {
	Polls                     int     // the points with a rate
	Completed, Failed         float64 // their sums
	PeakCompleted, PeakFailed float64 // and highest
	// Fewest of Of units expected contributed at the bucket's worst poll:
	// the one missing the most (a tie, the one with fewer contributing).
	// Of is 0 before the first point.
	Fewest, Of int
}

// Add sums p, a point with an expected unit, into st.
func (st *DayStat) Add(p DayPoint) {
	o := DayStat{Fewest: p.Contributing, Of: p.Expected}
	if p.Contributing > 0 {
		o.Polls, o.Completed, o.Failed, o.PeakCompleted, o.PeakFailed = 1, p.Completed, p.Failed, p.Completed, p.Failed
	}
	st.Merge(o)
}

// Merge sums o, another bucket's stats, into st: the worst poll of both
// is the worse of their worst, by the rule of DayStat, so a merge gives
// what adding all their points would, in any order.
func (st *DayStat) Merge(o DayStat) {
	if o.Of == 0 {
		return
	}
	missing, worst := o.Of-o.Fewest, st.Of-st.Fewest
	if st.Of == 0 || missing > worst || missing == worst && o.Fewest < st.Fewest {
		st.Fewest, st.Of = o.Fewest, o.Of
	}
	st.Polls += o.Polls
	st.Completed += o.Completed
	st.Failed += o.Failed
	st.PeakCompleted, st.PeakFailed = max(st.PeakCompleted, o.PeakCompleted), max(st.PeakFailed, o.PeakFailed)
}

// Day is one worker's DayStats per DayBucket, merged and per unit label:
// a day or more kept in buckets, not in snapshots, which a day of would be
// too many to hold. It is not safe for concurrent use.
type Day struct {
	worker  string
	buckets map[int64]map[string]*DayStat // by the bucket's start (unix seconds), then label
}

// NewDay returns an empty Day of worker.
func NewDay(worker string) *Day {
	return &Day{worker: worker, buckets: make(map[int64]map[string]*DayStat)}
}

// Add sums s's points of the Day's worker into s's bucket. Snapshots may
// come in any order.
func (d *Day) Add(s *Snapshot) {
	start := s.At.Truncate(DayBucket)
	for _, p := range DayPoints(s) {
		if p.Worker == d.worker {
			d.stat(start, p.Label).Add(p)
		}
	}
}

// Put sets the stats of the view label ("" merged) in the bucket starting
// at start: a day read back from where they were stored.
func (d *Day) Put(start time.Time, label string, st DayStat) { *d.stat(start, label) = st }

// stat is the bucket starting at start's stats of the view label, made
// when missing.
func (d *Day) stat(start time.Time, label string) *DayStat {
	b := d.buckets[start.Unix()]
	if b == nil {
		b = make(map[string]*DayStat)
		d.buckets[start.Unix()] = b
	}
	st := b[label]
	if st == nil {
		st = &DayStat{}
		b[label] = st
	}
	return st
}

// ChartStart is where a chart of span up to end starts: its first point's
// step, a little more than span back — what a read for it reads from.
func ChartStart(span time.Duration, end time.Time) time.Time {
	return end.Truncate(StepFor(span)).Add(-span)
}

// Rates charts span up to end, merged ("") or of the unit labelled
// label: one point per StepFor(span), at its start, of the step's average
// completed/s (failed/s with failed), its buckets merged, a gap where none
// was polled; and the span's peak, its highest single poll. A point of a
// chart longer than the day says its date.
func (d *Day) Rates(label string, failed bool, span time.Duration, end time.Time, n Noun) (pts []Point, peak float64) {
	step := StepFor(span)
	from := "15:04"
	if span > DayWindow {
		from = "Mon 2 Jan 15:04"
	}
	last := end.Truncate(step)
	for at := ChartStart(span, end); !at.After(last); at = at.Add(step) {
		when := at.Local().Format(from) + "–" + at.Add(step).Local().Format("15:04")
		var st DayStat
		for b := at; b.Before(at.Add(step)); b = b.Add(DayBucket) {
			if bs := d.buckets[b.Unix()][label]; bs != nil {
				st.Merge(*bs)
			}
		}
		if st.Polls == 0 {
			pts = append(pts, Point{At: at, Gap: true, Title: when + " · no data"})
			continue
		}
		sum, top := st.Completed, st.PeakCompleted
		if failed {
			sum, top = st.Failed, st.PeakFailed
		}
		v := sum / float64(st.Polls)
		peak = max(peak, top)
		pts = append(pts, Point{
			At:      at,
			V:       v,
			Partial: st.Fewest < st.Of,
			Title:   fmt.Sprintf("%s · avg %.1f/s · peak %.1f/s · %d/%d %s", when, v, top, st.Fewest, st.Of, n.Many),
		})
	}
	return pts, peak
}
