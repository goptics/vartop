package web

import (
	"math"
	"strconv"

	"github.com/goptics/vartop/internal/api"
)

// SourcesView is a worker's sources as its page draws them: Nodes, in pick
// order, fanning into the worker. Label says how it picks: the strategy
// in words with two or more sources, "Pulls directly" with one, "" when
// the server does not say (Legacy: varmq before API 1.5.0, the nodes read
// from queue_ids: no strategy, no priorities).
type SourcesView struct {
	Nodes  []SourceNode
	Label  string
	Hint   string // what the strategy means, "" when Label says it all
	Legacy bool
}

// SourceNode is one source: a queue (Queue, URL to its page), a topic
// subscription, or storage with no Queue ID (NoID). Kind is as varmq sent
// it. Priority is "p N" when the worker picks by priority and N was set.
type SourceNode struct {
	Kind     string
	Queue    QueueName
	URL      string
	NoID     bool
	Priority string
}

// strategyWords names each strategy varmq sends, and what it means.
var strategyWords = map[string][2]string{
	"priority":    {"Priority", "lowest number first"},
	"round_robin": {"Round robin", "each source in turn"},
	"max_len":     {"Max length", "longest queue first"},
	"min_len":     {"Min length", "shortest non-empty first"},
}

// WorkerSources is the sources of the page's worker, nil with none to show
// (none reported, or a stale page: no current value).
func (p Page) WorkerSources() *SourcesView {
	d := p.Detail
	if d == nil || p.Stale != "" {
		return nil
	}
	v := &SourcesView{}
	srcs := d.Sources
	if srcs == nil {
		v.Legacy = true
		for _, id := range d.QueueIDs {
			srcs = append(srcs, api.WorkerSource{QueueID: id, Kind: "queue"})
		}
	}
	if len(srcs) == 0 {
		return nil
	}
	for _, s := range srcs {
		n := SourceNode{Kind: s.Kind}
		switch {
		case s.QueueID != "":
			n.Queue, n.URL = p.QName(s.QueueID), jobsURL(s.QueueID, p.Server, "", 0, defaultJobLimit)
		case s.Kind == "queue":
			n.NoID = true
		}
		if d.Strategy == "priority" && s.Priority != nil && *s.Priority != math.MaxInt64 {
			n.Priority = "p " + strconv.FormatInt(*s.Priority, 10)
		}
		v.Nodes = append(v.Nodes, n)
	}
	switch w, known := strategyWords[d.Strategy]; {
	case len(v.Nodes) == 1:
		v.Label = "Pulls directly"
	case d.Strategy == "":
	case known:
		v.Label, v.Hint = w[0], w[1]
	default:
		v.Label = d.Strategy
	}
	return v
}
