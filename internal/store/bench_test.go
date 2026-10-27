package store

import (
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/goptics/vartop/internal/fleet"
)

// fleetTicks returns n snapshots, interval apart, of a server with units
// units of workers workers each: counters that grow at varied rates, gauges
// that move, a few balanced units and errors, as a busy fleet reports them.
func fleetTicks(n, units, workers int, interval time.Duration) []*fleet.Snapshot {
	rng := rand.New(rand.NewPCG(1, 2))
	names := make([]string, workers)
	for w := range names {
		names[w] = fmt.Sprintf("%s-%s-%d", []string{"email", "report", "billing", "sync", "export"}[w%5],
			[]string{"sender", "builder", "worker", "consumer"}[w%4], w)
	}
	type counter struct {
		completed, failed uint64
		rate              float64
	}
	counters := make([][]counter, units)
	for u := range counters {
		counters[u] = make([]counter, workers)
		for w := range counters[u] {
			counters[u][w] = counter{completed: rng.Uint64N(5_000_000), rate: rng.Float64() * 200}
		}
	}
	const lb = "http://varmq-lb.prod.svc.cluster.local:8080/varmq"
	start := time.UnixMilli(time.Now().Add(-time.Duration(n) * interval).UnixMilli())
	out := make([]*fleet.Snapshot, n)
	for i := range out {
		snapAt := start.Add(time.Duration(i) * interval)
		s := &fleet.Snapshot{At: snapAt, Behind: map[string]int{lb: 3}}
		for u := range units {
			ep := fmt.Sprintf("http://10.42.%d.%d:8080/varmq", u/4, 10+u)
			inst := fmt.Sprintf("api-7d9f8c6b5-%05x", 0x1a2b3+u*7919)
			pod := fleet.PodSnapshot{Key: ep, Via: ep, Instance: inst, Label: fmt.Sprintf("instance-%d", u+1)}
			if u >= units-3 { // the last three behind the balancer
				pod.Key, pod.Via, pod.Balanced = "#"+inst, lb, true
			}
			if (i+u)%97 == 0 {
				pod.Err = "not reached this refresh · last seen 5s ago"
				s.Pods = append(s.Pods, pod)
				continue
			}
			pod.Workers = make(map[string]*fleet.Sample, workers)
			for w, name := range names {
				c := &counters[u][w]
				c.rate = math.Max(0, c.rate+rng.NormFloat64()*5)
				done := uint64(c.rate * interval.Seconds())
				fails := uint64(0)
				if rng.IntN(20) == 0 {
					fails = rng.Uint64N(3)
				}
				c.completed += done
				c.failed += fails
				sm := &fleet.Sample{
					At:            snapAt.Add(time.Duration(20+rng.IntN(300)) * time.Millisecond),
					CompletedRate: float64(done) / interval.Seconds(),
					FailedRate:    float64(fails) / interval.Seconds(),
					RateOK:        i > 0,
				}
				sm.Name, sm.Status, sm.Concurrency = name, "running", 16
				sm.NumProcessing, sm.NumPending, sm.NumIdleWorkers = rng.IntN(17), rng.IntN(5000), rng.IntN(17)
				sm.Instance, sm.Origin = inst, pod.Via
				sm.Metrics.Submitted = c.completed + uint64(sm.NumPending) + uint64(sm.NumProcessing)
				sm.Metrics.Completed, sm.Metrics.Failed = c.completed, c.failed
				sm.Metrics.Successful = c.completed - c.failed
				pod.Workers[name] = sm
			}
			s.Pods = append(s.Pods, pod)
		}
		out[i] = s
	}
	return out
}

// BenchmarkSnapshotSize measures a 10-unit × 20-worker server's blob and
// what each tick adds to its file, and projects a day at a 5s interval.
func BenchmarkSnapshotSize(b *testing.B) {
	const n = 300
	snaps := fleetTicks(n, 10, 20, 5*time.Second)

	dir := b.TempDir()
	_, token := claimed(b, dir)
	db := newWriter(b, dir, "prod")
	empty, err := db.used()
	if err != nil {
		b.Fatal(err)
	}
	for _, s := range snaps {
		if _, err := db.Append(token, fleet.Tick{Snap: s}); err != nil {
			b.Fatal(err)
		}
	}
	if _, err := db.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		b.Fatal(err)
	}
	used, err := db.used()
	if err != nil {
		b.Fatal(err)
	}
	perTick := float64(used-empty) / n

	var blob int
	b.ResetTimer()
	for i := range b.N {
		data, err := encodeSnapshot(snaps[i%n])
		if err != nil {
			b.Fatal(err)
		}
		blob += len(data)
	}
	b.ReportMetric(float64(blob)/float64(b.N), "blob-B/tick")
	b.ReportMetric(perTick, "file-B/tick")
	b.ReportMetric(perTick*float64(24*time.Hour/(5*time.Second))/1e6, "MB/day@5s")
}

// BenchmarkWindowLoad measures loading an hour of a 10 × 20 server polled
// every 5s, as a feed's first Sync does.
func BenchmarkWindowLoad(b *testing.B) {
	snaps := fleetTicks(int(time.Hour/(5*time.Second)), 10, 20, 5*time.Second)
	dir := b.TempDir()
	_, token := claimed(b, dir)
	db := newWriter(b, dir, "prod")
	for _, s := range snaps {
		if _, err := db.Append(token, fleet.Tick{Snap: s}); err != nil {
			b.Fatal(err)
		}
	}
	r, err := OpenServer(dir, "prod", false)
	if err != nil {
		b.Fatal(err)
	}
	defer r.Close()
	since := snaps[0].At

	b.ResetTimer()
	for range b.N {
		got, _, err := r.Window(since)
		if err != nil || len(got) != len(snaps) {
			b.Fatalf("Window = %d snapshots, %v; want %d", len(got), err, len(snaps))
		}
	}
}

// BenchmarkAppend measures one Append of a 10 × 20 server's snapshot, its
// day buckets included (20 workers × 11 views upserted).
func BenchmarkAppend(b *testing.B) {
	snaps := fleetTicks(b.N, 10, 20, 5*time.Second)
	dir := b.TempDir()
	_, token := claimed(b, dir)
	db := newWriter(b, dir, "prod")
	b.ResetTimer()
	for _, s := range snaps {
		if _, err := db.Append(token, fleet.Tick{Snap: s}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDayRead measures reading a worker's full day of buckets of a
// 10 × 20 server, what a 24h chart reads: one snapshot per bucket stored,
// which fills every bucket as a day of polls would.
func BenchmarkDayRead(b *testing.B) {
	snaps := fleetTicks(int(fleet.DayWindow/fleet.DayBucket), 10, 20, fleet.DayBucket)
	dir := b.TempDir()
	_, token := claimed(b, dir)
	db := newWriter(b, dir, "prod")
	for _, s := range snaps {
		if _, err := db.Append(token, fleet.Tick{Snap: s}); err != nil {
			b.Fatal(err)
		}
	}
	r, err := OpenServer(dir, "prod", false)
	if err != nil {
		b.Fatal(err)
	}
	defer r.Close()
	var worker string
	for _, u := range snaps[0].Pods {
		if names := slices.Sorted(maps.Keys(u.Workers)); len(names) > 0 {
			worker = names[0]
			break
		}
	}
	since := snaps[0].At
	b.ResetTimer()
	for range b.N {
		d, err := r.Day(worker, "", since)
		if err != nil {
			b.Fatal(err)
		}
		if pts, _ := d.Rates("", false, fleet.DayWindow, snaps[len(snaps)-1].At, fleet.NounFor(nil)); len(pts) == 0 {
			b.Fatal("no points")
		}
	}
}

// BenchmarkLongRead measures reading one view of a worker's buckets over
// a longer chart's span, and charting it, on a 10 × 20 server whose 30
// days of buckets are written straight in — in time order, as the
// collector writes them, every bucket full, rates fractional — and
// reports the bytes they take: what a 7d or 30d chart reads.
func BenchmarkLongRead(b *testing.B) {
	dir := b.TempDir()
	claimed(b, dir)
	db := newWriter(b, dir, "prod")
	end := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	if _, err := db.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 30 * 288),
		w(j) AS (SELECT 0 UNION ALL SELECT j + 1 FROM w WHERE j < 19),
		l(k) AS (SELECT 0 UNION ALL SELECT k + 1 FROM l WHERE k < 10)
		INSERT INTO buckets SELECT 'worker-' || j, ? - (30 * 288 - i) * 300, CASE k WHEN 0 THEN '' ELSE 'instance-' || k END,
			60, 100.37 + (i % 7) * 0.13, 1.07 + k * 0.01, 3.11 + j * 0.01, 1.13, 10, 10 FROM n, w, l`, end.Unix()); err != nil {
		b.Fatal(err)
	}
	var size int64
	if err := db.db.QueryRow(`SELECT sum(pgsize) FROM dbstat('main', 1) WHERE name = 'buckets'`).Scan(&size); err != nil {
		b.Fatal(err)
	}
	r, err := OpenServer(dir, "prod", false)
	if err != nil {
		b.Fatal(err)
	}
	defer r.Close()
	for _, days := range []int{1, 7, 30} {
		span := time.Duration(days) * fleet.DayWindow
		b.Run(fmt.Sprintf("%dd", days), func(b *testing.B) {
			for range b.N {
				d, err := r.Day("worker-3", "", fleet.ChartStart(span, end))
				if err != nil {
					b.Fatal(err)
				}
				if pts, _ := d.Rates("", false, span, end, fleet.NounFor(nil)); len(pts) == 0 || pts[0].Gap {
					b.Fatal("no first point")
				}
			}
			b.ReportMetric(float64(size)/1e6, "MB-buckets")
		})
	}
}
