# Web UI: Instances Behind a Load Balancer — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every varmq instance behind a balancer URL becomes its own unit (instance-1, instance-2, …) with stable labels, rates and a merged view; "pod" wording stays for Kubernetes only; merged actions reach every instance behind a balancer.

**Architecture:** The poller's unit changes from "endpoint" to "instance": `PodSnapshot.Endpoint` becomes `Key` (endpoint URL for a direct unit, `#`+instance ID for a balanced one). A poller-owned `tracker` remembers which instances answered which endpoint within a window, classifies endpoints as direct or balancer, numbers labels, and drives `reach`, a helper that repeats a request through balancer endpoints on new connections until every wanted instance has answered. Views and actions work on units; copy comes from a `Noun` chosen per server.

**Tech Stack:** Go 1.26 stdlib (`net/http`, `html/template`, `httptest`), htmx 2.0.4 (vendored, unchanged). No new dependencies.

**Spec:** `docs/superpowers/specs/2026-10-26-web-ui-instances-design.md` (amends `docs/superpowers/specs/2026-10-25-web-ui-design.md`).

## Global Constraints

- Go 1.26, stdlib only; no new module dependencies. `internal/tui` and `cmd/` untouched.
- `internal/api` changes are additive: existing `Client` methods keep their behaviour (the TUI uses them).
- Balanced unit key = `"#" + instanceID`. Direct unit key = the endpoint URL. Query parameter stays `pod`.
- Lifecycle window = `max(time.Minute, 3*interval)`.
- Attempt cap `attemptCap(k) = ceil(k*(ln k + 4)) + 2` with k ≥ 1 (k=1 → 6, k=3 → 18, k=10 → 66), computed once per tick (or per action) from `k = max(known balanced units, balancer endpoints)`.
- Non-Kubernetes labels: `fmt.Sprintf("instance-%d", n)`, n from 1, given in first-seen order (new keys of one tick in ascending key order), never reused within a session. Kubernetes labels as today.
- Nouns: Kubernetes server → `Noun{One: "pod", Many: "pods", Article: "a"}`; every other server → `Noun{One: "instance", Many: "instances", Article: "an"}`. `Cap()` = "Pod"/"Instance", `CapMany()` = "Pods"/"Instances".
- Exact copy (with `{one}`, `{many}`, `{One}`, `{Many}` from the Noun):
  - picker `{One}`, option `All {many} (merged)`; nav and page title `{One} grid`
  - health `healthy / total {many}` (title) and `{h}/{t} {many}`
  - `Pending (max/{one})` (card, column header) and point tooltip `{time} · {n} pending (max/{one})`
  - `Concurrency (per {one})`; `{n} ×{p} {many}`; `absent on {k} {many}`; KPI subtitle `{one} × worker`
  - rate tooltip `{time} · {v}{unit} · {k}/{n} {many}`
  - `{Many} reporting:`; `per {one}, on {scope}`; aria `Concurrency per {one}` and `Concurrency per {one}, exact`; scope `all {many}`
  - `{action} each worker on each {one} below:`; `not on this {one}`; `No {many} were targeted.`; `No {many} to show.`
  - `the selected {one} is not {article} {one} of this server; reload`; `{One} {label} is no longer reported — reload`; detail not found `Worker %q not found on any {one}`; 404 outcome msg `not on this {one}`
- Balancer copy: note `{n} instances behind {host:port}` (several joined with ` · `, endpoints sorted); unit error `not reached this refresh · last seen {age} ago` (`age` = `time.Duration.Round(time.Second).String()`); grid cell / tip `not reached`; action failure msg `not reached through the load balancer after {n} tries`; restart refusal `restart isn't available through a load balancer`; one-unit refusal and note `A load balancer picks which instance answers, so actions here would reach a random one. Use All instances (merged), or list the instances' own addresses in the server config.`
- Before editing an existing symbol: `npx -y gitnexus@latest impact <symbol> --direction upstream --repo .` (refresh first with `npx -y gitnexus@latest analyze --index-only` when HEAD moved since the last index); report HIGH/CRITICAL risk in your report. Renames of existing symbols use `gopls rename`, never find-and-replace.
- Before each commit: `npx -y gitnexus@latest detect-changes --scope all --repo .`; `gofmt -l .` empty; `go vet ./...` and `go test -race ./...` green.
- Never stage `AGENTS.md`, `CLAUDE.md` or `.claude/`. Commit messages are conventional (`feat(web): …`, `feat(api): …`) and end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. A balancer answering some attempts with a non-varmq error (502 page, no `X-Varmq-Instance`): those attempts must not count as an instance's answer, and the units that did answer stay healthy. → Task 4 `TestPollBalancerIgnoresAnswersWithoutInstance`.
2. varmq without the instance header behind a balancer: vartop cannot see the instances; it must stay one direct unit with no note and no panic. → Task 4 `TestPollNoInstanceHeaderStaysDirect`.
3. Tick deadline reached mid balancer poll (a slow instance): the snapshot is still appended from the answers so far, the slow instance's unit is errored or its workers not reached, and no goroutine outlives the tick. → Task 4 `TestPollBalancerDeadline`.
4. An action through a balancer while one instance has gone: bounded by the cap, that unit gets `failed (not reached through the load balancer after {n} tries)`, the rest `ok`. → Task 5 `TestActionBalancedUnreachableInstance`.
5. Worker names needing escaping (`a/b`, `x y`) on the new-connection paths. → Task 1 `TestFreshMethodsEscapeNothing` and Task 4 `TestPollBalancerEscapesWorkerNames`.

---

### Task 1: API calls on new connections that report the answering instance

**Files:**
- Modify: `internal/api/client.go`
- Test: `internal/api/client_test.go`

**Interfaces:**
- Consumes: existing `Client`, `APIError`, `InstanceHeader`, `WorkerSummary`, `WorkerDetails`.
- Produces:
  - `func (c *Client) ListWorkersFresh(ctx context.Context) ([]WorkerSummary, string, error)`
  - `func (c *Client) GetWorkerFresh(ctx context.Context, name string) (*WorkerDetails, string, error)` — `name` already path-escaped
  - `func (c *Client) ActionFresh(ctx context.Context, workerName, action string) (string, error)`
  - `func (c *Client) TuneConcurrencyFresh(ctx context.Context, workerName string, n int) (string, error)`
  - The `string` is the reply's `X-Varmq-Instance` ("" without a reply or header), set on non-2xx replies too (then `err` is an `*APIError`). Each request dials a new TCP connection. Answers are also recorded like today's (`recordInstance`), and the returned values (Origin, Instance fields, errors) match the pooled variants.

- [ ] **Step 1: Write the failing tests** in `internal/api/client_test.go`

```go
// TestFreshMethodsUseNewConnections: an httptest server counting http.StateNew
// via srv.Config.ConnState (set before srv.Start on an httptest.NewUnstartedServer).
//   3× ListWorkersCtx            → 1 new connection
//   then 3× ListWorkersFresh     → 3 more (4 total)
//   then GetWorkerFresh, ActionFresh, TuneConcurrencyFresh → 3 more (7 total)

// TestFreshMethodsReportInstance: server sets X-Varmq-Instance: "a".
//   _, inst, err := c.ListWorkersFresh(ctx)      → inst == "a", err == nil
//   _, inst, err = c.GetWorkerFresh(ctx, "gone") (server 404 {"error":"worker not found"})
//       → inst == "a", errors.As(err, &apiErr) && apiErr.StatusCode == 404
//   inst, err = c.ActionFresh(ctx, "w", "pause") (409) → inst == "a", apiErr 409
//   inst, err = c.TuneConcurrencyFresh(ctx, "w", 3)    → inst == "a", PATCH path ".../workers/w/config/concurrency/3"
//   c.SeenInstances() == 1 and c.LastInstance() == "a"

// TestFreshMethodsNoReply: client for "http://127.0.0.1:1" (refused)
//   _, inst, err := c.ListWorkersFresh(ctx) → inst == "", err != nil

// TestFreshMethodsEscapeNothing: GetWorkerFresh(ctx, url.PathEscape("a/b")) and
//   ActionFresh(ctx, url.PathEscape("x y"), "stop") reach EscapedPath
//   "/workers/a%2Fb" and "/workers/x%20y/actions/stop" (use recordedRequest.EscapedPath)
```

- [ ] **Step 2: Run** `go test ./internal/api -run 'TestFresh' -v` — expected FAIL (undefined methods).
- [ ] **Step 3: Implement** in `internal/api/client.go`: a second `*http.Client` on `Client` (`Timeout: 10 * time.Second`, `Transport`: `http.DefaultTransport.(*http.Transport).Clone()` with `DisableKeepAlives = true`), built in `NewClient`; route `doRequest` through a helper that takes the `*http.Client` so pooled and fresh share header, auth and `recordInstance` logic; read the instance from the response header before `handleResponse`.
- [ ] **Step 4: Run** `go test -race ./internal/api ./internal/tui` — expected PASS.
- [ ] **Step 5: Commit** `feat(api): requests on new connections that report the answering instance`.

---

### Task 2: Units in snapshots and views

Turns the endpoint-keyed model into units without changing what the poller fetches yet (every unit is still a direct endpoint after this task).

**Files:**
- Modify: `internal/web/history.go`, `internal/web/view.go`, `internal/web/render.go`, `internal/web/server.go`, `internal/web/actions.go`, `internal/web/poller.go`, `internal/web/templates/{layout,dashboard,grid,detail,confirm,results}.html`, `internal/web/static/app.css`
- Test: `internal/web/{history,view,render,server,actions,poller}_test.go`

**Interfaces:**
- Consumes: Task 1 not needed.
- Produces (later tasks rely on these exact names):
  - `PodSnapshot` fields: `Key string` (renamed from `Endpoint` with gopls), `Via string` (endpoint reached through), `Instance string`, `Label string`, `Balanced bool`, `Err string`, `Workers map[string]*Sample`, `Unreached []string` (sorted worker names on this unit's list that got no answer). `LoadBalanced` removed.
  - `func (p PodSnapshot) Tip() string` — `Instance + " via " + Via` when both set, else `Via`, else `Key`.
  - `Snapshot` gains `Behind map[string]int` (balancer endpoint → distinct instances that answered through it this tick); `Pods` sorted by `Key`.
  - `History.Append` keys rate bases by `Key` + `"\x00"` + worker.
  - `PodColumn{Key, Label, Err, Tip string}`, `PodOption{Key, Label, Tip string}` (renamed `Endpoint` → `Key`, `Tip` added); `ActionResult{Worker, Pod, Key, Tip, Outcome, Msg string}` (`Endpoint` renamed `Key`, `Tip` added).
  - `GridCell` gains `Unreached bool`.
  - `func BalancerNote(latest *Snapshot) string` replaces `LBWarning`; `Page.LBWarning` becomes `Page.Note`.
  - `RowView.RateNA` and `merged.allLB` removed.

- [ ] **Step 1: Rename** `PodSnapshot.Endpoint` → `Key`, `PodColumn.Endpoint` → `Key`, `PodOption.Endpoint` → `Key`, `ActionResult.Endpoint` → `Key` with `gopls rename` (run impact on each first). `go build ./... && go test ./internal/web` must still pass before any behaviour change.
- [ ] **Step 2: Write the failing tests**

```go
// history_test.go
// TestAppendKeysRatesByUnitKey: two units with Via == "http://lb" and Keys "#a", "#b",
// same worker "w"; tick1 a.Completed=10, b=100; tick2 (+interval) a=20, b=130
//   → a.CompletedRate == 10/interval.Seconds(), b == 30/interval.Seconds(), both RateOK

// view_test.go
// TestUnreachedIsNotAbsent: units u1 (healthy, has "w"), u2 (healthy, Unreached ["w"])
//   BuildDashboard: row "w" AbsentOn == 0; StatusTip contains "u2: not reached"
//   BuildGrid: cell for u2 has Unreached == true, Present == false
//   workerRates over [snap] for "w": point Partial == true, Title ends " · 1/2 pods"
// TestFleetRatesUnreachedMakesPartial: u1 has a valid rate and Unreached ["x"]; u2 healthy
//   with a valid rate → point not Gap, Partial == true, V == u1+u2 rates
// TestBalancerNote: Behind {"http://10.0.0.1:8090/varmq": 3, "http://10.0.0.2:8090/varmq": 2}
//   → "3 instances behind 10.0.0.1:8090 · 2 instances behind 10.0.0.2:8090"; nil/empty → ""
// TestTip: {Instance:"a", Via:"http://x"} → "a via http://x"; {Via:"http://x"} → "http://x";
//   {Key:"#a"} → "#a"
// TestNoLoadBalancedExclusion: replace the tests that asserted LB exclusion/RateNA/LBWarning
//   (delete them; RateNA and LBWarning no longer exist)

// render_test.go / server_test.go
// grid page with an unreached cell renders `<span class="na">not reached</span>`;
// header renders Page.Note in `<p class="note" role="note">`; option/column/result
// title attributes carry Tip
```

- [ ] **Step 3: Run** `go test ./internal/web` — expected FAIL on the new tests.
- [ ] **Step 4: Implement.**
  - Unreached rules (view.go, file comment updated): `workerNames` includes names in healthy units' `Unreached`; `mergeWorker` treats an unreached name as neither reporting nor absent and adds `"{label}: not reached"` to `statusTips`; `workerRates` counts a healthy unit whose `Unreached` holds the name as expected, not contributing; `fleetRates` counts a healthy unit with workers as expected, adds its valid rates, counts it contributing only when it has a valid rate **and** an empty `Unreached`, and the point is `Partial` when `contributing < expected` (a point is a gap only when nothing contributed and no rate was summed); `BuildGrid` sets `Unreached` cells; confirm `Absent` is false for an unreached name.
  - Remove every `LoadBalanced` branch, `RateNA`, `allLB`, `LBWarning`; the dashboard trend cell always draws the sparkline.
  - Poller (minimal): `pollPod` sets `Key` and `Via` to the endpoint and `Instance` to the list reply's instance (`list[0].Instance`, or `c.LastInstance()` for an empty list); `pollOnce` stops reading `pool.LoadBalanced()`; sort by `Key`; `dedupeLabels` falls back on `Via` then `Key`.
  - Templates: titles use `.Tip`; grid unreached cell `<span class="na">not reached</span>`; `#status` note `{{if .Note}}<p class="note" role="note">{{.Note}}</p>{{end}}`; `.note` styled muted (no warning colour) in `app.css`.
- [ ] **Step 5: Run** `go test -race ./...`, `go vet ./...`, `gofmt -l .` — expected clean.
- [ ] **Step 6: Commit** `feat(web): snapshots carry units with keys, tips and unreached workers`.

---

### Task 3: Pod or instance wording

**Files:**
- Modify: `internal/web/render.go`, `internal/web/view.go`, `internal/web/server.go`, `internal/web/actions.go`, `internal/web/templates/*.html`
- Test: `internal/web/{render,view,server,actions}_test.go`

**Interfaces:**
- Consumes: Task 2 types.
- Produces:
  - `type Noun struct{ One, Many, Article string }`; `func (n Noun) Cap() string`; `func (n Noun) CapMany() string`; `func NounFor(srv *config.Server) Noun` (nil or non-Kubernetes → instance noun).
  - `Page.Noun Noun`; `ConfirmView.Noun Noun`; results template data becomes `type ResultsView struct{ Results []ActionResult; Noun Noun }`.
  - `func BuildDashboard(snaps []*Snapshot, pod string, interval time.Duration, n Noun) DashboardView`
  - `func BuildDetail(snaps []*Snapshot, pod, worker string, interval time.Duration, n Noun) (DetailView, bool)`
  - `Page.Scope()` returns `"all " + p.Noun.Many` or the unit label.

- [ ] **Step 1: Write the failing tests**

```go
// TestNounFor: Kubernetes server → {"pod","pods","a"}; static, DNS, nil → {"instance","instances","an"};
//   Cap/CapMany → "Pod"/"Pods", "Instance"/"Instances"
// TestPagesSayInstanceOnStaticServers: render dashboard, grid, detail (merged) for a static
//   server; body contains "All instances (merged)", ">Instance grid<", "/3 instances",
//   "Pending (max/instance)", "Concurrency (per instance)", "instance × worker",
//   "Instances reporting:", "per instance, on all instances", "4 ×3 instances";
//   body has no "pod" outside attribute values/URLs (assert !strings.Contains(text, "pod")
//   on the tag-stripped text)
// TestPagesSayPodOnKubernetes: same pages for a Kubernetes server → "All pods (merged)",
//   ">Pod grid<", "Pending (max/pod)", "pod × worker", "4 ×3 pods"
// TestMessagesUseNoun: static server: stale pod= of a gone unit → banner
//   "Instance instance-2 is no longer reported — reload"; POST with unknown pod →
//   400 "the selected instance is not an instance of this server; reload"; 404 action
//   outcome msg "not on this instance"; empty results "No instances were targeted.";
//   confirm "pause each worker on each instance below:"; detail miss
//   `Worker "x" not found on any instance`
// Tooltip titles: workerRates point Title ends " · 2/3 instances"; pending point
//   "pending (max/instance)"
```

- [ ] **Step 2: Run** — expected FAIL.
- [ ] **Step 3: Implement**: `page()` and `livePage` set `p.Noun = NounFor(h.m.cfg.Servers[p.Server])` (session's `s.Server` where a session is at hand); every string in Global Constraints' copy list uses the Noun; `concurrencyLabel`, `ratePoint`, `pendingSeries` take the Noun.
- [ ] **Step 4: Run** `go test -race ./...`, `go vet ./...`, `gofmt -l .` — clean.
- [ ] **Step 5: Commit** `feat(web): say pod on Kubernetes servers, instance elsewhere`.

---

### Task 4: Polling through balancers

**Files:**
- Create: `internal/web/balancer.go`, `internal/web/fakebalancer_test.go`, `internal/web/balancer_test.go`
- Modify: `internal/web/poller.go`, `internal/web/poller_test.go`, `internal/web/fakevarmq_test.go` (add `fakeWorker.GetStatus int`: when non-zero, `GET /workers/{name}` replies with it and `{"error":"…"}`; add `fakeVarmq.ListStatus int`: same for `GET /workers`)
- Test: `internal/web/balancer_test.go`, `internal/web/poller_test.go`

**Interfaces:**
- Consumes: Task 1 `ListWorkersFresh`, `GetWorkerFresh`; Task 2 `PodSnapshot`/`Snapshot` fields.
- Produces:
  - `func attemptCap(k int) int` (k < 1 treated as 1).
  - `type answer[T any] struct { V T; Err error; Via string; At time.Time }` — one instance's first reply; `Err` is the reply's error (`*api.APIError` for non-2xx), nil on 2xx.
  - `func reach[T any](ctx context.Context, eps []*api.Client, want []string, limit int, send func(context.Context, *api.Client) (T, string, error)) (got map[string]answer[T], sent int, lastErr error)` — sends through `eps` in turn (attempt i → `eps[i%len(eps)]`) in rounds; each round sends `min(limit-sent, max(missing, len(eps)))` attempts concurrently, where `missing` = wanted instances not yet answered; stops when every wanted instance answered, `sent == limit`, or ctx is done. A reply with an instance ID is that instance's answer (first one kept, wanted or not); an attempt with no instance ID (transport error, reply without header) sets `lastErr`. `want` may be empty: then exactly one round of `len(eps)` attempts is sent.
  - `type tracker struct` (poller-goroutine only): `func newTracker(interval time.Duration, kubernetes bool) *tracker`; `func (t *tracker) saw(endpoint, instance string, at time.Time)` ("" ignored); `func (t *tracker) balancer(endpoint string, now time.Time) bool` (>1 instance within window); `func (t *tracker) known(balancers []string, direct map[string]bool, now time.Time) map[string]time.Time` (instance → last seen through any of balancers, within window, minus direct IDs); `func (t *tracker) lastVia(instance string) string`; `func (t *tracker) prune(now time.Time)`; `func (t *tracker) label(pods []PodSnapshot)` (instance-N numbering, non-Kubernetes only).
  - `func pollOnce(ctx context.Context, pool *api.Pool, t *tracker) *Snapshot` (signature change); `Session` owns a `*tracker` created in `NewSession` with `newTracker(interval, srv.Kubernetes != nil)`.

`pollOnce` per tick (now = tick start):
1. Split `pool.Clients()` into balancer (`t.balancer(ep, now)`) and direct endpoints.
2. Poll direct endpoints concurrently: list with `ListWorkersFresh`, then `GetWorkerCtx` per worker (today's rules). Record `t.saw(ep, inst, now)` for the list reply and every fetched sample's `Instance`. A direct endpoint whose replies this tick came from more than one instance is moved to the balancer set and its unit dropped.
3. `direct` = instance IDs of the remaining healthy direct units; `known := t.known(balancerEPs, direct, now)`; `limit := attemptCap(max(len(known), len(balancerEPs)))`.
4. Lists: `reach` with `ListWorkersFresh`, want = known IDs. Each answering instance not in `direct` is known (new ones included) and, if its reply is 2xx, has a list.
5. Workers: for each name on any list, `reach` with `GetWorkerFresh(url.PathEscape(name))`, want = instances whose list has the name. 2xx → sample (`At` = answer time); 404 → absent; any other reply → stays unreached. Names run concurrently.
6. Units: an instance with a list → `Key "#"+id`, `Via` = its list answer's endpoint, `Instance`, `Balanced`, `Workers`, `Unreached` (sorted). An instance whose list answer was an error → errored with that error's text (as a direct unit's failed list). A known instance with no list answer at all → errored `not reached this refresh · last seen {age} ago` (age from its last sighting; `Via = t.lastVia(id)`). If the lists `reach` got no instance answer at all and `lastErr != nil`: every known instance is errored with `lastErr.Error()`; with none known, one unit per balancer endpoint `{Key: ep, Via: ep, Balanced: true, Err: lastErr.Error()}`. Samples or answers from instances in `direct` are dropped. Record `t.saw` for every answer of both phases.
7. `Behind[ep]` = distinct instances that answered through ep this tick (all answers).
8. Labels: Kubernetes → direct units as today (`pool.PodName`, `dedupeLabels`), balanced units their instance ID; otherwise `t.label(pods)`. Sort `Pods` by `Key`; `t.prune(now)`.

- [ ] **Step 1: Write the fake balancers** in `fakebalancer_test.go`:
  - `func newRoundRobinLB(t *testing.T, backends ...*httptest.Server) *httptest.Server` — each request goes to the next backend (`httputil.ReverseProxy`).
  - `func newPerConnLB(t *testing.T, backends ...*httptest.Server) *httptest.Server` — picks the next backend once per client TCP connection (key the choice by `r.RemoteAddr`) and keeps it for every request on that connection, like kube-proxy.
  - Helper `func lbServer(urls ...string) *config.Server` building a static server over the balancer URLs.
- [ ] **Step 2: Write the failing tests** in `balancer_test.go`

```go
// TestAttemptCap: 0→6, 1→6, 3→18, 10→66
// TestReachStopsWhenAllAnswered: round-robin over 3 fakes, want {a,b,c}, limit 18
//   → got has a,b,c; sent == 3
// TestReachRespectsLimit: want {a,b,c,zz} (zz never answers), limit 18 → sent == 18, no zz
// TestPollRoundRobinBalancer: fakes a,b,c each with worker "w" (Completed 10/20/30) behind
//   newRoundRobinLB; tracker for a static server; tick 1 → 3 units keys "#a","#b","#c",
//   Balanced, labels "instance-1".."instance-3", Behind[lb] == 3, no unit keyed by the LB URL
// TestPollPerConnBalancerDetected: same behind newPerConnLB → within 3 ticks 3 balanced units
// TestPollBalancerRates: two ticks interval apart with counters advanced → each unit's
//   sample RateOK with its own rate (via History.Append)
// TestPollBalancerUnreachedWorker: fake c's worker "w" has GetStatus 500 → unit "#c"
//   healthy, Unreached == ["w"], no sample for "w"
// TestPollBalancerListError: fake c gets ListStatus 500 → unit "#c" Err contains "500"
// TestPollBalancerMissedInstance: after tick 1, fake c's server is closed (the LB answers
//   502 without an instance header for it) → tick 2 unit "#c" Err starts
//   "not reached this refresh · last seen "
// TestPollBalancerForgetsInstance: fake c gone; ticks with `now` advanced past the window
//   (inject time: pass now through the tracker or tick helper) → no "#c" unit
// TestPollBalancerBackToDirect: only fake a remains behind the LB past the window →
//   unit keyed by the LB URL, not Balanced
// TestPollDedupesInstanceAcrossEndpoints: same fakes a,b behind two round-robin LBs → two
//   units "#a","#b" (not four); a static list [LB, a's own URL] → a is the direct unit
//   (key = a's URL), b is "#b", no "#a"
// TestPollBalancerIgnoresAnswersWithoutInstance: LB that answers every 3rd request with a
//   bare 502 (no header) → units a,b still healthy
// TestPollNoInstanceHeaderStaysDirect: fakes with Instance "" behind round-robin → one
//   direct unit keyed by the LB URL, Behind empty
// TestPollBalancerDeadline: fake c Delay > tick deadline → snapshot returned before the
//   deadline + 100ms; "#c" errored or its workers unreached; a,b healthy
// TestPollBalancerEscapesWorkerNames: worker "a/b" behind the LB → fakes saw
//   "GET /workers/a%2Fb"
// TestLabelsNeverReused: tracker.label over ticks with keys {#a,#b} then {#a,#c} →
//   #a instance-1, #b instance-2, #c instance-3; static list of 3 URLs → instance-1..3
//   in URL order; Kubernetes tracker leaves labels alone
// TestBalancerEndpointDown: LB URL refused on every attempt with instances known →
//   all known units Err = the dial error text; none known → one unit {Key: LB URL, Balanced}
```

- [ ] **Step 3: Run** `go test ./internal/web -run 'TestAttemptCap|TestReach|TestPoll|TestLabels|TestBalancer' -v` — expected FAIL.
- [ ] **Step 4: Implement** `balancer.go` (`attemptCap`, `answer`, `reach`, `tracker`) and the `pollOnce` steps above in `poller.go`; keep `pollDirect` (renamed from `pollPod`) returning the instances that answered. Tests that need time control may call an unexported `pollAt(ctx, pool, t, now)` that `pollOnce` wraps.

  `ponytail:` no per-endpoint request limit; add one if new-connection volume bites.
- [ ] **Step 5: Run** `go test -race ./...`, `go vet ./...`, `gofmt -l .` — clean.
- [ ] **Step 6: Commit** `feat(web): poll every instance behind a load balancer`.

---

### Task 5: Actions through balancers, controls and README

**Files:**
- Modify: `internal/web/actions.go`, `internal/web/server.go`, `internal/web/render.go`, `internal/web/templates/{dashboard,detail,confirm}.html`, `README.md`
- Test: `internal/web/actions_test.go`, `internal/web/server_test.go`, `internal/web/render_test.go`

**Interfaces:**
- Consumes: Task 1 `ActionFresh`, `TuneConcurrencyFresh`; Task 2 unit fields; Task 3 `Noun`; Task 4 `reach`, `attemptCap`, `answer`.
- Produces:
  - `type actScope struct { direct []*api.Client; balanced []PodSnapshot; dead []PodSnapshot; balancers []*api.Client }` and `func scopeOf(clients []*api.Client, latest *Snapshot, pod string, n Noun) (actScope, error)`:
    - `pod == ""`: `direct` = clients whose endpoint is not a key of `latest.Behind` (sorted by endpoint); `balanced` = latest units with `Balanced && Instance != ""`; `dead` = latest units with `Balanced && Instance == ""`; `balancers` = clients whose endpoint is a key of `latest.Behind` or the `Via` of a balanced unit.
    - `pod` names a unit that is `Balanced` in latest (any `#` key, or a balancer endpoint's errored unit): error with the one-unit refusal text.
    - otherwise `pod` must be a client's endpoint (today's rule and message, with the Noun).
  - `Page.ScopeBalanced bool` (scope holds a balanced or dead unit) and `Page.OneBalanced bool` (scope is one balanced unit), set in `fill`.

Rules:
- Merged action / concurrency / bulk: `direct` as today (`fanOut`; concurrency still skips direct units already at n). `balanced`: `reach` with `ActionFresh` / `TuneConcurrencyFresh`, want = their instance IDs, `limit = attemptCap(max(len(want), len(balancers)))`; each wanted instance's first answer → `outcome` (2xx ok, 409 no change (msg), 404 no change (not on this {one}), else failed (msg)); never answered → `failed (not reached through the load balancer after {sent} tries)`. Answers from other instances are not results. `dead` units → `failed ({unit Err})`, nothing sent. Results labelled with the unit's `Label`, `Key`, `Tip()`; order: direct then balanced then dead, each by key; bulk per worker.
- Restart with `ScopeBalanced` → 400 `restart isn't available through a load balancer`, nothing sent. Any POST whose scope is one balanced unit → 400 one-unit text, nothing sent.
- Detail page: `OneBalanced` → the controls section is replaced by `<p class="note">{one-unit text}</p>`; `ScopeBalanced` → no Restart button. Dashboard: `OneBalanced` → bulk form replaced by the same note.
- Confirm panel columns: `direct` endpoints (label/tip from latest, `ShortOrigin` fallback), then `balanced`, then `dead` (with `Err`); `Absent` as today plus Task 2's unreached rule.
- README: in the Web UI section, one short paragraph: a balancer URL shows each instance behind it (named instance-1, …), merged actions reach every instance, restart and single-instance actions need the instances' own addresses or Kubernetes/DNS discovery.

- [ ] **Step 1: Write the failing tests** in `actions_test.go` (fakes a,b,c behind `newRoundRobinLB`, session polled once so latest has units `#a,#b,#c`)

```go
// TestActionMergedReachesEveryInstance: POST /workers/w/actions/pause (merged) → 200;
//   results "instance-1 ok", "instance-2 ok", "instance-3 ok"; each fake's worker Paused;
//   total PATCHes ≤ attemptCap(3)
// TestActionBalancedRepeatIsNoChange409: concurrency n=4 where b is already 4 (fake 409)
//   → b "no change (…)"; a, c "ok"
// TestActionBalancedUnreachableInstance: fake c removed from the LB after the poll →
//   c "failed (not reached through the load balancer after 18 tries)"; a, b ok
// TestRestartRefusedWithBalancedScope: merged restart → 400 body contains
//   "restart isn't available through a load balancer"; zero PATCHes on every fake
// TestOneBalancedUnitRefused: pod=#a → action, concurrency and bulk each 400 with the
//   one-unit text; zero PATCHes
// TestMixedScope: static [LB(a,b), c's own URL] → merged pause: c via its URL (one PATCH
//   on c through its URL), a and b via the LB; three results
// TestDeadBalancerUnit: LB down, unit {Key: LB URL, Balanced, Err "x"} → result
//   "failed (x)", nothing sent
// TestBulkConfirmColumnsAreUnits: confirm panel has columns instance-1..3, titles carry Tip
// server_test.go: detail page pod=#a has the one-unit note and no "Pause" button;
//   merged detail on the LB server has no "Restart" button; dashboard pod=#a has no
//   bulk form; a static server without balancers still shows Restart
```

- [ ] **Step 2: Run** — expected FAIL.
- [ ] **Step 3: Implement** per the rules above; `podScope` is replaced by `scopeOf`.
- [ ] **Step 4: Run** `go test -race ./...`, `go vet ./...`, `gofmt -l .` — clean.
- [ ] **Step 5: Commit** `feat(web): act on every instance behind a load balancer`.
