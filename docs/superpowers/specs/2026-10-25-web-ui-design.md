# vartop web UI — design (2026-10-25)

## Context

vartop is a Go CLI + Bubble Tea TUI for monitoring and controlling varmq workers across pods (static, Kubernetes, DNS discovery). The owner wants a **browser UI for their own local use** that does what the TUI does, plus what a terminal does badly:

- **trends over time** (TUI shows only the current snapshot)
- **same view, nicer** (mouse, wide layout)
- **see more at once**: pods of the active server side by side
- **easier control**: concurrency slider, bulk actions, confirmations

Success: run `vartop web`, a browser tab opens, and you can watch throughput and failures move, spot a hot or dead pod at a glance, and pause, resume or tune workers without memorising keys.

## Decisions the owner made

| Topic | Decision |
|---|---|
| Who | Owner alone, local browser on their laptop |
| Scope | Trends + TUI parity + pod side-by-side + easier control |
| History | In memory, since `vartop web` started, lost on quit |
| Frontend | **htmx + Go templates** (no Node build) |
| Multi-view | **Pods of one server** side by side; one active server at a time |
| Metrics source | **vartop only**: `GET /workers/{name}` per worker per pod each poll; no varmq change |
| Merged Pending | **Max per pod**, labelled "Pending (max/pod)", per-pod values in tooltip; never summed |
| Layout | Mockups A/B/C (dashboard, pod grid, worker detail) approved; described under Screens, the text is the spec |

## Goals / non-goals / anti-goals

**Goals:** screens A, B, C; live refresh at the server's interval; actions + concurrency on all pods or one pod.

**Non-goals:** auth/login; binding anything but loopback; persistence across runs; several servers live at once; adding/removing/editing servers in the browser (stays `vartop server …` + wizard); TUI behaviour changes; varmq changes.

**Anti-goals:** a Node/npm toolchain or JS chart library; the token reaching the browser; a destructive or bulk action firing without a confirm that names its scope; an action hitting a different server than the one on screen; a poll refresh wiping a selection or slider; summed Pending; TUI regressions.

## Architecture

```
cmd/web.go                 cobra `vartop web` (reuses persistent -s/--server)
internal/web/
  server.go                routes, guard middleware, handlers
  session.go               session{server, pool, history, ctx, cancel}; switch logic
  poller.go                one goroutine per session: fan out, build snapshot
  history.go               ring of immutable snapshots, rate derivation, aggregation
  render.go                html/template funcs incl. SVG polyline builder
  templates/*.html         layout, dashboard, grid, detail, fragments (go:embed)
  static/htmx.min.js       vendored, pinned htmx 2.x (go:embed)
  static/app.css           dark = TUI Tokyo Night (styles.go:9-18); light palette is new, via prefers-color-scheme
```

**Reuse:** `api.PoolFor`, `Pool.Clients`, `Pool.PodName`, `Pool.LoadBalanced` (`internal/api/pool.go`); `Client.ListWorkers/GetWorker/Action/TuneConcurrency` (`internal/api/client.go`); `config.Load`, `GetActiveServer`, `(*config.Server).DisplayEndpoint` (`internal/config/config.go:99`); the label rule of TUI `(*Model).connectionLabel` (`internal/tui/dashboard.go:285-296`, a Model method, so mirror its switch: k8s `ctx/ns`, `dns:host:port`, else `DisplayEndpoint()`); status colours from `getStatusStyle` (`internal/tui/types.go:103-115`); success rate = successful / (successful + failed) as in `internal/tui/detail.go:331-336`.

**Small additive changes outside `internal/web`** (run `impact()` first, per CLAUDE.md):
- `internal/api/client.go`: add ctx-taking variants (`ListWorkersCtx`, `GetWorkerCtx`, `ActionCtx`, `TuneConcurrencyCtx` via `http.NewRequestWithContext`); existing methods untouched so TUI/CLI don't change.
- `cmd/root.go`: extract inline onboarding into `loadOrOnboard() (*config.Config, error)` (onboard on `ErrConfigNotFound` **or** zero servers), used by root and web.
- Shared `effectiveInterval(*config.Server) time.Duration` (`Interval <= 0` → 5s, TUI rule at `internal/tui/commands.go:204-207`); TUI rewiring to it optional.

### Pod identity

- Key = **endpoint URL** (`Client.PrimaryEndpoint()`) everywhere: snapshot maps, `?pod=`, action targeting. Matches TUI `selectedPodOrigin` and CLI `--origin`.
- Display label (`Pool.PodName`) is **captured into the snapshot at poll time**, never resolved at render (vanished clients/k8s pods stop resolving). If labels collide in a snapshot (local instances share hostname; `ShortOrigin` drops the port), fall back to `host:port`, then full URL.

### Poller and history

1. **One poller goroutine per session.** Ticks run inline (no overlap); next tick scheduled with a `time.Timer` reset after each tick, so ticks slip rather than stack. Each tick has a ctx deadline ≈ 0.8 × interval.
2. Per tick, per pod concurrently: `ListWorkersCtx`, then `GetWorkerCtx` for each name concurrently. Requests per tick = P × (1 + W). `ponytail:` no per-pod semaphore or custom `http.Transport`; add a semaphore of ~4 per pod if TIME_WAIT build-up or tick latency shows up (DefaultTransport keeps 2 idle conns/host). A pod whose list fails or times out is marked errored in that snapshot (this replaces the TUI's separate `/health` call — deliberate). Status/concurrency/pending come from the `GetWorker` body so one row describes one instance.
3. Each (pod, worker) sample stores `sampledAt` (when its response arrived) plus the `WorkerDetails` (carries `Instance`). `ponytail:` plain structs; switch to compact samples with interned strings if P × W × ring depth passes ~50k entries.
4. **Rates computed at append time** against the last sample for the same (URL, worker), stored in the snapshot. A point is a **gap** (not 0) when: counter went down, instance changed, Δt < 0.5 × interval, or previous sample is older than 3 × interval.
5. **History** = ring of immutable `*Snapshot`, capacity `ceil(1h / effectiveInterval)`, guarded by `sync.RWMutex`. Poller appends under Lock; handlers RLock, copy pointers, render outside the lock. **GET handlers never call varmq.**
6. Merged series = sum of per-pod rates per snapshot; each merged point records **k/N pods reporting**; when k < N draw it hollow and put k/N in its `<title>`.
7. **Load-balanced endpoints** (`Pool.LoadBalanced()` non-empty): rate cells/sparklines show "n/a: load-balanced" and are excluded from merged rate KPIs; reuse TUI warning text (`internal/tui/dashboard.go:339-352`).

### Pod lifecycle per discovery source

- **Static:** endpoint never leaves the pool; a dead instance shows a persistent greyed offline column with its error.
- **Kubernetes:** not-ready pods leave the EndpointSlice immediately; the column disappears (no offline phase), its history rolls off within the hour.
- **DNS:** pods as of the last good resolution.
- Grid columns = pods in the **latest** snapshot only.

### Sessions and server switch (mirrors TUI `handlers.go:99-124`)

- Active `session` held in an `atomic.Pointer`. Each poller writes only its own session's history, so an old in-flight tick can never land in the new history.
- `POST /server`: bump `switchSeq`, run `api.PoolFor` in a goroutine; apply only if seq still matches, else `Close()` the built pool. On success: swap session, cancel old ctx, `Close()` old pool, page re-renders (new interval in triggers). On failure: **keep the old session polling**, show the error.
- Startup: page shows "Connecting to {connectionLabel}…" until the first pool is built; on failure shows the real error + a Retry button.
- **Server binding:** every form and polling request carries `server=<name>` (hidden input / `hx-vals`); `?server=` in the URL. Mismatch with the active server → POST rejected with 409 + "Server changed to Y — reload" banner, nothing sent to varmq; GET returns the same banner instead of Y's data.

### Browser updates (my call; replaces the SSE mentioned in playback)

htmx polling of the local server (`hx-trigger="every {interval}s"`), no extension. Backend only changes once per tick, so polling shows the same thing with less machinery.
- **Interactive controls live outside the polled region** (checkbox column/selection, slider, action buttons). Polled region = read-only cells/cards, updated by id (`hx-swap-oob` on `cell-<hash>-<col>`).
- `hx-sync` so polls drop while an action is in flight. Slider fires on `change` (release), not `input`. Rows sorted by worker name. Pod/server selects use `hx-push-url`.
- After an action the handler pokes the poller ("poll now" over a channel; poller stays the single writer), waits briefly for that snapshot, then renders from it, so the row reflects the change.
- `htmx.config.responseHandling` swaps 4xx/5xx into a fixed `#errors` region; nothing fails silently.

### Screens

- **A Dashboard:** header: server select, pod select ("All pods (merged)" + each pod), health dot (● all / ◐ some / ○ none) + `h/t pods`, interval, LB warning. KPI cards with sparklines: completed/s, failed/min, Pending (max/pod), workers by status (counts pod×worker instances, labelled so). Table, one row per worker in merged view (**deliberate departure from TUI's row-per-pod**; pod grid covers that):
  - status: Idle and Running both count as "active", Pausing/Stopping as transitional; "mixed" only when classes differ, per-pod statuses in tooltip; badge "absent on k pods" when a worker is missing from some reporting pods
  - concurrency: per-pod value, never summed ("4 ×3 pods" or "mixed 3–5")
  - busy and idle summed; Pending = max/pod
  - sparkline = 10-min completed/s; row click → C; checkbox column for bulk pause/resume
- **B Pod grid:** rows = workers, columns = pods (captured labels); cell = status · busy/concurrency · pending; errored pod column greyed with its error.
- **C Worker detail:** completed/s + failed/s chart (last hour), counters + success rate, concurrency control, pause/resume/restart/stop with scope "all pods" or one pod; per-pod result lines like `vartop action`.
- Charts: server-rendered inline SVG `<polyline>`, native `<title>` for hover values. No chart library.

### Actions and concurrency

- Scope: merged view → all pods; pod-filtered view → that pod only (same for bulk).
- Action names allow-listed to `pause|resume|restart|stop` (CLI rule `cmd/workers.go:95-100`; varmq also accepts `start`, not exposed).
- Worker names: `url.PathEscape` in `internal/web` before calling `Client` (Client does not escape; don't change it).
- Results per pod, HTTP 200 even on partial failure. varmq **409 → "no change (already …)"**, not a failure; only non-409 errors count as failed.
- Concurrency: labelled "per pod", starts at max current per-pod value, min 1, slider max `max(64, 2 × current max)` plus a number input; pods already at target are skipped.
- **Confirms:** stop, restart: `hx-confirm` naming worker + scope + server. **Bulk:** two-step server confirm — first POST returns a panel listing names × pods and the server; second POST with `confirm=1` (+ `server=`) executes. No custom JS.

### Command

`vartop web [-s server] [--addr 127.0.0.1:7070] [--no-open]`. Order: `loadOrOnboard` → `GetActiveServer(serverFlag)` → listen → print URL → open browser (`xdg-open` / `open` / `rundll32 url.dll,FileProtocolHandler`) unless `--no-open`. Fixed default port so bookmarks survive. Ctrl+C → `http.Server.Shutdown` + cancel session + `pool.Close()`.

## Security

- `--addr`: resolve host, **every** resolved IP must be loopback (`IsLoopback`), listen on the literal IP; otherwise error.
- Host allowlist built from `ln.Addr()` (`127.0.0.1:port`, `localhost:port`, `[::1]:port` when v6), case-insensitive, on **all** routes incl. GET (DNS rebinding reads data) → 403.
- Mux wrapped in `http.NewCrossOriginProtection()` (Go 1.25+, repo is 1.26); mutating routes are POST-only and also require `HX-Request: true`. No CORS headers, ever.
- Token stays in Go; never rendered into HTML/JSON.
- All names rendered through `html/template` auto-escaping.

## Calls I made (check these while reading)

htmx polling instead of SSE · rate gaps instead of clamp-to-0 · list call replaces `/health` · merged table aggregates per worker (TUI doesn't) · concurrency shown per pod, never summed · bulk = two-step server-side confirm · port 7070 · light theme newly designed · LB endpoints get no rates · pages render from history, so values can be up to one interval old · grid shows only pods in the latest snapshot · no semaphore / compact samples until load shows a need.

## Verification

- `go test -race ./...` green incl. untouched `internal/tui`; `go vet ./...`; `gofmt -l .` empty.
- Unit (`internal/web`): ring roll-off at capacity; rate table (normal, counter down → gap, instance change → gap, Δt too small → gap, stale previous → gap, pod appears mid-window); merged sum with k/N; Pending = max/pod; status class aggregation + "absent on k"; concurrency display; label collision fallback; `effectiveInterval(0) == 5s`.
- `httptest` with fake varmq servers (canned `/workers`, `/workers/{name}`, `X-Varmq-Instance`, 409 replies): each page/fragment renders; wrong Host → 403; cross-origin POST → 403; POST without `HX-Request` → 403; GET on mutating route → 405; stale `server=` → 409 and no varmq call; action `start` or unknown → 400; partial failure + 409 rendered per pod; token string absent from every response body; non-loopback `--addr` → error; server switch with a slow old tick leaves new history clean; failed switch keeps old session.
- Manual: 3 local varmq instances (static endpoints, distinct `varmq.InstanceID`) via `vartop web`, in the built-in browser pane: sparklines move; pause one worker on one pod → grid cell changes; stop with confirm; bulk pause shows names × pods panel; switch pod and server; kill one instance → column goes offline **and stays** (static); for roll-off use DNS or k8s.
