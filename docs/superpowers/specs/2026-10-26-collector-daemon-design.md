# vartop collector daemon + shared SQLite history, label URLs — design

Amends `2026-10-25-web-ui-design.md` and `2026-10-26-web-ui-instances-design.md`. Where they disagree, this one wins.

> **Superseded in part on 2026-10-27 (R46).** The daemon, its login service and the TUI are gone: `vartop web` runs the collector itself while it runs. The store, its lease, pokes, the R44 current-snapshot rule and the label URLs stand. Read R46 first.

## Context

Two gaps the owner hit while using the instances-behind-a-balancer build (PR goptics/vartop#1):

1. **URLs leak internal keys.** A filtered page reads `?pod=%23lb-1` (or `?pod=http%3A%2F%2F…`): wrong noun off Kubernetes, and an encoded internal key instead of what the UI shows.
2. **History lives in one process's memory.** `vartop web` keeps an in-memory 1h ring for the active server only (`internal/web/history.go`); it is lost on restart and on every server switch, and the TUI has no history or charts at all (it polls varmq directly, `internal/tui/commands.go`). The owner wants a background updater so the TUI and web always show the same data, with charts in both.

Outcome: an always-on `vartop daemon` polls every configured server into a local SQLite store (24h); `vartop` (TUI) and `vartop web` become readers of that store, show identical numbers, and both draw 1h/10-min trends; URLs say `?instance=instance-2` (Kubernetes: `?pod=<pod name>`).

## Owner's decisions

| Topic | Decision |
|---|---|
| Updater | Detached `vartop daemon` does all polling; UIs only read. Always running once vartop is installed; never exits on its own; with no servers it idles. |
| Scope | Every configured server, all the time |
| Store | SQLite, pure Go (`modernc.org/sqlite`, no cgo); retention 24h; charts still show 1h (detail) and 10 min (sparklines) |
| UIs | TUI and web read the same store → same data. TUI gains a completed/s sparkline per row, KPI sparklines, and a 1h completed/failed chart on the detail screen |
| URL | `?instance=instance-2` off Kubernetes, `?pod=<pod name>` on Kubernetes; label numbers stable across daemon restarts |
| Label reuse | A number unused for 24h (the retention) becomes free; new units take the lowest free number |
| Environment | The UI's environment wins: a vartop with a different config path or `KUBECONFIG` restarts the daemon under its own environment; the login service saves an allow-list of non-secret variables |
| Supervision | With the login service installed, the service manager starts/stops/restarts the daemon (restart after a crash, not after a deliberate stop); without it, vartop spawns a detached daemon |

## Architecture

```
internal/fleet/   moved from internal/web, no HTTP: Snapshot/PodSnapshot/Sample, rate calculator
                  (split out of History.Append), Ring (1h window of *Snapshot), pollAt/pollDirect,
                  tracker, reacher, pure view builders (BuildDashboard/Grid/Detail, WorkerRates…),
                  Noun (+ Param "pod"/"instance"), action core (scopeOf, act, fanOut, reachUnits, outcome)
internal/fleet/fleettest/   shared fakes (fakevarmq, fake balancers) for fleet, store, daemon, web, tui tests
internal/store/   SQLite: control.db (lease, pokes, errors) + one file per server (snapshot blobs, labels);
                  open/version/corruption, tail/window/latest, prune + per-server cap, delete on server removal
internal/daemon/  Run(ctx, Options): lease, one collector per server, config watcher, pokes, writer, label load/save
internal/web/     handlers + templates only; reads store per request; no Manager/Session/pool
internal/tui/     reads store; chart.go (sparklines, block chart)
cmd/daemon.go     vartop daemon [run] | start | stop | restart | status | install | uninstall; ensureDaemon
cmd/detach_{unix,windows}.go, cmd/service_{linux,darwin,windows}.go   the only build-tagged files
```

New dependency: `modernc.org/sqlite` only (≈10–15 MB binary). `golang.org/x/sys` stays indirect.

**Data dir:** `VARTOP_DATA_DIR`, else `os.UserCacheDir()/vartop` (0700; files 0600): `control.db`, `servers/<name>-<hash6>.db` (one per configured server), `daemon.log`. Tokens never stored there (read from config at use).

## Store (`internal/store`) — one SQLite file per server + a small control file

- **Per-server file** `servers/<sanitized key>-<first 6 hex of sha256(key)>.db` (name safe on every OS, unique per config key; `store.PathFor(dataDir, key)` is the only mapping, used by daemon and readers):
  - `snapshots(id INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER /*unix ms, wall clock*/, data BLOB /*flate(JSON short-field record)*/)`, index `(at)`. Record types are store-private (stable shape); they carry server-level `Err`, `Behind`, units (key, via, instance, label, balanced, err, unreached) and samples (counters, status, rates rounded to 3 dp).
  - `labels(id=1, state BLOB)`: tracker `numbers`/`followed` with last-seen times; upserted in the same tx as the snapshot only when changed.
  - `meta`: config key (to verify the mapping), `user_version`.
- **Control file** `control.db`: `daemon(id=1, token, pid, build, started, beat, stop)` lease row; `servers(name PRIMARY KEY, err, poke)` control rows. Small, written by the daemon (beat, errors) and by UIs (pokes, stop requests).
- Pragmas (every file): WAL, `busy_timeout(5000)`, `synchronous(NORMAL)`, `journal_size_limit`; daemon `_txlock=immediate`, one writer goroutine per server file (each collector writes its own file, so servers never queue behind each other; queue 8, drop + log a tick when busy/full). Readers `query_only`, single short statements, open only the files they show.
- Queries (per file): tail `id > ?`; window `at >= now-1h-3·interval`; latest `ORDER BY id DESC LIMIT 1`.
- Prune every 5 min per file: older than 24h; hard cap 256 MB used bytes **per server** deletes oldest rows in batches; no VACUUM (pages reused). Thinning past 1h is the documented lever, not built.
- **Server removed from config** → its collector stops at once; its file (+ `-wal`/`-shm`) is deleted only after the server has been absent from the daemon's config for 24h (the retention, so nothing still shown is lost), and only once no reader holds it open (Windows) — retried at each prune. A wrong or half-written config therefore never wipes history. Remove + re-add under the same name within 24h resumes the old history; under another name = fresh history.
- `user_version` mismatch → drop/recreate `snapshots` only (labels kept). Open failing with corrupt/not-a-db → rename that server's file aside, recreate; other servers unaffected.
- Size: ~70–100 MB/day for a 10×20 server at 5s (blob ≈4–6 KB/tick); measured by a benchmark in task 3.

## Daemon (`internal/daemon`, `cmd/daemon.go`)

- **Single instance by lease:** `Claim` in an IMMEDIATE tx writes a random token, pid, build, beat unless a fresh lease (beat < 15s) exists → loser exits 0. `Beat` every 2s; every `Append` checks the token in its tx, so a daemon waking from sleep with a lost lease can never double-write; it exits.
- **Collectors:** one per server, staggered start (i·interval/N), today's loop (timer reset after tick, 0.8·interval deadline), `pollAt` → rate → label → writer. Pool build retries with backoff (5s→5min); until built, an error snapshot every interval (`Err` "connecting…"/"discovery failed: …"). Per-tick panic recovery → error snapshot, other servers unaffected. Poke counter (checked every 250 ms) → coalesced poll-now (≥1s apart).
- **Config:** watch mtime/size every 2s; parse/validate error keeps last good config and reports it; diff servers by key + JSON (token, interval, endpoints) → start/stop/restart only changed collectors (tracker reloads labels). `config.Save` becomes atomic (temp + rename). Zero servers → idle.
- **Lifecycle:** never exits on its own; exits on stop request, lost lease, fatal store error. SIGINT/SIGTERM graceful; SIGHUP ignored. Logs: slog text, state changes only, deduped per server; log renamed to `.1` over 5 MB at spawn.
- **Always running:** `vartop daemon install` registers a per-user login service that runs `vartop daemon run` with restart-on-failure (Linux systemd `--user` unit; macOS LaunchAgent `RunAtLoad`+`KeepAlive`; Windows logon scheduled task); `uninstall` removes it. First run (onboarding) offers to install it. Independently, `ensureDaemon` in `vartop`/`vartop web` starts the daemon when no fresh lease exists — through the service manager when the service is installed, otherwise as a detached process (Unix `Setsid`; Windows `DETACHED_PROCESS|CREATE_NEW_PROCESS_GROUP`; stdin nil, stdout/stderr → log) — waits ≤3s for the lease, else shows the log path.
- **Version skew:** build identity = executable path + size + mtime. A UI finding a different build restarts the daemon (see Supervision).
- **Commands:** `daemon run` (foreground), `start`, `stop`, `restart`, `status` (pid, uptime, build, environment, store path/size, per-server last snapshot age, units, error; exit 3 when not running), `install`, `uninstall`.

### Environment (owner: the UI's environment wins)

Collection depends on the starting process's environment: the config path (`os.UserConfigDir`: `XDG_CONFIG_HOME`/`HOME`), `KUBECONFIG` and `PATH` (EKS/GKE/AKS credential plugins), proxy variables, `VARTOP_DATA_DIR`.
- The lease row records the daemon's **config path** and **`KUBECONFIG`** (and, for display only, its `PATH`).
- `ensureDaemon` compares them with its own. On a difference it restarts the daemon under its own environment — exactly like a build mismatch: with the service installed, it first rewrites the service's saved environment (below) and restarts through the service manager; without it, it stops the daemon and spawns a detached one.
- The login service saves a fixed allow-list of non-secret variables at install (and whenever a UI rewrites it): `PATH`, `HOME`, `XDG_CONFIG_HOME`, `XDG_CACHE_HOME`, `VARTOP_DATA_DIR`, `KUBECONFIG`, `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` (and lowercase forms), `AWS_PROFILE`, `AWS_REGION`, `AWS_DEFAULT_REGION`, `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`, `CLOUDSDK_CONFIG`, `CLOUDSDK_ACTIVE_CONFIG_NAME`, `GOOGLE_APPLICATION_CREDENTIALS`, `AZURE_CONFIG_DIR`. Never keys, secrets or session tokens (`*_KEY*`, `*_SECRET*`, `*_TOKEN*`, `AWS_ACCESS_KEY_ID`, …). `PATH` is saved but not compared, so ordinary shell differences don't cause restarts.
- `status` shows the daemon's config path and `KUBECONFIG`; a Kubernetes server failing on auth shows the store error as usual.

### Supervision (owner: the service manager supervises when installed)

Exit codes decide what the service manager does:
- **0 — do not restart:** stop requested (`vartop daemon stop`), lease lost, lease loser (another daemon already collects).
- **non-zero — restart:** fatal store error, crash/panic outside the per-tick recovery.

Service definitions: Linux systemd `--user` unit `vartop.service` with `Restart=on-failure`, `RestartSec=5`; macOS LaunchAgent `dev.goptics.vartop` with `RunAtLoad` and `KeepAlive = {SuccessfulExit = false}`, `ThrottleInterval` 10; Windows scheduled task "vartop daemon" triggered at logon with restart-on-failure (every 1 min). Each runs `<absolute path to vartop> daemon run` with the saved environment.

With the service installed, everything goes through the service manager so the daemon stays supervised: `daemon start` (`systemctl --user start` / `launchctl kickstart` / `schtasks /run`), `daemon stop` (stop request → exit 0, stays stopped), `daemon restart`, build or environment mismatch, `ensureDaemon` at UI start, the web "Start collector" button and the TUI `r` key. Without the service, those spawn a detached daemon. `daemon stop` sticks until the next login or the next `vartop`/`vartop web` start (which starts it again), and says so.

### Builder's calls (from the builder check)

Lease check across files (attach `control.db` in the writer's transaction or copy the lease token into each server file) · after a daemon restart the rate calculator is seeded from each server's latest stored row · label last-seen times are coarsened (to the minute) so the labels row is upserted only when it changes · after a long sleep, last-seen is refreshed before any number is freed · a mismatch kill targets the recorded pid only while that lease is fresh (pid reuse) · old URL-shaped or `#` `pod=` values on Kubernetes are told apart from pod names and fall back to merged · the TUI detail screen shows the selected row's instance.

## Readers (web + TUI)

- **Feed:** per server, loads the 1h window then tails new rows into a Ring of decoded `*Snapshot` (no rate step — rates come from the daemon), so both UIs build from identical data with the same `fleet` view builders.
- **Staleness:** latest `At` older than `2·interval + 2s` → banner with age and reason (daemon not running / server error from the store), current values shown as "—"; history stays drawn.
- **Actions:** clients built from the latest snapshot (direct unit `Key`, balancer `Behind` keys + balanced `Via`) with the token read from config at action time; `scopeOf`/`act`/reacher unchanged except matching by label; R18's "unrefreshed" branch goes away. Snapshot not acted on (R44: stale, a previous daemon's, or no fresh lease of this environment) → poke and wait ≤2s (no poke with no daemon running), still not → 409, nothing sent; a lease that cannot be read → 409 at once. After an action: poke, tail until a snapshot newer than the poke (≤ `settleWait` 2s), render from it.
- **Web:** server comes from each request (`?server=`), Manager/Session/`POST /server` removed, server picker becomes a GET form; feeds cached per server (dropped after 10 min unused); GET never calls varmq. "Start collector" button in the stale banner (POST behind the existing guards).
- **Label URL:** `Noun.Param` is "pod" (Kubernetes) or "instance"; `pageURL`, picker, and every hidden input use it with the label as value; views filter units by label across snapshots (a process's chart survives its key changing); POSTs resolve the label against the fresh latest snapshot (gone → 409, nothing sent); a label found only in older snapshots → gone banner with a link to the merged view; unknown → "instance-9 is not an instance of {server}". Old `pod=` links show the merged view.
- **TUI:** 1s tick syncs the feed (stale guard by server + feed generation, replacing `scopedMsg`/`*api.Pool`); rows = unit × worker from the latest snapshot (Node = label); header shows `Health`, `BalancerNote` (hidden when not current, R44), stale age; switcher health from each server's latest snapshot, switching is instant; `r` pokes (and starts a stopped daemon). Actions keep today's keys on the selected row's unit (balanced unit → refused with the one-instance message); direct client + poke.
- **TUI charts:** Trend column (completed/s, last 10 min, `▁▂▃▄▅▆▇█`, width = min(20, spare), only when spare ≥ 8; ASCII fallback when block runes are wide); KPI line sparklines; detail screen 1h completed/failed block charts (5 rows each, 8 levels per cell), skipped under 20 columns. No new dependency.

## Label numbering (amends R22/R27/R30)

Numbers persist per server in `labels`. A number whose follows-entry has not been seen for 24h is freed; a new unit takes the lowest free number (no stored snapshot can then show the previous owner). Uniqueness within a snapshot, Kubernetes pod names, and R30's down-unit rules are unchanged.

## Pre-fixes found during design

- **Laptop sleep bug (today's code):** `time.Now()`'s monotonic reading doesn't advance during suspend, so `rate()`/`early()` see ~5s across an 8h sleep → a huge rate spike. Use wall-clock `time.Now().Round(0)` per tick for `Snapshot.At`/`Sample.At`; `newest()` selects by time (`At >= end-span`), not by count.
- `http.DefaultTransport` keeps 2 idle conns/host → ~18 new connections per pod per tick; one shared tuned transport (`MaxIdleConnsPerHost` 16).

## Tasks (each leaves `go test -race ./...` green; GitNexus impact before editing, detect-changes before commit)

1. Pre-fixes on current code: wall-clock times, time-based windows, atomic `config.Save`, tuned transport.
2. Move domain code to `internal/fleet` (+ `fleettest`), split the rate calculator from the ring; no behaviour change.
3. `internal/store`: `control.db` (lease, pokes, errors) and per-server files (`PathFor`, blob codec, insert/tail/window/latest, labels), prune + per-server cap, delete on removal, version reset, corruption recovery, size + load benchmarks.
4. `internal/daemon`: `Run` with lease, collectors, config watcher, pokes, writer, label persistence + 24h number reuse.
5. `cmd/daemon.go` + detach/service files: run/start/stop/restart/status/install/uninstall, `ensureDaemon`, build identity, onboarding offer.
6. Web on the store: feeds, per-request server, GET picker, stale banner + start button, snapshot-built clients, poke-and-wait, label URL param.
7. TUI on the store: data path, rows by label, header/stale, switcher, actions.
8. TUI sparklines (Trend column, KPI line). 9. TUI detail chart.
10. Docs: README (daemon, data dir, volume, service install, Windows "stop before upgrading"), spec + amendments.

Execution: spec doc → implementation plan (writing-plans) → subagent-driven development with per-task review and a whole-branch review, as before.

## Verification

- Unit: store round-trip (balanced/dead/errored units, unreached, Behind) deep-equals; tail without duplicates; 1h window; prune at 24h and per-server cap (one big server doesn't touch another's file); `PathFor` safe/unique for odd keys (`/`, spaces, unicode, case-only differences on case-insensitive filesystems); file deleted on server removal; version reset keeps labels; one corrupt server file recovered without touching others; concurrent readers vs writers under `-race`; benchmark bytes/tick and 1h load time.
- Daemon in-process over fleettest fakes: second claim refused; takeover makes old `Append` fail and `Run` return; stop request → exit ≤1s; config add/remove/edit/half-written; poke → snapshot ≤1s; panic → error snapshot; labels survive restart; number freed after 24h reused lowest-first; 8h wall-clock gap → rate gap, not a spike.
- cmd: `ensureDaemon` with injected spawner (absent → spawn, same build → no-op, different build → stop + spawn); one re-exec subprocess test; service files rendered per OS (golden), install/uninstall with injected runner.
- Web/TUI: existing suites ported to store-backed fixtures; `?instance=instance-2`/`?pod=<name>` resolve; gone/unknown labels; stale banner; actions refused when stale; TUI rows/sparklines/chart across `TestResizeAllScreensDoesNotPanic`.
- Manual: demo fleet (static pods, round-robin + per-connection balancers, mixed) with the daemon running; open TUI and web side by side → same numbers and labels; restart daemon → labels and charts continue; stop daemon → stale banners; `daemon install` on Linux → survives logout/login; laptop suspend → no spike.

## Calls I made (check these)

SQLite as the only daemon↔UI channel (lease, beat, stop, pokes in `control.db`; no sockets/pid files) · one SQLite file per server, named sanitized-key + hash, deleted 24h after the server leaves the config · one compressed snapshot blob per tick (not row-per-sample) · daemon computes rates once · 256 MB hard cap per server · service install offered at onboarding, not forced · UIs auto-spawn too · tokens never in the store · web tabs bound to their own `?server=` (no "Server changed" race) · TUI actions stay single-row (no merged/bulk in TUI) · old `pod=` links fall back to merged.

## Amendments during the build

Rulings taken while building (plan tasks 1–10) and fixing what the whole-branch review found (the final fix wave) where the build extended or departed from the design above; an amendment wins over the section it touches. `R1`…`R45` are the build's own ruling numbers (the order they were taken in; R31–R45 came out of that review, its fix wave and the wave's re-review); they are unrelated to the R-numbers of the earlier instances spec cited above (R18, R22, R27, R30). Each entry reads: what — why — cost if wrong.

### Store

- **R7** Label numbers are freed at last-seen + 24h + 1 min (last-seen is truncated to the minute), and `Followed` entries that point at a freed number are pruned — accepted as it fell out of the minute-coarsened last-seen — a number freed a minute late.
- **R8** `store.IsCorrupt(err)` is exported; the daemon closes a server file whose `Append`/`Latest` returns a corrupt error and reopens it through `OpenServer`, which sets it aside — one corrupt file recovered without touching the others — one exported func.
- **R9** `Feed` treats a 0-byte file like a missing one ("waiting"); `OpenServer` removes a file it just created when init fails — readers must not error-loop on a half-created file — none.
- **R10** `ServerCapBytes` stays 256 MB. Measured ≈149 MB/day for a busy 10×20 server at 5s (see below), so a server ~1.7× busier keeps under 24h; charts only show 1h; the README documents the formula — less than 24h of history on very busy servers.
- **R12** `store.SetAside` is exported: a corrupt table behind a readable header is set aside, not reopened forever (extends R8) — one exported func.
- **R13** `store.RemoveStale` measures the 24h grace from the file's last write (mtime), not from how long the server has been absent from the daemon's config (amends "Server removed from config"): a configured server is written every tick, so mtime ≈ when it left the config, and it survives restarts. A daemon that was down >24h and starts on a config without X deletes X at once — its data is past retention anyway, only label numbers are lost — renumbered labels for a server re-added after >24h away.
- **R38** (refines "Queries": tail `id > ?`) `Feed` tails only rows with `at ≥ now − window − 3·interval`; a cursor older than that reloads the window. In the same, unreset file an empty window keeps the snapshots held and only advances the cursor, so a feed whose daemon went down shows stale, not waiting — memory stays bounded after a long hidden or idle period — none known.
- **R40** (refines Daemon › "Single instance by lease") `store.fresh` treats a beat more than `2·BeatEvery` in the future as not fresh, so a lease is fresh when its beat is less than `LeaseTTL` (15 s) old and at most 4 s ahead — a clock stepped back must not keep a dead daemon's lease fresh for as long as the step — a daemon after a backwards clock step may lose its lease and exit (supervised → restarted).
- **R43** A feed whose 1h window is empty but whose file has rows seeds with the newest row Window counted to, read by its id so seed and cursor agree (on a first load or after a reset; a loaded, unreset feed keeps what it holds, R38) — both UIs then show the stale banner with its age, not "Waiting for the first snapshot" — one extra row read on first sync.

### Daemon

- **R1** (superseded by R5: no `(*fleet.Collector).Seed` exists; seeding goes through `NewCollector`'s `CollectorInit.Last` into `Rater.Seed`) `(*fleet.Collector).Seed(*fleet.Snapshot)` (calls its `Rater.Seed`), fed the server file's `Latest()` at collector start — the spec requires rates to continue after a restart; the fleet split left the hook out — one exported method.
- **R5** (refines R1) A collector receives its starting state before its goroutine runs: `NewCollector` takes a `CollectorInit` (saved `LabelState` + last stored `Snapshot` for seeding) instead of calling `LoadLabels`/`Seed` after start — avoids a first-tick race — one signature change.
- **R6** When a server's writer drops a tick that carried label state, it keeps that state and attaches it to the next tick it writes (latest wins); the collector clears "changed" at hand-off — a dropped tick must not lose label numbers — none.
- **R14** Per-tick panic recovery covers the poll fan-out goroutines: a panic is recovered there and re-raised on the poller goroutine, so the tick's recovery emits the error snapshot — "other servers unaffected" — a little recover plumbing.
- **R34** (refines Daemon › "Single instance by lease", Supervision's "0 — lease loser" and "Always running" › "waits ≤3s for the lease") A daemon whose `Claim` finds a fresh lease held by another token watches that lease's beat for `store.DeadAfter` (3·`BeatEvery` = 6 s): the beat moves → another daemon really collects, exit 0; it does not → the holder is dead, and the daemon takes the lease over with a claim conditional on that token still being in the row (`Control.TakeOver`, so two watchers cannot both win). `waitStarted` waits `DeadAfter` longer when a fresh lease existed as it began — a crash restart under the supervisor must not exit 0 as a loser, leaving no daemon — up to 6 s slower start after a crash.
- **R36** The daemon checks on every beat that `control.db` is still the file it opened (`os.SameFile` against the stat taken at open); a mismatch or a missing path is fatal: it exits 1 (`data directory removed`), so a supervised daemon is restarted and a detached one is spawned by the next UI. Each writer checks its server file the same way at prune time and, on a mismatch, closes it and opens a new one — a deleted data directory must not leave a daemon writing to unlinked files nobody reads — one restart after a cache cleaner runs.
- **R37** (refines Daemon › "Config": "Zero servers → idle") `config.ErrConfigNotFound` is a good config of zero servers: the collectors stop and the files stay for `RemovedGrace` (R13); a parse or validate error still keeps the last good config — deleting the config must stop the use of its tokens — a config moved away stops collection until it is restored.

### cmd and login services

- **R2** `ensureDaemon` calls went into `cmd/root.go` and `cmd/web.go` as they were when the daemon commands landed (the TUI and web still polled themselves); the later rewiring kept them — none.
- **R15** (superseded by R17) `ensureDaemon` rewrites the service definition (`Install`) before `Start` whenever the service is installed and no daemon runs — never starts a stale exe or environment — one daemon-reload per UI start that finds no daemon.
- **R16** `serviceEnv` always emits `KUBECONFIG`, `XDG_CONFIG_HOME`, `XDG_CACHE_HOME`, `VARTOP_DATA_DIR` (empty when unset in the UI) — "the UI's environment wins": the service manager's own values must not stand in for them — four extra lines per service file.
- **R17** (supersedes R15; amends Supervision) `Install` is the one call that (re)starts the service's daemon (Linux enable + restart; macOS bootout + bootstrap; Windows `/Create /F` + `/Run`); `launchDaemon` calls `Install` only; `uninstall` stops the daemon gracefully first; `Start`/`Restart` are removed from the supervisor — a single path that leaves one daemon running the new definition — none known.
- **R18** `daemon stop` after a kill, with the service installed, calls `supervisor.Stop` to cancel the manager's automatic restart — the stop copy promises "starts again at your next login" — one method per OS.
- **R19** `daemon run` rotates `daemon.log` only when it sees no fresh lease at start — a lease loser must not rename the live log — an over-5 MiB log may skip one rotation in a start race.
- **R32** (refines R17 and Supervision's "everything goes through the service manager") `launchDaemon` falls back to a detached spawn when the supervisor's `Install` fails, or no lease appears within `startWait` after it. The service's error is then a warning (`vartop daemon started detached; its login service failed: …`), shown only once the spawned daemon claimed the lease; if the spawn fails too, both errors are returned. `Install` removes the unit or plist it wrote anew when the manager command fails (Linux, macOS; a file there before is kept), so `Installed` stays false. A failed `daemon install` that had stopped a daemon starts it again, detached. Install is still the one call that starts the service's daemon; the spawn is the fallback — a broken user manager must not mean no daemon ever — a detached daemon where the service was intended.
- **R33** (refines "Builder's calls": "a mismatch kill targets the recorded pid only while that lease is fresh") `stopDaemon` kills only when the lease is fresh, has the same token **and its beat advanced** during `DeadAfter` after the stop wait (`store.BeatAdvances`): a process that beats is the lease's own, so the pid is not a reused one. Otherwise nothing is killed: `daemon stop` fails with `the vartop daemon is not responding; its lease expires within 15s`, while `ensureDaemon`, `restart`, `install` and `uninstall` carry on and the daemon they launch takes the lease over (R34) — a pid reused after a crash, plus a clock step, must never get SIGKILL — a hung daemon that no longer beats waits out its 15 s lease instead of being killed.
- **R42** `ensureDaemon` takes a same-build, same-environment fresh lease whose beat is older than `2·BeatEvery` as suspect, and spawns a daemon without stopping that one: its R34 watch takes a dead holder's lease over, or exits 0 behind a live one (the UI accepts that when the old lease beats again) — a UI opened within 15 s of a crash must not start nothing — one extra spawn that exits 0 when a live daemon is merely slow to beat.

### Web

- **R20** `POST /daemon/start` answers an htmx request with `HX-Redirect` (a non-htmx request keeps the 303) — htmx follows a 303 itself and swaps the page in without its meta refresh or header — one header.
- **R22** (amends "Builder's calls" on old `pod=` values) The rule that a key-shaped `pod=` value is absent applies to GET links only. POSTs take the parameter verbatim (a key → unknown label, 409), and a non-empty parameter of the other noun → 409, nothing sent — "POSTs resolve the label … nothing sent" — old tabs must reload before acting.
- **R23** The server picker is a plain GET form (native navigation); htmx requests that land on a waiting page get a full reload — the controls must appear when the first snapshot lands — a full page load on server switch.
- **R24** The reader copy — `WaitingMsg`, `NotConfiguredMsg`, `StaleMsg` (collection error from the control file, else the snapshot's own), `SyncErrMsg`, `GoneLabelMsg` — lives in `fleet`; web and TUI both call it — one source for copy that binds both UIs — a small `fleet` API.
- **R11** (also TUI) The first feed `Sync` (~350 ms for a 1h window) runs off the UI goroutine in the TUI (a `tea.Cmd`) and once per feed in web (feeds are cached) — a slow first page.
- **R31** (both UIs; amends Readers › "Actions", R21's signature; R44: the check is made on the lease row, fresh, released or stale, not only on a fresh lease) Both UIs compare a fresh lease's `ConfigPath` and `Kubeconfig` with their own environment (`cmd.sameEnv`; web `Source.SameEnv`, TUI `tui.Daemon.SameEnv`). On a mismatch: the banner `The vartop daemon is collecting with another configuration — Start collector to use this one.` (TUI: `… — press r to use this one.`, from `fleet.OtherConfigMsg`) with the **Start collector** button (TUI: `r`), which re-asserts this UI's environment through `ensureDaemon`; current values read "—"; every action is refused with that text (web 409) and nothing is sent or poked — the token must never go to endpoints discovered under another configuration — an extra click after switching configs.
- **R35** (both UIs; amends Readers › "Staleness": `2·interval + 2s` → `3·interval + 2s`) `fleet.Stale`: stale when `now − latest.At > 3·interval + 2s`. A server whose polls run to their 0.8·interval deadline has snapshots up to 2.6·interval old while the daemon collects as it should, which flapped the banner at 2·interval — no banner flapping on slow servers — staleness shown about one interval later.
- **R39** (both UIs) `fleet.Point` gains `At`, filled by every series builder (including `CompletedRatesOf`). `BuildDetail`, `CompletedRates1h` and `FailedRates1h` are bounded to the hour (`fleet.HistoryWindow`) up to the newest snapshot. Web charts place each point at its time over the fixed window (10 min sparklines, 1 h detail), break a line at a gap point or where consecutive points are more than `2·interval` apart, and take their peak from the points drawn; the TUI's `byTime` reads `Point.At` (the parallel `at` slice is gone) — charts had been count-bounded and index-spaced, so a 40-minute hole drew as none — one field per point.
- **R44** (both UIs; amends R31, Readers › "Actions") A newest snapshot is *current* only when it is not stale (R35), the lease row's environment is the UI's (R31) — fresh, released or stale, since Release and a crash keep `ConfigPath`/`Kubeconfig`; a row no daemon claimed is of none — and its `At` is not before the lease's `Started` (an older one is a previous daemon's: after a claim the old one's appends fail the token check). Not current: values "—", unit errors, balancer note and the TUI detail's unit tip hidden; the notice is R31's copy when the environment differs, else the stale copy. An action needs a fresh lease of this environment and a current snapshot, else poke and settle (no poke with no daemon running), re-read, refuse; a lease that cannot be read refuses at once, `cannot read the vartop daemon's lease: {err}` (fail closed; pages keep the stale rule). One rule, `store.DaemonState` (`ReadDaemon`, `Current`, `Acts`), for both UIs — R31 saw only a fresh lease, so in a hand-off the other configuration's last snapshot, fresh for up to `3·interval + 2s`, read as current and was acted on with this token — after any daemon restart, "—" and refused actions for up to about two poll intervals (staggered start, first poll up to 0.8·interval).
- **R45** (web; amends R23) The server picker applies on change, as the unit picker does (an htmx GET of `/` into the body, the URL pushed), with no Show button; an htmx request that lands on a waiting page gets `HX-Redirect` to the page asked for, not `HX-Refresh` — after a picker change a refresh would load the page picked from — found in the demo, where picking a server did nothing until Show was pressed — none known.

### TUI

- **R21** (the parameter became `tui.Daemon{Start, SameEnv, StartErr}`, see R31) `tui.NewModel` takes a fifth parameter, `start func() error` (`cmd` passes `ensureLocalDaemon`) — `tui` cannot import `cmd`, and `r` must start a stopped daemon — signature drift from the plan's Interfaces.
- **R25** Row selection is kept by (label, worker). If the selected row is gone with the action menu open, the menu closes and nothing is sent — the second key must act on the unit the user chose — the menu closes on unit churn.
- **R26** An `actedMsg` for the server on screen is applied whatever its generation; another server's is dropped — an action already sent must report its result — none known.
- **R27** (in the spirit of R11) Trend series are built off the UI goroutine (a sync/build `Cmd`) from one `Snapshots()` read and delivered with a (feed, latest, label) key; `Update` installs them only on a key match; only what the TUI shows is built (one `BuildDashboard` for the KPIs + one series per visible row) — trends may be one message behind the rows.
- **R28** Sparkline buckets = min(width, ⌊TrendWindow / (2·interval)⌋), right-aligned — long intervals must not draw holes that read as "no data" — narrower sparklines at intervals over 15 s.
- **R29** (supersedes the plan's Task 7 deferred "action menu not reset") Any server switch closes the action menu (nothing sent), and the switcher calls `show()` before `rebuildTable` — a same-named (label, worker) on another server must not inherit an open menu — the menu closes on a switch.
- **R30** The 1h chart's peak label and scale are the peak of the bucket means it draws (as the sparklines do), not the raw hourly maximum — the gutter must describe the bars drawn — a one-poll spike reads lower than its raw value.

### Process

- **R3** Models for the build: task 1 Sonnet; tasks 2–7 Opus; tasks 8–10 Sonnet. Reviewers: Sonnet for tasks 1, 8, 9, 10; Opus for 2–7. The final review is a multi-dimension workflow with an adversarial verify step, on Opus.
- **R4** `GOTMPDIR` for every `go` command is the build's scratch dir under `.superpowers/sdd/2026-10-26-collector-daemon/gotmp` — the system temp quota was near full; scratch stays under the workspace.
- **R41** The final fix wave ran as one fresh implementer per slice, sequentially — A store, daemon and `fleet.Stale`; B cmd; C web and fleet; D TUI and fleet performance; E docs — then one re-review of the wave — about 30 items in one context would degrade — more dispatches.

### Other amendments

- **Lease check cost.** Every `Append` takes `control.db`'s write lock (ATTACH + IMMEDIATE transaction) to check the lease token, so writers, `Beat` and `Poke` serialize for milliseconds. "Servers never queue behind each other" (Store, pragmas) holds for the per-server files, not for that check.
- **Measured size.** ≈149 MB/day for a 10×20 server at 5s (≈8.3 KB compressed, ≈8.6 KB on disk per tick), where the Store section estimated 70–100; under the 256 MB cap.
- **Copy added during the build.** `This page is out of date — reload` (an old tab's POST naming a unit under the other noun's parameter, see R22); `No new data for {age}.` (stale, daemon running, no recorded error); `No servers are configured`.
- **TUI KPI line.** Values use fixed 7-column fields; sparklines appear from 73 columns.

### Final review fixes

What the whole-branch review (`39d7ac7..e1f8c65`) found and the fix wave changed, beyond R31–R44, that a maintainer should know:

- **Lease helpers** (R33, R34, R40). `store.DeadAfter = 3·BeatEvery`; `Control.BeatAdvances(ctx, token, within, now, sleep)` is true as soon as the lease with `token` shows a beat value other than the first it read (a beat that moved backwards counts), false once `within` passes or the token is gone; `Control.TakeOver(dead, l, now)` is `Claim` that a fresh lease of token `dead` does not stop.
- **control.db corruption is not blamed on server files.** An error from `Append`'s control.db read is never `IsCorrupt`; a `Begin` error is attributed by checking the server file alone (`blame`), so only a fault in that file sets it aside.
- **Undecodable rows.** `store.ErrUndecodable`: `Window` and `Tail` skip such a row and log it once per file through `store.SetLogger` (silent until set: `vartop web` sets it, the TUI never, since it owns the terminal); a collector whose saved labels or last snapshot cannot be decoded starts without them.
- **Writers.** A fresh server file gets the last saved label state on its first tick; a failed append reaches `SetServerErr` as `store: …` and is cleared by the next clean write. `Feed.Close` is final: a later `Sync` is a no-op that keeps the snapshots held.
- **cmd.** A relative `VARTOP_DATA_DIR` is written absolute in the service environment (as `spawnDetached` does); `daemon run` sends a crash's output to `daemon.log` (`debug.SetCrashOutput`, unless stderr is the log), which launchd and Task Scheduler would otherwise lose; `config.Save` writes through a symlink, via `os.CreateTemp` (0600) beside the target, removes the temp file on failure and an older vartop's `config.json.tmp`; the TUI's start error and the detached-start warning go on its error line, not stderr.
- **Web.** A page that is not current (R44) hides the balancer note, unit errors and down marks, and the detail "reporting" list, besides showing "—" for values; every page reads the lease once per request for the environment check.
- **TUI.** Worker names are path-escaped in actions; the detail viewport and footer widths are clamped, so a screen 1–3 rows or columns wide no longer panics; a feed hidden for 10 minutes is closed, and a kept feed whose latest went stale is reloaded on show; one tick sync is in flight per feed generation; an action, and `r`, poll and then wait for the snapshot the poke asked for (up to `SettleWait`), as the web does, in place of one 300 ms resync; every row's trend is built in one pass (`fleet.CompletedRatesOf`; 200×10 at 1 s: 595 → 187 ms).
- **Copy added.**
  - `The vartop daemon is collecting with another configuration — Start collector to use this one.` (web banner and 409 body) and `… — press r to use this one.` (TUI)
  - `cannot read the vartop daemon's lease: {err}` (R44: web 409 banner, TUI error line of a refused action)
  - `the vartop daemon is not responding; its lease expires within 15s` (`daemon stop`, exit 1)
  - `vartop daemon started detached; its login service failed: {err}`; `{err} (the vartop daemon runs detached meanwhile)`; `vartop daemon's login service: {err}` joined with the spawn's error
  - `data directory removed: {dir} no longer holds the control.db this daemon opened` (the daemon's exit error)
  - `{Pods|Instances} reporting: —` on a stale detail page
  - log lines `the daemon holding the lease does not beat; taking it over`, and `server removed` for each server when the config is deleted
- **Left open** (ledgered, not fixed): `RequestStop` is not bound to a token; `daemon stop` does not stop a supervised daemon that is crash-looping or not responding; a balanced unit unreached on the first tick after more than 24 h loses its number; the web detail form still prefills concurrency from a stale snapshot (its actions are refused anyway); the web Start collector button and the TUI's `r` drop the detached-start warning; each TUI trend build allocates about 85 MB at 200×10 and 1 s, two thirds of its time in `BuildDashboard` (a KPI-only builder would cut both).

### Web-only (R46)

- **R46** (owner's decision, 2026-10-27; plan `docs/superpowers/plans/2026-10-27-web-only.md`) vartop is the CLI plus `vartop web`; the daemon, its login service, the detached process and the TUI are removed — the owner wants one simple way to run it — the TUI's users lose it.
  - **Collection.** `vartop web` runs `collect.Run` (the daemon package, renamed `internal/collect`) in `collectLoop` for as long as it serves, and stops it when it exits, letting the lease go. Runs start at least 5 s apart; Run returns as soon as another collector's beat moves (and takes the lease over if it stays still for `store.DeadAfter`, or claims it at once if it is let go), so a second `vartop web` on the same data dir shows what the first collects and takes over within about 5 s of it stopping (about 11 s after a crash). Who collects is logged once per holder, not by Run on every call.
  - **The lease** is the one-writer guard between `vartop web` processes. It records pid, config path, `KUBECONFIG`, started and beat; the `build`, `path_env` and `stop` columns stay in `control.db`, written empty, so existing data dirs open unchanged (the table keeps its name, `daemon`). `RequestStop` and `Beat`'s stop result are gone.
  - **No longer apply:** the environment swap and version skew of R16/R31 (a vartop no longer restarts another's collector; the R31 check of the lease against this vartop's environment stays), R32 (detached fallback), R33 (kill guard), the login service and its environment allow-list, `daemon.log`, the Start collector button and `POST /daemon/start`, onboarding's login-service offer.
  - **Copy.** `… — vartop web is not collecting.` (stale, nothing collecting); `Another vartop (pid {pid}) is collecting with another configuration; this vartop web collects once it stops.` (banner and 409); `cannot read the collector's lease: {err}`; log `another vartop is collecting into this data directory; this one shows what it collects, and takes over once it stops`.
  - **Data directory deleted** under `vartop web`: the collecting one's Run returns `collect.ErrDataDirRemoved` at its next beat; one that only reads checks the control.db its pages read before each Run and returns the same error (a Run would make the directory anew, collect into it, and leave the pages refusing every action). Either stops with `data directory removed: {dir} no longer holds the control.db …; start vartop web again` — its pages would read the deleted files.
  - **Flags.** `--host` (default `127.0.0.1`, loopback only, as before) and `--port` (default `7070`, `0` any free port) replace `--addr`.
  - **Snapshot source** (final review). Each snapshot carries `Source`, `fleet.SourceOf` its server's config (a short hash of endpoints, Kubernetes and DNS settings; not the token). An action goes out only when the newest snapshot's Source is that of the config the request reads; else it pokes, waits up to `SettleWait`, and refuses with `The configuration of {server} changed; actions wait for its first snapshot under it. Try again in a few seconds.` — after an edit the token read now must not go to the old endpoints. The record field is optional (`src`): rows written before it read with none and are not acted on, so `StoreVersion` stays 1.
  - **Removed with the TUI:** `fleet.CompletedRates`, `CompletedRatesOf`, `CompletedRates1h`, `FailedRates1h`; `workerRates` folded back into `WorkerRates`.

### 24h charts (R47)

- **R47** (owner's decision, 2026-10-27) Each of a worker page's two charts gets its own **1h | 24h** switch (`completed=24h`, `failed=24h` in the address, kept by the poller, the instance picker and an action's refresh). The dashboard and grid keep their 10-minute trends.
  - **A day point** is a 5-minute bucket (`fleet.DayBucket`): the average of the polls with a rate; its tooltip the window, average, peak and the fewest instances that answered; the caption's peak the day's highest single poll; the axis every 6 hours on the local clock, found hour by hour so that a daylight-saving change keeps it on the clock.
  - **Stored as the collector writes** (owner chose it over reading a day of snapshots, ~9 s the first time on a 10 × 20 server): each server file has a `buckets` table (worker, bucket start, label → `fleet.DayStat`), upserted in `Append`'s transaction from `fleet.DayPoints` (merged and per label; a poll no unit answered names no worker and adds nothing). A page reads ≤ 289 × (labels + 1) rows through its feed's reader: 3.7 ms for a full day of a 10 × 20 server. `Append` with buckets: 1.2 ms per poll; buckets ≈ 3.8 MB a day for that server. `Prune` drops buckets that ended before the retention; the size cap leaves them alone.
  - **History from before the buckets** (a file without the table): `init` makes the table and records `fill_to` (the newest snapshot id then) and `filled` in meta; the collector's writer goroutine calls `FillBuckets` 50 snapshots at a time, every 50 ms between ticks, each batch one transaction under the lease check with its progress, so a fill resumes where it stopped and nothing is summed twice. A snapshots reset (new version, another key) resets the buckets with it. A reader of a file whose writer has not made the table yet reads an empty day.
  - `StoreVersion` stays 1: the table and the snapshot record's optional `src` are additive.

