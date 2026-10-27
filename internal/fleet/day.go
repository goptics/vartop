package fleet

import (
	"fmt"
	"maps"
	"slices"
	"time"
)

const (
	// DayWindow is how far back the 24h charts reach: the store's retention.
	DayWindow = 24 * time.Hour
	// DayBucket is the span one point of a 24h chart sums up: 288 a day.
	DayBucket = 5 * time.Minute
)

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
	missing, worst := p.Expected-p.Contributing, st.Of-st.Fewest
	if st.Of == 0 || missing > worst || missing == worst && p.Contributing < st.Fewest {
		st.Fewest, st.Of = p.Contributing, p.Expected
	}
	if p.Contributing == 0 {
		return
	}
	st.Polls++
	st.Completed += p.Completed
	st.Failed += p.Failed
	st.PeakCompleted, st.PeakFailed = max(st.PeakCompleted, p.Completed), max(st.PeakFailed, p.Failed)
}

// Day is one worker's DayStats per DayBucket, merged and per unit label:
// the day kept in buckets, not in snapshots, which a day of would be too
// many to hold. It is not safe for concurrent use.
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

// Prune drops the buckets that end at or before cut.
func (d *Day) Prune(cut time.Time) {
	for key := range d.buckets {
		if !time.Unix(key, 0).Add(DayBucket).After(cut) {
			delete(d.buckets, key)
		}
	}
}

// Rates charts the day up to end, merged ("") or of the unit labelled
// label: one point per bucket, at its start, of the bucket's average
// completed/s (failed/s with failed), a gap where none was polled; and the
// day's peak, its highest single poll.
func (d *Day) Rates(label string, failed bool, end time.Time, n Noun) (pts []Point, peak float64) {
	last := end.Truncate(DayBucket)
	for at := last.Add(-DayWindow); !at.After(last); at = at.Add(DayBucket) {
		span := at.Local().Format("15:04") + "–" + at.Add(DayBucket).Local().Format("15:04")
		st := d.buckets[at.Unix()][label]
		if st == nil || st.Polls == 0 {
			pts = append(pts, Point{At: at, Gap: true, Title: span + " · no data"})
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
			Title:   fmt.Sprintf("%s · avg %.1f/s · peak %.1f/s · %d/%d %s", span, v, top, st.Fewest, st.Of, n.Many),
		})
	}
	return pts, peak
}
