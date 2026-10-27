# Collector Daemon + Shared SQLite History Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An always-on `vartop daemon` polls every configured server into one SQLite file per server (24h); `vartop` (TUI) and `vartop web` read that store, show identical data with 1h/10-min trends, and web URLs carry the instance label.

**Architecture:** Domain code moves out of `internal/web` into `internal/fleet` (snapshots, rate calculator, ring, poller/tracker/reacher, view builders, action core). `internal/store` owns SQLite (`control.db` for the lease/pokes/errors, `servers/<name>-<hash6>.db` per server holding compressed snapshot blobs and label state, plus a reader `Feed`). `internal/daemon` runs one collector per server writing through the store; `cmd/daemon.go` manages it (detached spawn or the OS login service). The web and TUI become store readers.

**Tech Stack:** Go 1.26 stdlib, `modernc.org/sqlite` (pure Go, new), existing cobra/bubbletea/lipgloss/htmx.

**Spec:** `docs/superpowers/specs/2026-10-26-collector-daemon-design.md` (amends the two earlier web specs).

## Global Constraints

- Go 1.26; the only new module dependency is `modernc.org/sqlite`; no cgo.
- Data dir: `VARTOP_DATA_DIR`, else `os.UserCacheDir()/vartop`; dir 0700, files 0600. Layout: `control.db`, `servers/<file>.db`, `daemon.log`. Tokens are never written there.
- `store.PathFor(dataDir, key)` = `servers/` + sanitized key (lowercase, `[a-z0-9._-]` kept, others → `-`, max 40 chars) + `-` + first 6 hex of sha256(key) + `.db`. It is the only key→file mapping.
- Constants (names fixed): `Retention = 24 * time.Hour`, `ServerCapBytes = 256 << 20`, `LeaseTTL = 15 * time.Second`, `BeatEvery = 2 * time.Second`, `PokeEvery = 250 * time.Millisecond`, `MinPokeGap = time.Second`, `ConfigEvery = 2 * time.Second`, `PruneEvery = 5 * time.Minute`, `RemovedGrace = 24 * time.Hour`, `StoreVersion = 1`, `settleWait = 2 * time.Second` (exists), stale when `now − latest.At > 2·interval + 2s`.
- Pragmas on every file: `journal_mode(WAL)`, `busy_timeout(5000)`, `synchronous(NORMAL)`, `journal_size_limit(16777216)`; writer connections add `_txlock=immediate` and `SetMaxOpenConns(1)`; readers `query_only(1)`.
- Times stored as unix ms from wall-clock `time.Now().Round(0)`.
- Daemon exit codes: `0` = stop requested, lease lost, or lost the claim; non-zero = fatal store error or unrecovered crash.
- Environment compared between UI and daemon: config path and `KUBECONFIG` only. Service allow-list (exact): `PATH HOME XDG_CONFIG_HOME XDG_CACHE_HOME VARTOP_DATA_DIR KUBECONFIG HTTP_PROXY HTTPS_PROXY NO_PROXY http_proxy https_proxy no_proxy AWS_PROFILE AWS_REGION AWS_DEFAULT_REGION AWS_CONFIG_FILE AWS_SHARED_CREDENTIALS_FILE CLOUDSDK_CONFIG CLOUDSDK_ACTIVE_CONFIG_NAME GOOGLE_APPLICATION_CREDENTIALS AZURE_CONFIG_DIR`; any name containing `KEY`, `SECRET` or `TOKEN` is never saved even if listed.
- Service identifiers: systemd user unit `vartop.service` (`Restart=on-failure`, `RestartSec=5`); LaunchAgent label `dev.goptics.vartop` (`RunAtLoad`, `KeepAlive` dict `SuccessfulExit=false`, `ThrottleInterval` 10); Windows task `vartop daemon` (logon trigger, restart on failure every 1 min). Each runs `<absolute vartop> daemon run`.
- URL param: `Noun.Param` is `pod` on Kubernetes servers and `instance` otherwise; its value is the unit label.
- Exact copy:
  - waiting: `Waiting for the first snapshot of {server}…`
  - stale (daemon down): `No new data for {age} — the vartop daemon is not running.` + button `Start collector`
  - stale (server error): `No new data for {age} — {server}: {err}`
  - unknown label: `{label} is not {article} {one} of {server}` (e.g. `instance-9 is not an instance of lb-cluster`)
  - gone label: `{One} {label} is no longer reported — reload` (existing) with the existing merged-view link
  - not configured: `Server {name} is not configured`
  - collector error snapshots: `connecting…`, `discovery failed: {err} — retrying in {d}`, `internal error (see daemon log)`
  - `daemon stop`: `vartop daemon stopped. It starts again at your next login or the next time you open vartop.`
  - spawn failure: `vartop daemon did not start; see {logPath}`
  - onboarding offer: `Start the vartop daemon at login? It keeps collecting history in the background.`
- GitNexus via the npx CLI (MCP is broken): `npx -y gitnexus@latest analyze --index-only` when HEAD moved, `impact <symbol> --direction upstream --repo .` before editing an existing symbol (report HIGH/CRITICAL), `detect-changes --scope all --repo .` before each commit. Renames with gopls (`/home/fahim/go/bin/gopls`), moves with `git mv`; never find-and-replace a symbol.
- Every commit: `go test -race ./...`, `go vet ./...`, `gofmt -l .` clean; conventional message ending `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`; never stage AGENTS.md, CLAUDE.md, .claude/.

## Review Focus

1. Two UIs opened at the same moment with no daemon: exactly one daemon ends up collecting, the other exits 0, and both UIs see data. → Task 5 `TestEnsureDaemonRace`.
2. One server whose discovery hangs (a Kubernetes auth plugin waiting for input, an unreachable API server) never delays other servers' snapshots or the daemon's stop. → Task 4 `TestHungDiscoveryIsolated`.
3. A half-written or invalid config.json: the daemon keeps collecting with the last good config and deletes no history. → Task 4 `TestBadConfigKeepsLastGood`.
4. A reader holding a server file while the daemon prunes or resets it (version change, corruption rename): the reader recovers on its next sync without an error loop or duplicate snapshots. → Task 3 `TestFeedSurvivesReset`.
5. A web tab or TUI pointed at a server that was just removed from the config: a clear "not configured" state, no panic, no request to varmq. → Task 6 `TestRemovedServerPage` and Task 7 `TestTUIRemovedServer`.

---

### Task 1: Pre-fixes on today's code

**Files:** Modify `internal/web/poller.go`, `internal/web/balancer.go`, `internal/web/view.go`, `internal/config/config.go`, `internal/api/client.go`. Tests: `internal/web/history_test.go`, `internal/web/view_test.go`, `internal/config/config_test.go`, `internal/api/client_test.go`.

**Interfaces:** Produces `newest(snaps []*Snapshot, span time.Duration) []*Snapshot` (signature loses `interval`; selects `At >= latest.At − span`, at least one); `config.Save` writes `config.json.tmp` then renames; `api.NewClient` uses one package-level pooled `*http.Transport` cloned from `http.DefaultTransport` with `MaxIdleConnsPerHost = 16`.

- [ ] **Step 1: Failing tests**
```go
// TestRateAcrossSleepIsGap: prev.At = t, cur.At = t+8h (wall clock, no monotonic), counters +1e6
//   → rate(...) ok == false
// TestPollTimesAreWallClock: pollAt(..., now) with now from time.Now() → every Sample.At and Snapshot.At
//   has no monotonic reading (t == t.Round(0))
// TestNewestByTime: snaps at t-20m, t-9m, t-5m, t → newest(snaps, 10*time.Minute) = last 3; one snap → it
// TestSaveIsAtomic: Save writes via a temp file in the same dir; after Save no *.tmp remains; content parses
// TestPooledTransportReusesMoreConns: 8 concurrent GetWorkerCtx on one client, twice → ≤ 8 new conns total
```
- [ ] **Step 2:** run, expect FAIL. **Step 3:** implement (`time.Now().Round(0)` wherever a tick/sample time is taken; callers of `newest` updated). **Step 4:** full suite green. **Step 5:** commit `fix: wall-clock poll times, time-based windows, atomic config save, pooled transport`.

---

### Task 2: Move the domain code to `internal/fleet`

No behaviour change; `internal/web` keeps working on top of `fleet`.

**Files:** `git mv` from `internal/web` to `internal/fleet`: `history.go`, `poller.go` (Session becomes Collector), `balancer.go`, `view.go`, the non-HTTP half of `actions.go` → `fleet/act.go`, `Noun`/`NounFor` → `fleet/noun.go`; test fakes `fakevarmq_test.go`, `fakebalancer_test.go` → exported package `internal/fleet/fleettest`. Web and TUI tests import `fleettest`.

**Interfaces (produced, used by every later task):**
- Types (exported, same fields as today): `Sample`, `PodSnapshot` (+ method `IsUnreached(name string) bool` replacing `unreached`), `Snapshot` (+ new field `Err string` — server-level error), `Point`, `HealthView`, `KPIView`, `RowView`, `DashboardView`, `PodColumn`, `GridCell`, `GridRow`, `GridView`, `DetailView`, `ActionResult`, `Noun{One, Many, Article, Param string}` (`Param` added now, "pod"/"instance"), `NounFor(*config.Server) Noun`.
- Rate calculator split from `History.Append`: `type Rater struct`, `NewRater(interval time.Duration) *Rater`, `(*Rater) Rate(s *Snapshot)` (fills rate fields; same `rate`/`early` rules), `(*Rater) Seed(s *Snapshot)` (sets the last sample per unit key+worker without rating).
- `type Ring struct`, `NewRing(interval time.Duration) *Ring`, `(*Ring) Append(*Snapshot)` (no rating), `Snapshots()`, `Latest()`, `Interval()`. `History` stays as Ring+Rater (`NewHistory`, `Append` rates then stores) until Task 6 removes it.
- `type LabelState struct { Numbers map[string]LabelNumber; Followed map[string]string }`, `type LabelNumber struct { N int; Seen time.Time }` (Seen truncated to the minute); tracker gains `labels() (LabelState, bool /*changed since last call*/)` and `loadLabels(LabelState)`.
- `type Tick struct { Snap *Snapshot; Labels *LabelState }` (Labels non-nil only when changed). `type Collector struct`; `NewCollector(srv *config.Server, pool *api.Pool, sink func(Tick) error) *Collector`, `(*Collector) PollNow(ctx) bool`, `(*Collector) Close() error`, `(*Collector) LoadLabels(LabelState)`. A sink error ends the collector's loop (logged by the caller).
- View builders exported unchanged in behaviour: `Health`, `BalancerNote`, `BuildDashboard`, `BuildGrid`, `BuildDetail`, `StatusClass`, `PodsOf(s *Snapshot, pod string)`, `WorkerRates`, `PointTime`.
- Action core exported: `ActScope`, `ScopeOf(clients []*api.Client, latest *Snapshot, pod string, n Noun) (ActScope, error)`, `Act`, `ActionSender(action string)`, `UnitsOf`, `IsUnchanged`, the message constants (`OneBalancedMsg`, `RestartRefusedMsg`, `WaitingMsg`, …), `SettleWait`, `BalancerActionDeadline`.
- Web keeps `Manager`/`Session` behaviour by wrapping `Collector` with a sink appending to its `History`.

- [ ] **Step 1:** `git mv` files; fix package clauses and imports; export the names above with gopls renames.
- [ ] **Step 2:** split `History.Append` into `Rater.Rate` + `Ring.Append`; add `TestRaterMatchesHistory` (same samples through `History.Append` and `Rater.Rate` give identical rates) and `TestRaterSeed` (seeding from a stored snapshot, then rating the next → same as an unbroken run).
- [ ] **Step 3:** tracker label state (numbers are refreshed for every unit in the current tick before any number is freed, so a long sleep never frees a live unit's number): `TestLabelStateRoundTrip` (labels() after a few ticks, loadLabels into a fresh tracker → same numbers for the same units), `TestLabelNumberFreedAfter24h` (entry Seen 25h ago is freed; a new unit gets the lowest free number, e.g. 2 when 1 and 3 are live), `TestLiveNumberNotFreedAfterSleep` (tick, clock jumps 30h, tick with the same units → same numbers).
- [ ] **Step 4:** `go test -race ./...` green (all existing web/tui tests pass, moved where needed). **Step 5:** commit `refactor: move fleet domain code out of internal/web`.

---

### Task 3: `internal/store`

**Files:** Create `internal/store/{dir.go,control.go,server.go,codec.go,feed.go}` + tests. `go get modernc.org/sqlite`.

**Interfaces:**
- `DataDir() (string, error)`; `PathFor(dataDir, key string) string`.
- `OpenControl(dataDir string) (*Control, error)`; `type Lease struct { Token string; PID int; Build, ConfigPath, Kubeconfig, PathEnv string; Started, Beat time.Time; Stop int }`; methods `Claim(l Lease, now time.Time) (token string, ok bool, err error)`, `Beat(token string, now time.Time) (stopRequested bool, err error)` (`ErrLeaseLost`), `Release(token string) error`, `Lease() (Lease, bool /*fresh*/, error)`, `RequestStop() error`, `Poke(server string) (time.Time, error)`, `Pokes() (map[string]int64, error)`, `SetServerErr(server, msg string) error`, `ServerErrs() (map[string]string, error)`, `Close() error`.
- `OpenServer(dataDir, key string, writer bool) (*ServerDB, error)` (creates dir/file; `meta` holds key + version; version mismatch drops/recreates `snapshots` keeping `labels`; corrupt/not-a-db → rename to `<file>.corrupt-<unix>` and recreate); methods `Append(token string, t fleet.Tick) (id int64, err error)` (one IMMEDIATE tx; ATTACHes `control.db` and checks the lease token inside it → `ErrLeaseLost`), `Window(since time.Time) ([]*fleet.Snapshot, int64, error)`, `Tail(afterID int64) ([]*fleet.Snapshot, int64, error)`, `Latest() (*fleet.Snapshot, error)`, `Labels() (fleet.LabelState, bool, error)`, `Prune(now time.Time) error` (older than Retention, then oldest-first batches of 1000 while used bytes > ServerCapBytes), `Close() error`; `RemoveServerFile(dataDir, key string) error`.
- Blob codec (private): flate(BestSpeed) of JSON with short field names in store-private record types; sample `At` as ms offset from the snapshot; rates rounded to 3 dp, omitted when not valid; sample instance omitted when equal to the unit's.
- `type Feed struct`; `NewFeed(dataDir, key string, interval time.Duration) *Feed`; `(*Feed) Sync() error` (opens lazily read-only; first sync loads `Window(now − 1h − 3·interval)`, later `Tail`; on error closes and retries next sync; if the file was reset (max id < cursor) reloads the window), `Snapshots() []*fleet.Snapshot`, `Latest() *fleet.Snapshot`, `Interval() time.Duration`, `Close() error`. Feeds never rate (rates come stored).

- [ ] **Step 1: Failing tests**
```go
// TestPathForSafeAndUnique: "prod", "Prod", "a/b", "x y", "ünï", 100-char key → distinct names, only [a-z0-9._-],
//   ≤ 40+1+6+3 chars, deterministic
// TestRoundTrip: snapshot with direct, balanced, dead and errored units, Unreached, Behind, server Err,
//   sample instance ≠ unit instance, invalid rates → Append then Latest deep-equals (rates to 3 dp)
// TestTailNoDuplicates / TestWindowOnlyLastHour / TestPruneRetentionAndCap (cap via a tiny override)
// TestLeaseClaimBeatRelease: second Claim while fresh → ok=false; after LeaseTTL → ok=true; old token's
//   Beat and Append → ErrLeaseLost; RequestStop → Beat returns stop=true
// TestPokes: Poke twice → counter +2
// TestVersionResetKeepsLabels / TestCorruptFileRecovered (garbage bytes → renamed aside, fresh file; another
//   server's file untouched)
// TestFeedSurvivesReset (Review Focus 4): feed synced; daemon side resets the file → next Sync reloads, no dup,
//   no error loop
// TestConcurrentReadersWriter: writer appends while 4 feeds sync, under -race
// BenchmarkSnapshotSize (10 units × 20 workers): report bytes/tick and MB/day at 5s; BenchmarkWindowLoad
```
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** green; record the benchmark numbers in the report. **Step 5:** commit `feat(store): SQLite control and per-server history files`.

---

### Task 4: `internal/daemon`

**Files:** Create `internal/daemon/{daemon.go,collector.go,config.go}` + tests (in-process, `fleettest` fakes, temp `VARTOP_DATA_DIR`).

**Interfaces:**
- `type Options struct { DataDir string; Config func() (*config.Config, string /*path*/, error); BuildPool func(*config.Server) (*api.Pool, error); Lease store.Lease /*PID, Build, ConfigPath, Kubeconfig, PathEnv*/; Now func() time.Time; Log *slog.Logger }`.
- `Run(ctx context.Context, o Options) (code int, err error)`: Claim (lost → return 0); Beat every BeatEvery (stop requested → graceful, 0; ErrLeaseLost → 0); one collector per server, start staggered `i·interval/N`; config re-read every ConfigEvery when mtime or size changed — parse/validate error keeps the last good config and is logged once; diff by key + JSON encoding → start/stop/restart only those; pool build in its own goroutine with backoff 5s doubling to 5min, error snapshots meanwhile (`connecting…`, `discovery failed: {err} — retrying in {d}`); per-tick panic recovery → `internal error (see daemon log)` snapshot; pokes every PokeEvery → coalesced `PollNow` at most once per MinPokeGap per server; per-server writer goroutine (queue 8; full/busy → drop the tick, log once); labels loaded from the server file at collector start, saved with the tick when changed; rater seeded from the latest stored snapshot; prune every PruneEvery; server files absent from config for RemovedGrace are removed; zero servers → idle. Fatal store error → non-zero.

- [ ] **Step 1: Failing tests**
```go
// TestSecondDaemonExitsZero, TestTakeoverEndsOldDaemon (old Append → ErrLeaseLost → Run returns 0)
// TestStopRequestExits (RequestStop → Run returns 0 within 1s; lease released)
// TestCollectsEveryServer (3 servers incl. a round-robin balancer → each file gets snapshots, labels instance-N)
// TestConfigAddRemoveEdit (add → new file filling; remove → collector stops, file kept; interval edit → restart)
// TestBadConfigKeepsLastGood (Review Focus 3): write invalid JSON → still collecting, no file removed
// TestRemovedServerFileDeletedAfterGrace (Now advanced past RemovedGrace → file gone)
// TestHungDiscoveryIsolated (Review Focus 2): BuildPool for one server blocks → others keep a snapshot every
//   interval; stop returns within 1s
// TestPokeTriggersPoll (Poke → new snapshot within 1s; 5 pokes in 200ms → ≤ 2 extra polls)
// TestPanicBecomesErrorSnapshot, TestLabelsSurviveRestart (run, stop, run → same numbers; newcomer gets the
//   lowest free), TestRatesContinueAfterRestart (first sample after restart within 3·interval has RateOK)
```
- [ ] **Steps 2–5:** FAIL → implement → green → commit `feat(daemon): collect every server into the store`.

---

### Task 5: `cmd/daemon.go`, detach and login service

**Files:** Create `cmd/daemon.go`, `cmd/detach_unix.go` (`//go:build !windows`), `cmd/detach_windows.go`, `cmd/service.go` (interface + allow-list + rendering), `cmd/service_linux.go`, `cmd/service_darwin.go`, `cmd/service_windows.go`, `cmd/service_other.go` (no-op for other OSes); modify `cmd/root.go`, `cmd/web.go`, `internal/config/prompt.go` (onboarding offer). Tests with golden files under `cmd/testdata/`.

**Interfaces:**
- `vartop daemon` with subcommands `run` (foreground: `daemon.Run`, exits with its code; SIGINT/SIGTERM graceful, SIGHUP ignored), `start`, `stop` (RequestStop, wait ≤5s for release, then kill the recorded PID only if that lease is still fresh; prints the stop copy), `restart`, `status` (pid, uptime, build, config path, KUBECONFIG, store dir and size, per server: last snapshot age, units, error; exit 3 when not running), `install`, `uninstall`.
- `type supervisor interface { Installed() bool; Install(exe string, env map[string]string) error; Uninstall() error; Start() error; Restart() error }` per OS; `spawnDetached(exe string, args []string, logPath string) error` (Unix `Setsid`; Windows `DETACHED_PROCESS|CREATE_NEW_PROCESS_GROUP`; stdin nil; stdout/stderr → log opened for append, renamed to `.1` first when over 5 MB; `go cmd.Wait()`).
- `buildID() string` = absolute executable path + size + mtime. `serviceEnv(environ []string) map[string]string` applies the allow-list and the KEY/SECRET/TOKEN exclusion.
- `ensureDaemon(o ensureOptions) error` with injected supervisor, spawner, control, clock: no fresh lease → start (supervisor if installed, else spawn), wait ≤3s for a fresh lease, else the spawn-failure copy; fresh lease with a different build, config path or KUBECONFIG → (supervisor installed: rewrite the service env, `Restart`) else RequestStop, wait, spawn. Called by root (TUI) and `web` before they run; failures are shown, not fatal.
- Renderers: `systemdUnit(exe string, env map[string]string) string`, `launchdPlist(exe string, env map[string]string) string`, `windowsTaskXML(exe string, env map[string]string) string`.
- Onboarding (`loadOrOnboard` path, first run only): huh confirm with the onboarding-offer copy → `supervisor.Install`.

- [ ] **Step 1: Failing tests**
```go
// TestEnsureDaemonStartsWhenAbsent / NoopWhenSameBuildAndEnv / RestartsOnBuildMismatch /
//   RestartsOnConfigPathOrKubeconfigMismatch (fake supervisor installed → env rewritten + Restart; not installed
//   → stop + spawn) / PathDifferenceIgnored
// TestEnsureDaemonRace (Review Focus 1): two ensureDaemon calls at once over in-process daemon.Run spawners
//   → one lease, the other Run returns 0
// TestServiceEnvAllowList: secrets (AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN, GITHUB_TOKEN) never kept
// TestServiceFilesGolden: systemd unit, plist, task XML match testdata goldens
// TestDaemonStatusExitCodes (running 0, not running 3), TestDaemonStopCopy
// TestDetachedSpawnSubprocess (skipped under -short): re-exec the test binary as a daemon, it claims the lease,
//   `daemon stop` ends it
```
- [ ] **Steps 2–5:** FAIL → implement → green → commit `feat(cmd): vartop daemon commands, detached spawn and login service`.

---

### Task 6: Web reads the store; label URLs

**Files:** Modify `internal/web/{server.go,actions.go,render.go}`, `internal/fleet/act.go` (`ClientsFor`, label matching), `internal/fleet/view.go` (`PodsOf` by label), templates (`layout`, `dashboard`, `detail`, `grid`, `confirm`), `cmd/web.go`. Delete `internal/web/session.go` and `History` from fleet (no users left). Tests ported to a fixture that runs `daemon.Run` in-process over `fleettest` fakes with a temp data dir.

**Interfaces:**
- `type Source struct { DataDir string; Config func() (*config.Config, error); Control *store.Control; Start func() error /*ensureDaemon*/ }`; `NewHandler(src Source, hosts []string) (http.Handler, error)`.
- Server comes from `?server=` (default `cfg.DefaultServer`); picker is a GET form (`name="server"`); `POST /server` and the "Server changed" banner are removed. Feeds cached per server key+interval, evicted after 10 min unused; each GET syncs its feed (never calls varmq).
- Label param: `Page.Pod` holds a label read from `Noun.Param`; `pageURL(path, server, param, label)`; every hidden input `name="{{.Noun.Param}}"`; `PodsOf` and `ScopeOf` match `Label`; on Kubernetes a `pod=` value starting with `#` or containing `://` is treated as absent.
- `fleet.ClientsFor(latest *Snapshot, token string) []*api.Client` (direct unit keys, balanced `Via`s and `Behind` keys); `ScopeOf`'s unrefreshed branch removed.
- Actions: `bind` resolves server (not configured → 409 not-configured copy), syncs, stale → `Control.Poke` + wait ≤ SettleWait, still stale → 409 stale copy; label gone → 409, nothing sent. After an action: Poke, then sync until a snapshot newer than the poke or SettleWait.
- Stale banner per the copy, with `POST /daemon/start` (behind the existing guards) calling `Source.Start`.

- [ ] **Step 1: Failing tests** — `TestInstanceParamResolves` (`?instance=instance-2` filters; hidden inputs and links carry `instance=`), `TestKubernetesPodParam` (`?pod=<name>`), `TestUnknownAndGoneLabels` (copy above), `TestOldPodParamFallsBackToMerged`, `TestTwoTabsTwoServers` (no server-changed banner), `TestStaleBannerAndStart` (daemon stopped → banner; POST /daemon/start → Start called), `TestActionRefusedWhenStale` (no PATCH), `TestActionPokesAndShowsResult`, `TestRemovedServerPage` (Review Focus 5), `TestGETNeverCallsVarmq`, the existing guard/token tests.
- [ ] **Steps 2–5:** FAIL → implement → green → commit `feat(web): read the shared store; label URLs`.

---

### Task 7: TUI reads the store

**Files:** Modify `internal/tui/{model.go,commands.go,handlers.go,dashboard.go,detail.go,switcher.go,types.go}`, `cmd/root.go`. Tests rewritten per the explorer map: fetch/health/pool tests replaced by feed-driven ones; pure UI tests kept.

**Interfaces:**
- `tui.NewModel(cfg *config.Config, serverKey string, dataDir string, ctl *store.Control) Model`; 1s `tickMsg` → `syncCmd(feed, gen)` → `syncedMsg{server string; gen int; err error}`; messages from another server/gen are dropped (replaces `scopedMsg` / `*api.Pool`).
- Rows: one per unit × worker from `feed.Latest()` (units in snapshot order, workers by name); Node = label; errored units in the header; unreached cells `not reached`/`—`; `selectedLabel` filters like today's `selectedPodOrigin`.
- Header: `fleet.Health`, `fleet.BalancerNote`, `updated hh:mm:ss`, stale age in warning style when stale.
- Switcher: units/health per server from `store.OpenServer(…, false).Latest()` (opened while the switcher is open); switching swaps feeds instantly.
- Actions (same keys) on the selected row's unit: balanced → error line with `fleet.OneBalancedMsg`; direct → `api.NewClient(unit.Key, token).Action(...)`, then `Control.Poke`, sync after 300 ms. `r` → Poke (and `ensureDaemon` when not running).
- Detail screen: values from the latest sample of (label, worker); missing → `{label}: {err}`.

- [ ] **Step 1: Failing tests** — `TestRowsFromFeed`, `TestStaleHeader`, `TestDropsOtherGenSync`, `TestBalancedActionRefused`, `TestDirectActionPokes` (fakePod PATCH + poke counter), `TestSwitcherHealthFromStore`, `TestTUIRemovedServer` (Review Focus 5), kept: `TestResizeAllScreensDoesNotPanic`, detail scroll/layout, switcher navigation, `TestDashboardActionRoutesToRowPod` (rows from a snapshot whose unit keys are the fake URLs).
- [ ] **Steps 2–5:** FAIL → implement → green → commit `feat(tui): read the shared store`.

---

### Task 8: TUI sparklines

**Files:** Create `internal/tui/chart.go` + test; modify `dashboard.go`, `handlers.go`.

**Interfaces:** `sparkline(pts []fleet.Point, width int) string` (bucket points into `width` columns by time; bucket = mean of non-gap points, empty = space; levels `▁▂▃▄▅▆▇█` scaled to the series peak; any data ≥ `▁`), `asciiBlocks() bool` (true when `runewidth.RuneWidth('█') != 1` → levels `_.-=#`). Trend column after the last fixed column, header `Trend`, width `min(20, spare)` where spare = width − fixed columns − 12 − 2, shown only when spare ≥ 8; built in the row builder (never in View), plain text. KPI line under the header: completed/s, failed/min, pending (current value + sparkline each `clamp((width − text)/3, 0, 12)`, hidden below 4); table height −1.

- [ ] Tests: `TestSparklineBuckets` (gaps, partial, width 0/1/odd), `TestSparklinePeakScaling`, `TestASCIIFallback` (`RUNEWIDTH_EASTASIAN=1`), `TestTrendColumnThresholds` (92/106 columns), resize matrix. Commit `feat(tui): trend sparklines`.

---

### Task 9: TUI detail chart

**Files:** `internal/tui/chart.go`, `detail.go`.

**Interfaces:** `blockChart(pts []fleet.Point, width, rows int) []string` (rows × width, 8 levels per cell, peak gutter of 8 chars, `-60m … now` axis line, gaps blank, partial points dimmed by the caller). Detail screen: after the metrics section, `Completed /s (1h)` and `Failed /s (1h)` charts of 5 rows each from `fleet.BuildDetail(...).Completed1h/Failed1h` for the selected row's unit; width = viewport − 4; skipped below 20 columns.

- [ ] Tests: `TestBlockChartZeroVsGap`, `TestBlockChartPeak`, widths 20/80/200, resize matrix incl. detail. Commit `feat(tui): 1h detail charts`.

---

### Task 10: Docs

**Files:** `README.md`, spec amendments section in `docs/superpowers/specs/2026-10-26-collector-daemon-design.md`.

README: daemon (what it does, always on, `daemon` subcommands, login service per OS, environment rule), data dir + `VARTOP_DATA_DIR`, disk use (≈ measured MB/day per 10×20 server at 5s from Task 3's benchmark; 256 MB cap per server), Windows note "run `vartop daemon stop` before upgrading", URL params. Spec: amendments for every ruling taken during the build. Commit `docs: collector daemon`.
