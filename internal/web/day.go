package web

import (
	"errors"
	"io/fs"
	"net/url"
	"sync"
	"time"

	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/store"
)

// dayParam is the value of a worker page's completed and failed
// parameters that has that chart show the day; without it, the hour.
const dayParam = "24h"

// ChartRanges says which charts of a worker page show the day.
type ChartRanges struct{ Completed, Failed bool }

// rangesOf reads a page's completed=24h and failed=24h.
func rangesOf(q url.Values) ChartRanges {
	return ChartRanges{Completed: q.Get("completed") == dayParam, Failed: q.Get("failed") == dayParam}
}

// addTo is the page address u with r's parameters, so that the poller,
// the pickers and the other chart's switch keep each chart's choice.
func (r ChartRanges) addTo(u string) string {
	return withParam(withParam(u, "completed", r.Completed), "failed", r.Failed)
}

// withParam is the page address u with name=24h when day, without it
// when not.
func withParam(u, name string, day bool) string {
	pu, err := url.Parse(u)
	if err != nil {
		return u
	}
	q := pu.Query()
	if day {
		q.Set(name, dayParam)
	} else {
		q.Del(name)
	}
	pu.RawQuery = q.Encode()
	return pu.RequestURI()
}

// DayChart is a worker page chart's day: a point per fleet.DayBucket, and
// the day's highest single poll.
type DayChart struct {
	Points []fleet.Point
	Peak   float64
}

// switchLink is one choice of a chart's 1h | 24h switch.
type switchLink struct {
	Text, URL string
	Current   bool
}

// rangeSwitch is the 1h | 24h switch of the chart named chart
// ("completed" or "failed") of page p: links to p with only that chart's
// choice changed.
func rangeSwitch(p Page, chart string) []switchLink {
	day := p.Ranges.Completed
	if chart == "failed" {
		day = p.Ranges.Failed
	}
	return []switchLink{
		{Text: "1h", URL: withParam(p.SelfURL, chart, false), Current: !day},
		{Text: dayParam, URL: withParam(p.SelfURL, chart, true), Current: day},
	}
}

// dayCache keeps a fleet.Day per server and worker for the 24h charts:
// read from the server's file once, then topped up from the newest row it
// read. One no request read for feedIdle is dropped, its file let go.
//
// ponytail: a Day per worker, not per server: memory stays one worker's
// day (288 buckets × its units), at the cost of a pass over the file per
// worker first shown at 24h. One Day of every worker per server is the
// upgrade if those passes add up.
type dayCache struct {
	dataDir string

	mu   sync.Mutex
	days map[dayKey]*cachedDay
}

type dayKey struct{ server, worker string }

type cachedDay struct {
	mu     sync.Mutex
	day    *fleet.Day
	db     *store.ServerDB // nil until the file is there
	newest int64           // the newest row read
	used   time.Time       // guarded by dayCache.mu
}

func newDayCache(dataDir string) *dayCache {
	return &dayCache{dataDir: dataDir, days: make(map[dayKey]*cachedDay)}
}

// charts returns the day up to end of worker on the server keyed server,
// merged ("") or of the unit labelled label, as read at now: the
// Completed chart's when completed, the Failed chart's when failed.
func (c *dayCache) charts(server, worker, label string, end, now time.Time, n fleet.Noun, completed, failed bool) (cd, fd *DayChart, err error) {
	d := c.get(server, worker, now)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.topUp(c.dataDir, server, worker, now); err != nil {
		return nil, nil, err
	}
	if completed {
		pts, peak := d.day.Rates(label, false, end, n)
		cd = &DayChart{pts, peak}
	}
	if failed {
		pts, peak := d.day.Rates(label, true, end, n)
		fd = &DayChart{pts, peak}
	}
	return cd, fd, nil
}

// get returns the day of worker on the server keyed server, first letting
// go of those idle for feedIdle. One in use is never idle: its used is
// set here, under the cache's lock.
func (c *dayCache) get(server, worker string, now time.Time) *cachedDay {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, d := range c.days {
		if now.Sub(d.used) > feedIdle {
			d.close()
			delete(c.days, k)
		}
	}
	k := dayKey{server, worker}
	d := c.days[k]
	if d == nil {
		d = &cachedDay{day: fleet.NewDay(worker)}
		c.days[k] = d
	}
	d.used = now
	return d
}

// topUp adds the rows stored since it last read, within the day before
// now, and drops the buckets that left it. A file emptied or made anew
// (its newest row below the one last read) is read afresh.
func (d *cachedDay) topUp(dataDir, server, worker string, now time.Time) error {
	since := now.Add(-fleet.DayWindow)
	if d.db == nil {
		db, err := store.OpenServer(dataDir, server, false)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // nothing stored yet: an empty day
		}
		if err != nil {
			return err
		}
		d.db = db
	}
	newest, err := d.db.Each(d.newest, since, d.day.Add)
	if err == nil && newest < d.newest {
		d.day = fleet.NewDay(worker)
		newest, err = d.db.Each(0, since, d.day.Add)
	}
	if err != nil {
		return err
	}
	d.newest = newest
	d.day.Prune(since)
	return nil
}

// close lets the day's file go.
func (d *cachedDay) close() {
	if d.db != nil {
		_ = d.db.Close()
	}
}

// closeAll lets every day's file go.
func (c *dayCache) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, d := range c.days {
		d.close()
		delete(c.days, k)
	}
}

// dayCharts sets p's day charts, of worker up to end, as p.Ranges asks;
// a day that cannot be read is said in the banner, its chart left at the
// hour.
func (h *handler) dayCharts(p *Page, worker string, end time.Time) {
	if !p.Ranges.Completed && !p.Ranges.Failed {
		return
	}
	cd, fd, err := h.days.charts(p.Server, worker, p.Pod, end, time.Now(), p.Noun, p.Ranges.Completed, p.Ranges.Failed)
	if err != nil {
		p.Banner = fleet.SyncErrMsg(p.Server, err)
		return
	}
	p.CompletedDay, p.FailedDay = cd, fd
}
