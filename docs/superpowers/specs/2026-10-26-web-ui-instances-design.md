# vartop web UI — instances behind a load balancer

Amends `2026-10-25-web-ui-design.md`. Where the two disagree, this one wins.

## Context

The web UI treats each configured endpoint URL as one "pod". When that URL is a
load balancer in front of several varmq instances, every request reaches a
different instance, so today the UI shows one random instance per refresh: the
pod label flips (lb-1 → lb-3 → lb-2), status flips (Paused ↔ Running), rates are
shown as "n/a: load-balanced", the worker detail charts are blank, and an
action reaches one instance only. A yellow warning tells the user to switch to
Kubernetes or DNS discovery.

The owner wants a balancer URL to work like a list of pods: each instance behind
it appears in the picker (lb-1, lb-2, …), and **All (merged)** aggregates them.
The word "pod" belongs to Kubernetes; elsewhere the unit is an "instance".

## Decisions the owner made

| Topic | Decision |
|---|---|
| Picker behind a balancer | One entry per instance seen behind it (named by `X-Varmq-Instance`), plus **All instances (merged)**, which aggregates |
| Wording | "pod" / "Pod" only for Kubernetes-discovery servers; "instance" / "Instance" for every other server (static endpoints, DNS) |
| Labels | Non-Kubernetes units are named instance-1, instance-2, … (Kubernetes keeps pod names) |
| Actions behind a balancer | Ship with today's varmq (no varmq change): merged-scope pause / resume / stop / concurrency repeat until every instance confirms; restart and single-instance actions are not offered behind a balancer |
| Later | A varmq request header naming the target instance (e.g. `X-Varmq-Target`, answered 421 by any other instance) would allow every action on one instance; separate varmq PR, not part of this work |

## Goals / non-goals / anti-goals

**Goals:** behind a balancer, every instance gets its own picker entry, grid
column, health count and rates; merged view aggregates them like pods; status no
longer flips; detail charts work; actions in merged scope reach every instance
and say per instance what happened; Kubernetes servers say "pod", others
"instance".

**Non-goals:** acting on one instance behind a balancer; restart behind a
balancer; varmq changes; TUI or CLI changes (the TUI's flipping label stays);
guaranteeing every (instance, worker) is reached on every refresh.

**Anti-goals:** a result line saying "ok" for an instance that never confirmed;
any restart sent through a balancer; one rate series built from two instances'
samples; one instance counted twice in merged totals; the word "pod" on a
non-Kubernetes server; a balancer pinning vartop to one instance without vartop
noticing.

## Design

### Endpoints: direct or balancer

An endpoint is a **balancer** while more than one instance ID has answered it
within the lifecycle window (max(1 minute, 3 × interval)); otherwise it is
**direct**. So a single-replica Service URL that shows two pods during a rolling
update is a balancer for about a minute, then direct again.

Today a balancer that picks the instance per connection (Kubernetes Service
ClusterIP, AWS NLB) is never seen: vartop reuses its connections, so it only
ever talks to one instance. So every tick, the first worker-list request to
**every** endpoint goes over a new TCP connection, never a pooled one. A
per-connection balancer then shows a second instance within a few ticks.

### Units

The UI's unit (picker entry, grid column, health count, merged member, action
target) is one varmq instance, of one of two kinds:

- **Direct unit:** a direct endpoint. Key = the endpoint URL; reached through
  that endpoint as today. Exactly today's
  behaviour, including several direct endpoints whose processes share an
  instance ID (they stay separate units).
- **Balanced unit:** an instance ID that answered through one or more of the
  server's balancer endpoints within the lifecycle window, and is not the
  instance ID of any direct unit (a direct unit wins: the instance is shown and
  acted on once, through its own address). Key = `#` + instance ID (a URL never
  starts with `#`, so it cannot collide with an endpoint). Reached through any
  of the server's balancer endpoints, always on new connections.

So the same instance reached through two balancer endpoints (e.g. DNS discovery
of an NLB hostname with one IP per zone), or through a balancer and its own
address, is one unit and is counted once.

The key is what `?pod=` carries, what history keys samples by, and what the
picker, grid and confirm panel identify units by. The query parameter keeps the
name `pod`.

**Labels.** On a Kubernetes server units are labelled as today (pod names, with
today's collision fallbacks). On every other server, every unit — direct or
balanced — is labelled **instance-1, instance-2, …**: numbers are given in the
order this session first saw each unit (units first seen in the same tick in key
order, so a static list numbers in endpoint order), and are never reused, so a
unit keeps its number for the whole session and a newcomer after instance-2
left is instance-4, not instance-2. Wherever a label is shown (picker option,
grid column header, "Instances reporting", confirm panel, result lines) its
hover text names the real instance ID and the endpoint it was reached through.
A server switch starts numbering afresh.

### Wording

`noun` = "pod" when the server uses Kubernetes discovery, else "instance" (plural
"pods"/"instances", capitalised where the string starts a sentence or label).
Every user-visible string that says pod uses it, including: the picker label and
its "All pods (merged)" option; the nav link "Pod grid" and its page title;
"healthy / total pods" and "{h}/{t} pods"; "Pending (max/pod)" (card, column
header and point tooltip); "Concurrency (per pod)"; "{n} ×{p} pods"; "absent on
{k} pods"; the KPI subtitle (now "{noun} × worker" for both); rate point tooltips
"{k}/{n} pods"; "Pods reporting:"; "Concurrency per pod, on {scope}" and its
aria labels; scope "all pods" and "Actions on all pods"; "Selected workers on
all pods:"; the confirm panel "each worker on each pod below" and "not on this
pod"; "No pods were targeted."; "No pods to show."; "the selected pod is not a
pod of this server; reload"; "Pod {label} is no longer reported — reload"; the
404 outcome "not on this pod". Kubernetes servers read exactly as today.

### Polling balanced units

Per tick, with K = the balanced units currently known (at least 1 per balancer
endpoint), and the attempt cap C(K) = ⌈K · (ln K + 4)⌉ + 2 (K=1 → 6, K=3 → 18,
K=10 → 66). Requests go to the server's balancer endpoints in turn, on new
connections, and may run concurrently; the tick deadline (0.8 × interval)
bounds everything.

1. **Lists.** Request the worker list until every known balanced unit has
   answered once, or C(K) attempts. Each answer is that instance's worker list.
   An answer from a not-yet-known instance makes it known (the cap is not
   recomputed mid-tick). Answers from an instance that is a direct unit are
   ignored.
2. **Workers.** For each worker name in any balanced list, request it until
   every balanced unit whose list has it has answered with its details, or
   C(K) attempts. A 200 answer is that instance's sample for the worker; a 404
   means that instance no longer has it (skipped, as today).
3. **Units.** A balanced unit whose list answered this tick is healthy and
   carries its samples. A worker on its list that got no answer within the cap
   is **not reached** on that unit: neither a sample nor absent. The grid cell
   reads "not reached"; merged values for that worker come from the units that
   answered, its rate point is partial (k/N), and it does not count toward
   "absent on k". A known unit whose list did not answer is errored with "not
   reached this refresh · last seen {age} ago".
4. **Endpoint failure.** If every attempt failed with no instance answering
   (refused, timeout, non-varmq error), every known balanced unit is errored
   with that error; with none known, each balancer endpoint is one errored unit
   keyed by its URL, as today.

Direct units are polled as today, apart from the first list request's new
connection. A failed worker fetch on a direct unit still errors that unit
(today's rule).

`ponytail:` requests per tick for balanced units grow with workers × K × a
coverage factor (1 for round-robin, ~ln K for random), each a new connection;
add a per-endpoint request limit if that bites.

### Lifecycle

A balanced unit stays known while it has answered within the lifecycle window.
Until then it keeps its column, errored when its list was not reached (above).
After that it leaves the snapshot, like a Kubernetes pod that left the
EndpointSlice; its history rolls off. The existing gone-pod banner and `pod=`
handling apply to it unchanged. An endpoint changing between direct and
balancer changes its units' keys; their series restart (a chart break, not a
merged dip).

### Rates, merged view, display

- History keys samples by unit key + worker, so every series is one instance's;
  the existing gap rules apply unchanged.
- Nothing is "load-balanced" any more for the views: the LB exclusion from rates
  and KPIs, "n/a: load-balanced", and the yellow warning are removed. In their
  place `#status` shows a quiet note per balancer endpoint: "{n} instances behind
  {host:port}", n = the distinct instances that answered through it within the
  lifecycle window (amended, R14 below).
- Merged view aggregates every unit with today's rules (sum of rates, max pending
  per unit, concurrency per unit never summed, k/N partial points, mixed status,
  absent badges), plus the "not reached" rule above.

### Actions

Scope stays: merged view → every unit; filtered view → that unit.

**Merged scope (also bulk):**

- *Direct units:* as today.
- *Balanced units, pause / resume / stop / concurrency:* send the request through
  the balancer endpoints in turn, on new connections, until every known balanced
  unit has answered, or C(K) attempts. Each answer is attributed by its
  `X-Varmq-Instance` and becomes that unit's result: 2xx → "ok", 409 → "no
  change ({message})", 404 → "no change (not on this instance)", other error →
  "failed ({message})". Only a unit's first answer counts; later repeats on it
  are no-ops in varmq (pause, resume and stop return 200 when repeated;
  concurrency returns 409). An answer from an instance that is a direct unit is
  not a result (that unit gets its own request). Known balanced units that never
  answered get "failed (not reached through the load balancer after {n}
  tries)". Concurrency is sent to every balanced unit; one already at n answers
  409.
- *Restart:* never sent through a balancer. The Restart button is not rendered
  when the scope contains a balanced unit; a restart POSTed anyway gets 400
  "restart isn't available through a load balancer" and nothing reaches varmq.

**Filtered to one balanced unit:** no action or concurrency controls; in their
place: "A load balancer picks which instance answers, so actions here would
reach a random one. Use All instances (merged), or list the instances' own
addresses in the server config." Action, concurrency and bulk POSTs naming such
a unit get 400 with that text and reach nothing.

**Confirm panel (bulk):** columns are the units in scope; results are per
(worker, unit).

### Code touched

`internal/api/client.go` gains additive variants that use a new connection and
report the answering instance for lists, worker fetches, actions and concurrency
changes (the TUI keeps using the existing methods). Everything else is in
`internal/web`. Run GitNexus impact before editing existing symbols.

## Verification

- `go test -race ./...`, `go vet ./...`, `gofmt -l .` clean; `internal/tui`
  untouched.
- Unit tests with fake varmq instances behind two fake balancers: per-request
  round-robin, and per-connection (pins every request on one TCP connection to
  one instance, like kube-proxy). Assert: both get detected; a unit per
  instance with stable labels; per-instance rates; merged sums; a worker not
  reached on one unit reads "not reached", is not "absent", and its merged
  point is partial; a unit whose list is missed is errored "not reached…"; a
  unit gone past the lifecycle window leaves; an endpoint whose second instance
  ages out becomes direct again; the same instance through two balancer
  endpoints, or through a balancer and its own address, is one unit; merged
  pause reaches every unit with one result each; a unit that never answers gets
  "failed (not reached…)"; restart with a balanced unit in scope → 400 and zero
  PATCHes; an action on one balanced unit → 400 and zero PATCHes; wording
  "instance" for static/DNS servers and "pod" for Kubernetes.
- Manual, in the demo: `lb-cluster` (round-robin) and a per-connection balancer
  in front of three instances: picker lists instance-1…instance-3 (lb-1…lb-3
  on hover), labels stable, charts
  move, merged pause reports three lines and all three backends are paused,
  restart absent, one-instance view shows the note instead of controls.
- Label tests: static list of three → instance-1…3 in endpoint order; a unit
  that leaves and a new one that arrives → the newcomer gets the next unused
  number; hover text carries the instance ID; Kubernetes labels unchanged.

## Calls I made (check these)

Balanced unit key `#instanceID`, and the `pod` query name kept · instance-N
numbered by first sight, never reused, also for static/DNS direct units, real
ID on hover · an endpoint is a
balancer only while it has had >1 instance within the lifecycle window · first
list request per endpoint per tick always on a new connection; everything to a
balancer always on new connections · attempt cap ⌈K·(ln K + 4)⌉ + 2 · a worker
missed on a unit is "not reached" on that cell only; a unit whose list is missed
is errored; neither shows stale numbers · balanced unit forgotten after max(1
min, 3 intervals) · one instance seen through several endpoints is one unit; a
direct address wins over a balancer · quiet "{n} instances behind
{host:port}" note replaces the warning · KPI subtitle becomes "{noun} × worker"
for Kubernetes too · concurrency is sent to every balanced unit (no skip, 409 =
no change).

## Amendments made during the build

Rulings taken while building and reviewing this work changed the behaviour
below. Where this section and the text above disagree, this section wins.

- **R5, note wording.** One instance behind an endpoint reads "1 instance
  behind {host:port}"; more read "{n} instances behind {host:port}".
- **R6, worker unreached everywhere.** A worker that no unit in scope answered
  for keeps its dashboard row: status "not reached" (class unknown), and "—"
  for concurrency, busy, idle and pending.
- **R7, connecting label.** A static server with several endpoints connects as
  "Connecting to {n} instances…".
- **R8, R11, R15, R16, reaching instances behind a balancer.** Polls and merged
  actions use one event-driven reacher instead of rounds. Each request (the
  worker list, each worker's details, an action on each worker) is a job with
  its own budget of C(K) attempts. Every finished attempt is recorded at once,
  and its slot goes to a job drawn at random, so jobs side by side never fall
  into step behind a round-robin balancer. A job keeps max(instances it still
  misses, endpoints) attempts in flight and sends at least one attempt per
  endpoint. Attempts with no reply after max(10 ms, 2 × the slowest reply so
  far) are presumed stuck, and others are sent within the budget; a job that
  has every instance it wants counts a stuck attempt as back, and a job out of
  budget waits for its stuck attempts until the deadline. An instance counts as
  answered only on a definitive answer: a 2xx, a 404 for a worker fetch, a 409
  or 404 for an action. Other error answers are kept until a definitive one
  replaces them, so "only a unit's first answer counts" no longer holds.
- **R12, phases.** Endpoints that are balancers when a tick starts are polled
  alongside the direct ones, skipping the instances the previous tick showed
  as direct units. A direct endpoint found to be a balancer during the tick
  gets a balancer pass of its own after the direct phase, time permitting.
- **R13, direct unit without a reply.** A direct unit whose list got no reply
  keeps the instance last seen through its endpoint, so that instance does not
  also show up through a balancer.
- **R14, Behind.** The note's n counts the distinct instances that answered
  through the endpoint within the lifecycle window, not only this tick.
- **R17, a balancer as pod.** A POST whose pod names a balancer endpoint is
  refused like one naming a balanced unit.
- **R18, before the first refresh.** Every POST that would reach varmq (action,
  concurrency, bulk confirm and bulk) answers 409 "Waiting for the first
  refresh — try again" until the session has a snapshot. After it, only the
  keys of direct units are sent directly; an endpoint the latest snapshot lacks
  reads "failed (not refreshed yet — try again)" and is sent nothing.
- **R19, budget.** K in C(K) is max(units wanted, instances counted behind the
  balancer endpoints used, those endpoints), for polls and actions: a balancer
  picks among every instance behind it, direct units included.
- **R21, connection volume.** The ponytail estimate above is low: the coverage
  factor is about 1 + ln K for round-robin balancers too, since jobs interleave
  on the balancer's turn. The README states the volume.
- **R22, labels and order.** A unit's number follows its instance ID when no
  other unit of the snapshot has that ID, else its key, so it survives an
  endpoint turning out to be a balancer; numbers are still given in first-seen
  order and never reused. Off Kubernetes the picker, grid, "Instances
  reporting", confirm columns and results list units by number.
- **R23, no 0s age.** A unit missed in the tick in which a direct pass saw it
  reads "not reached this refresh", without "last seen 0s ago".
- **R24, action deadline.** An action's sends through balancers share a 15 s
  deadline; instances without an answer by then read "failed (not reached
  through the load balancer after {n} tries)". Direct sends are unchanged.
- **R25, pages.** A page filtered to a unit that is gone shows it chosen in the
  picker and links the merged view from its banner; one filtered to a unit
  that is down shows the unit's label and error in the banner instead of "No
  workers reported yet." or "Worker not found".
- **R27, R30, labels of a process that is down.** A unit without an instance
  (its process down and not seen within the window) keeps the number it had,
  unless another unit of the snapshot shows that instance; a unit numbered by
  its key (down since the session started) hands that number to the first
  instance it shows. So a static endpoint keeps its label and place while its
  process is down, however long. A process that comes back with a new
  instance ID is a new process and gets a new number.
