package web

import (
	"cmp"
	"math"
	"strconv"
	"strings"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/fleet"
)

// srcGroup is the live queue group of id on the worker page, if read.
func (p Page) srcGroup(id string) (fleet.QueueGroup, bool) {
	if p.srcq == nil {
		return fleet.QueueGroup{}, false
	}
	return p.srcq.Group(id)
}

// SourcesView is a worker's sources as its page draws them: Nodes, in pick
// order, fanning into the worker. Label says how it picks: the strategy
// in words with two or more sources, "Pulls directly" with one, "" when
// the server does not say (Legacy: varmq before API 1.5.0, the nodes read
// from queue_ids: no strategy, no priorities).
type SourcesView struct {
	Nodes  []SourceNode
	Label  string
	Legacy bool
}

// SourceNode is one source: a queue (Queue, URL to its page), a topic
// subscription, or storage with no Queue ID (Kind queue, no URL). Kind is
// as varmq sent it. Priority is "p N" when the worker picks by priority
// and N was set.
type SourceNode struct {
	Kind     string
	Queue    QueueName // Name cut in the middle (midCut); Full is the whole
	Full     string
	URL      string
	Priority string
	// State: "paused", "empty" or "N pending", of the queue read live;
	// "" when not read.
	State string
}

// Aria is the node's accessible name: full name, kind, priority, state.
func (n SourceNode) Aria() string {
	parts := []string{cmp.Or(n.Full, n.Kind)}
	for _, s := range []string{n.Queue.Kind, n.Priority, n.State} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}

// srcNameMax is the longest source name drawn whole.
const srcNameMax = 28

// midCut is s cut to max runes, the middle replaced by "…": both ends of a
// queue name tell queues apart.
func midCut(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	head := (max - 1) / 2
	return string(r[:head]) + "…" + string(r[len(r)-(max-1-head):])
}

// strategyWords says each strategy varmq sends in words.
var strategyWords = map[string]string{
	"priority":    "Priority: lowest number first",
	"round_robin": "Round robin: each source in turn",
	"max_len":     "Max length: longest queue first",
	"min_len":     "Min length: shortest non-empty first",
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
		if s.QueueID != "" {
			n.Queue, n.URL = p.QName(s.QueueID), jobsURL(s.QueueID, p.Server, "", 0, defaultJobLimit)
			n.Full, n.Queue.Name = n.Queue.Name, midCut(n.Queue.Name, srcNameMax)
			if n.Queue.Inst != "" {
				n.Full += " on " + n.Queue.Inst
			}
			if g, ok := p.srcGroup(s.QueueID); ok {
				switch {
				case g.IsPaused():
					n.State = "paused"
				case g.Counts == nil:
				case g.Counts.Pending == 0:
					n.State = "empty"
				default:
					n.State = strconv.FormatInt(g.Counts.Pending, 10) + " pending"
				}
			}
		}
		if d.Strategy == "priority" && s.Priority != nil && *s.Priority != math.MaxInt64 {
			n.Priority = "p " + strconv.FormatInt(*s.Priority, 10)
		}
		v.Nodes = append(v.Nodes, n)
	}
	switch {
	case v.Legacy: // the server does not say how it picks
	case len(v.Nodes) == 1:
		v.Label = "Pulls directly"
	default:
		v.Label = cmp.Or(strategyWords[d.Strategy], d.Strategy)
	}
	return v
}
