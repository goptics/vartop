package web

import (
	"net/url"
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

// dayCharts sets p's day charts, of worker up to end, as p.Ranges asks,
// from the day buckets in the server's file, read through its feed; a day
// that cannot be read is said in the banner, its chart left at the hour.
func (h *handler) dayCharts(p *Page, feed *store.Feed, worker string, end time.Time) {
	if !p.Ranges.Completed && !p.Ranges.Failed {
		return
	}
	day, err := feed.Day(worker, end.Add(-fleet.DayWindow))
	if err != nil {
		p.Banner = fleet.SyncErrMsg(p.Server, err)
		return
	}
	if p.Ranges.Completed {
		pts, peak := day.Rates(p.Pod, false, end, p.Noun)
		p.CompletedDay = &DayChart{pts, peak}
	}
	if p.Ranges.Failed {
		pts, peak := day.Rates(p.Pod, true, end, p.Noun)
		p.FailedDay = &DayChart{pts, peak}
	}
}
