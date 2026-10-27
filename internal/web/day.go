package web

import (
	"fmt"
	"net/url"
	"time"

	"github.com/goptics/vartop/internal/fleet"
	"github.com/goptics/vartop/internal/store"
)

// spansOf is the spans besides the hour a worker page's charts may show,
// shortest first, on a server of history: 24h, 7d and 30d as far as it
// reaches, and the history itself when none of those (a 14d, say).
func spansOf(history time.Duration) []time.Duration {
	var spans []time.Duration
	for _, span := range []time.Duration{fleet.DayWindow, 7 * fleet.DayWindow, 30 * fleet.DayWindow} {
		if span <= history {
			spans = append(spans, span)
		}
	}
	if history > spans[len(spans)-1] {
		spans = append(spans, history)
	}
	return spans
}

// spanParam is span as a page's address and switch say it: 24h, then
// days — 7d.
func spanParam(span time.Duration) string {
	if span == fleet.DayWindow {
		return "24h"
	}
	return fmt.Sprintf("%dd", span/fleet.DayWindow)
}

// ChartRanges is the span each chart of a worker page shows: 0 the hour,
// else one of its server's spans (spansOf).
type ChartRanges struct{ Completed, Failed time.Duration }

// rangesOf reads a page's completed= and failed= among spans; one not
// among them (past the history, made up) is the hour.
func rangesOf(q url.Values, spans []time.Duration) ChartRanges {
	span := func(v string) time.Duration {
		for _, s := range spans {
			if spanParam(s) == v {
				return s
			}
		}
		return 0
	}
	return ChartRanges{Completed: span(q.Get("completed")), Failed: span(q.Get("failed"))}
}

// addTo is the page address u with r's parameters, so that the poller,
// the pickers and the other chart's switch keep each chart's choice.
func (r ChartRanges) addTo(u string) string {
	return withParam(withParam(u, "completed", r.Completed), "failed", r.Failed)
}

// withParam is the page address u with name set to span, or without it
// for the hour (0).
func withParam(u, name string, span time.Duration) string {
	pu, err := url.Parse(u)
	if err != nil {
		return u
	}
	q := pu.Query()
	if span > 0 {
		q.Set(name, spanParam(span))
	} else {
		q.Del(name)
	}
	pu.RawQuery = q.Encode()
	return pu.RequestURI()
}

// DayChart is a worker page chart longer than the hour: a point per Step
// (fleet.StepFor) of Span, and its highest single poll.
type DayChart struct {
	Points     []fleet.Point
	Peak       float64
	Span, Step time.Duration
}

// switchLink is one choice of a chart's span switch.
type switchLink struct {
	Text, URL string
	Current   bool
}

// rangeSwitch is the 1h | 24h | … switch of the chart named chart
// ("completed" or "failed") of page p: links to p with only that chart's
// span changed, one per span of p's server.
func rangeSwitch(p Page, chart string) []switchLink {
	cur := p.Ranges.Completed
	if chart == "failed" {
		cur = p.Ranges.Failed
	}
	links := []switchLink{{Text: "1h", URL: withParam(p.SelfURL, chart, 0), Current: cur == 0}}
	for _, span := range p.Spans {
		links = append(links, switchLink{Text: spanParam(span), URL: withParam(p.SelfURL, chart, span), Current: cur == span})
	}
	return links
}

// dayCharts sets p's charts longer than the hour, of worker up to end, as
// p.Ranges asks, from the day buckets of p's view in the server's file,
// read once through its feed for both; a span that cannot be read is said
// in the banner, its chart left at the hour.
func (h *handler) dayCharts(p *Page, feed *store.Feed, worker string, end time.Time) {
	longest := max(p.Ranges.Completed, p.Ranges.Failed)
	if longest == 0 {
		return
	}
	day, err := feed.Day(worker, p.Pod, fleet.ChartStart(longest, end))
	if err != nil {
		p.Banner = fleet.SyncErrMsg(p.Server, err)
		return
	}
	chart := func(span time.Duration, failed bool) *DayChart {
		if span == 0 {
			return nil
		}
		pts, peak := day.Rates(p.Pod, failed, span, end, p.Noun)
		return &DayChart{Points: pts, Peak: peak, Span: span, Step: fleet.StepFor(span)}
	}
	p.CompletedDay, p.FailedDay = chart(p.Ranges.Completed, false), chart(p.Ranges.Failed, true)
}
