package fleet

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/goptics/vartop/internal/api"
	"github.com/goptics/vartop/internal/config"
)

// SettleWait bounds how long an action response waits for the snapshot that
// shows its effect.
const SettleWait = 2 * time.Second

// BalancerActionDeadline bounds the sends of an action through a server's
// balancers, all its attempts together; sends to direct units are bounded
// by the client's timeout only.
const BalancerActionDeadline = 15 * time.Second

// The refusals of what a load balancer cannot carry. OneBalancedMsg is also
// the note shown in place of one balanced unit's controls.
const (
	RestartRefusedMsg = "restart isn't available through a load balancer"
	OneBalancedMsg    = "A load balancer picks which instance answers, so actions here would reach a random one. Use All instances (merged), or list the instances' own addresses in the server config."
)

// Until a snapshot shows what an endpoint is, it may be a balancer, so
// nothing is sent to it: WaitingMsg refuses an action before the server's
// first snapshot, and NotRefreshedMsg is the failure of an endpoint found
// since the latest one.
const (
	WaitingMsg      = "Waiting for the first refresh — try again"
	NotRefreshedMsg = "not refreshed yet — try again"
)

// ActionResult is one pod's outcome of an action. Key is the unit's key and
// Tip the hover text of its label (Pod). Outcome is "ok", "no change" or
// "failed"; Msg, when set, is shown in parentheses after it. Worker names the
// worker when one response covers several (bulk).
type ActionResult struct{ Worker, Pod, Key, Tip, Outcome, Msg string }

// NoChange is what a fanOut fn returns for a unit it skipped, sending
// nothing; it becomes a "no change" result with the error text as Msg.
type NoChange string

func (e NoChange) Error() string { return string(e) }

// ActScope is what an action reaches, as latest shows the server. Clients
// are sorted by endpoint; units are in latest's order.
type ActScope struct {
	Direct      []*api.Client  // the direct units' clients, each sent the action
	Unrefreshed []*api.Client  // clients latest has no unit for, sent nothing
	Balanced    []PodSnapshot  // units of instances behind a balancer, reached through balancers
	Dead        []PodSnapshot  // units of balancer endpoints no instance answered through, sent nothing
	balancers   []*api.Client  // the balancer endpoints' clients
	behind      int            // how many instances latest counts behind balancers, in all
	place       map[string]int // unit key → its index in latest.Pods, the order the pages list units in
}

// ViaBalancer reports whether sc holds a unit behind a load balancer.
func (sc ActScope) ViaBalancer() bool { return len(sc.Balanced) > 0 || len(sc.Dead) > 0 }

// Listed compares units by key the way the pages list them (see
// Snapshot.Pods), units latest lacks last.
func (sc ActScope) Listed(a, b string) int {
	at := func(key string) int {
		if i, ok := sc.place[key]; ok {
			return i
		}
		return len(sc.place)
	}
	return cmp.Compare(at(a), at(b))
}

// ScopeOf is what an action on pod reaches, as latest shows the server:
// every unit when pod is "", else the direct unit whose endpoint is pod. An
// endpoint is a balancer, never a direct unit, when it is a key of
// latest.Behind or the endpoint a balanced unit was reached through, dead
// ones included; it is a direct unit only when it is the key of an
// unbalanced unit of latest. A client that is neither, found since latest,
// may be a balancer too, so it is unrefreshed. A pod naming a balanced unit
// or a balancer is refused with OneBalancedMsg: the balancer would pick the
// instance. n words the error for a pod that is no unit of the server.
func ScopeOf(clients []*api.Client, latest *Snapshot, pod string, n Noun) (ActScope, error) {
	sc := ActScope{place: make(map[string]int)}
	balancer := make(map[string]bool) // endpoint → it is a balancer
	direct := make(map[string]bool)   // endpoint → it is a direct unit
	one := false                      // pod is a balanced unit
	if latest != nil {
		for ep := range latest.Behind {
			balancer[ep] = true
		}
		for i, u := range latest.Pods {
			sc.place[u.Key] = i
			if !u.Balanced {
				direct[u.Key] = true
				continue
			}
			balancer[u.Via] = true
			one = one || u.Key == pod
			if u.Instance != "" {
				sc.Balanced = append(sc.Balanced, u)
			} else {
				sc.Dead = append(sc.Dead, u)
			}
		}
	}
	if pod != "" {
		if one || balancer[pod] {
			return ActScope{}, errors.New(OneBalancedMsg)
		}
		for _, c := range clients {
			switch {
			case c.PrimaryEndpoint() != pod:
			case direct[pod]:
				return ActScope{Direct: []*api.Client{c}}, nil
			default:
				return ActScope{Unrefreshed: []*api.Client{c}}, nil
			}
		}
		return ActScope{}, fmt.Errorf("the selected %s is not %s %s of this server; reload", n.One, n.Article, n.One)
	}
	for _, c := range slices.SortedFunc(slices.Values(clients), func(a, b *api.Client) int {
		return strings.Compare(a.PrimaryEndpoint(), b.PrimaryEndpoint())
	}) {
		switch ep := c.PrimaryEndpoint(); {
		case balancer[ep]:
			sc.balancers = append(sc.balancers, c)
			sc.behind += latest.Behind[ep]
		case direct[ep]:
			sc.Direct = append(sc.Direct, c)
		default:
			sc.Unrefreshed = append(sc.Unrefreshed, c)
		}
	}
	return sc, nil
}

// Sender is one request Act sends about a worker, given its name
// path-escaped: direct on a direct unit's own client, fresh through a
// balancer, on a new connection, reporting the instance that answered.
type Sender struct {
	Direct func(ctx context.Context, c *api.Client, name string) error
	Fresh  func(ctx context.Context, c *api.Client, name string) (string, error)
}

// ActionSender sends action (pause, resume, restart or stop).
func ActionSender(action string) Sender {
	return Sender{
		Direct: func(ctx context.Context, c *api.Client, name string) error { return c.ActionCtx(ctx, name, action) },
		Fresh: func(ctx context.Context, c *api.Client, name string) (string, error) {
			return c.ActionFresh(ctx, name, action)
		},
	}
}

// Act sends send's request about each worker of names to every unit of sc
// and returns, per name, one result per unit, listed as the pages list the
// units (see ActScope.Listed): the direct units' outcomes, the balanced
// units' (see reachUnits), the dead units', failed with their error, and
// the unrefreshed ones', failed with NotRefreshedMsg. Results are labelled
// from units (see UnitsOf); n words their messages.
//
// A send that panics, on whichever goroutine Act fans it out to, is raised
// again on the caller's goroutine, as a *TickPanic, once every send has
// returned: an HTTP handler's recovery gets it, and the process lives on.
func Act(ctx context.Context, sc ActScope, units map[string]PodSnapshot, n Noun, names []string, send Sender) [][]ActionResult {
	results := make([][]ActionResult, len(names))
	var reached [][]ActionResult
	var g panicGroup
	for i, name := range names {
		escaped := url.PathEscape(name)
		g.Go(func() {
			results[i] = fanOut(ctx, sc.Direct, units, n, func(ctx context.Context, c *api.Client) error {
				return send.Direct(ctx, c, escaped)
			})
		})
	}
	if len(sc.Balanced) > 0 {
		g.Go(func() { reached = reachUnits(ctx, sc, units, n, names, send.Fresh) })
	}
	g.Wait()
	for i := range results {
		if reached != nil {
			results[i] = append(results[i], reached[i]...)
		}
		for _, u := range sc.Dead {
			results[i] = append(results[i], outcome(u.Key, units, n, errors.New(u.Err)))
		}
		for _, c := range sc.Unrefreshed {
			results[i] = append(results[i], outcome(c.PrimaryEndpoint(), units, n, errors.New(NotRefreshedMsg)))
		}
		slices.SortStableFunc(results[i], func(a, b ActionResult) int { return sc.Listed(a.Key, b.Key) })
	}
	return results
}

// fanOut calls fn concurrently on every client of targets and returns one
// result per client, in targets' order, labelled from units (see UnitsOf).
// n words the results' messages. A call that panics is raised again once
// every call has returned (see panicGroup).
func fanOut(ctx context.Context, targets []*api.Client, units map[string]PodSnapshot, n Noun,
	fn func(context.Context, *api.Client) error) []ActionResult {
	results := make([]ActionResult, len(targets))
	var g panicGroup
	for i, c := range targets {
		g.Go(func() { results[i] = outcome(c.PrimaryEndpoint(), units, n, fn(ctx, c)) })
	}
	g.Wait()
	return results
}

// reachUnits sends fresh about each of names through sc's balancers, a
// reacher job per name, until the job has a definitive answer (a 2xx, a 409
// or a 404) from the instance of every balanced unit of sc, has spent its
// budget of attemptCap(max(units, instances behind, balancers)) attempts
// (the balancers pick among every instance behind them, not only the units
// wanted there), or BalancerActionDeadline has passed. It returns, per
// name, one result per balanced unit: the outcome of its instance's
// definitive answer, else of its latest other answer, else a failure saying
// it was not reached. Answers from other instances are not results; an
// instance sent the request twice is unchanged by the repeat. One reacher
// carries every name, so jobs side by side do not fall into step behind a
// round-robin balancer.
func reachUnits(ctx context.Context, sc ActScope, units map[string]PodSnapshot, n Noun, names []string,
	fresh func(context.Context, *api.Client, string) (string, error)) [][]ActionResult {
	ctx, cancel := context.WithTimeout(ctx, BalancerActionDeadline)
	defer cancel()
	ids := make([]string, len(sc.Balanced))
	for i, u := range sc.Balanced {
		ids[i] = u.Instance
	}
	r := newReacher(sc.balancers, attemptCap(max(len(ids), sc.behind, len(sc.balancers))))
	jobs := make([]*job[struct{}], len(names))
	for i, name := range names {
		escaped := url.PathEscape(name)
		jobs[i] = addJob(r, func(ctx context.Context, c *api.Client) (struct{}, string, error) {
			instance, err := fresh(ctx, c, escaped)
			return struct{}{}, instance, err
		}, IsUnchanged)
		jobs[i].want(ids...)
	}
	r.run(ctx)

	results := make([][]ActionResult, len(names))
	for i, j := range jobs {
		for _, u := range sc.Balanced {
			a, ok := j.got[u.Instance]
			if !ok {
				a, ok = j.failed[u.Instance]
			}
			if !ok {
				a.Err = fmt.Errorf("not reached through the load balancer after %d tries", j.sent)
			}
			results[i] = append(results[i], outcome(u.Key, units, n, a.Err))
		}
	}
	return results
}

// IsUnchanged reports whether err is varmq saying an action changes nothing
// on that instance: a 409 (already so) or a 404 (it lacks the worker).
func IsUnchanged(err error) bool {
	var apiErr *api.APIError
	return errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusConflict || apiErr.StatusCode == http.StatusNotFound)
}

// outcome is the result of err on the unit key: varmq's 409 ("already in that
// state"), its 404 (the pod lacks the worker: actions fan out to every pod
// in scope) and a skip are no change, any other error is a failure.
func outcome(key string, units map[string]PodSnapshot, n Noun, err error) ActionResult {
	u := units[key]
	u.Key = key // a pod the snapshot lacks is still labelled and tipped by its key
	r := ActionResult{Pod: cmp.Or(u.Label, config.ShortOrigin(key)), Key: key, Tip: u.Tip(), Outcome: "ok"}
	var skip NoChange
	var apiErr *api.APIError
	switch {
	case err == nil:
	case errors.As(err, &skip):
		r.Outcome, r.Msg = "no change", string(skip)
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict:
		r.Outcome, r.Msg = "no change", apiErr.Message
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound:
		r.Outcome, r.Msg = "no change", "not on this "+n.One
	default:
		r.Outcome, r.Msg = "failed", err.Error()
	}
	return r
}

// ClientsFor makes the clients an action on latest's units is sent
// through, with token: one per direct unit, at its key, and one per
// balancer endpoint — a key of latest.Behind, or the endpoint a balanced
// unit was reached through, dead ones included. They are sorted by
// endpoint, each once. With nil (no snapshot yet) there are none.
func ClientsFor(latest *Snapshot, token string) []*api.Client {
	if latest == nil {
		return nil
	}
	endpoints := slices.Collect(maps.Keys(latest.Behind))
	for _, u := range latest.Pods {
		if u.Balanced {
			endpoints = append(endpoints, u.Via)
		} else {
			endpoints = append(endpoints, u.Key)
		}
	}
	slices.Sort(endpoints)
	clients := make([]*api.Client, 0, len(endpoints))
	for _, ep := range slices.Compact(endpoints) {
		clients = append(clients, api.NewClient(ep, token))
	}
	return clients
}

// UnitsOf maps each unit key of snap to the unit, for its label and tip.
func UnitsOf(snap *Snapshot) map[string]PodSnapshot {
	units := make(map[string]PodSnapshot)
	if snap != nil {
		for _, p := range snap.Pods {
			units[p.Key] = p
		}
	}
	return units
}
