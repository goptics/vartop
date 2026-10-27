package fleet

import (
	"fmt"
	"time"
)

const (
	// DayWindow is how far back the 24h charts reach: the store's retention.
	DayWindow = 24 * time.Hour
	// DayBucket is the span one point of a 24h chart sums up: 288 a day.
	DayBucket = 5 * time.Minute
)

// Day is one worker's completed/s and failed/s per DayBucket, merged and
// per unit label, summed from the snapshots added to it: the day kept in
// buckets, not in snapshots, which a day of would be too many to hold. It
// is not safe for concurrent use.
type Day struct {
	worker  string
	buckets map[int64]*dayBucket // by the bucket's start, unix seconds
}

// dayBucket is one DayBucket of a Day: the merged view's sums, and each
// label's.
type dayBucket struct {
	merged dayStat
	units  map[string]*dayStat
}

// dayStat sums the rate points of one view over a bucket.
type dayStat struct {
	polls             int     // the points with a rate
	completed, failed float64 // their sums
	peakC, peakF      float64 // and highest
	// fewest of expected units contributed at the bucket's worst poll,
	// counting those with no rate at all; seen once one poll expected one.
	fewest, of int
	seen       bool
}

// NewDay returns an empty Day of worker.
func NewDay(worker string) *Day {
	return &Day{worker: worker, buckets: make(map[int64]*dayBucket)}
}

// Add sums s into its bucket: the units' rates merged, and each label's
// (its first unit's, as PodsOf takes). Snapshots may come in any order.
func (d *Day) Add(s *Snapshot) {
	key := s.At.Truncate(DayBucket).Unix()
	b := d.buckets[key]
	if b == nil {
		b = &dayBucket{units: make(map[string]*dayStat)}
		d.buckets[key] = b
	}
	var c, f float64
	var contributing, expected int
	seen := make(map[string]bool, len(s.Pods))
	for _, p := range s.Pods {
		uc, n, e := unitRate(p, d.worker, completedRate)
		uf, _, _ := unitRate(p, d.worker, failedRate)
		c, f, contributing, expected = c+uc, f+uf, contributing+n, expected+e
		if seen[p.Label] {
			continue // PodsOf takes the first
		}
		seen[p.Label] = true
		st := b.units[p.Label]
		if st == nil {
			st = &dayStat{}
			b.units[p.Label] = st
		}
		st.add(uc, uf, n, e)
	}
	b.merged.add(c, f, contributing, expected)
}

// add sums one rate point of contributing of the expected units.
func (st *dayStat) add(c, f float64, contributing, expected int) {
	if expected == 0 {
		return // the worker is not on these units
	}
	if !st.seen || contributing < st.fewest {
		st.fewest, st.of, st.seen = contributing, expected, true
	}
	if contributing == 0 {
		return
	}
	st.polls++
	st.completed += c
	st.failed += f
	st.peakC, st.peakF = max(st.peakC, c), max(st.peakF, f)
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
		st := d.stat(at.Unix(), label)
		if st == nil || st.polls == 0 {
			pts = append(pts, Point{At: at, Gap: true, Title: span + " · no data"})
			continue
		}
		sum, top := st.completed, st.peakC
		if failed {
			sum, top = st.failed, st.peakF
		}
		v := sum / float64(st.polls)
		peak = max(peak, top)
		pts = append(pts, Point{
			At:      at,
			V:       v,
			Partial: st.fewest < st.of,
			Title:   fmt.Sprintf("%s · avg %.1f/s · peak %.1f/s · %d/%d %s", span, v, top, st.fewest, st.of, n.Many),
		})
	}
	return pts, peak
}

// stat is the bucket at key's sums of the view label ("" merged), or nil.
func (d *Day) stat(key int64, label string) *dayStat {
	b := d.buckets[key]
	switch {
	case b == nil:
		return nil
	case label == "":
		return &b.merged
	}
	return b.units[label]
}
