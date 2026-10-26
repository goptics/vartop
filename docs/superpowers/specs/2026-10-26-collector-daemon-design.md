# vartop collector daemon + shared SQLite history, label URLs — design

Amends `2026-10-25-web-ui-design.md` and `2026-10-26-web-ui-instances-design.md`. Where they disagree, this one wins.

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
- **Actions:** clients built from the latest snapshot (direct unit `Key`, balancer `Behind` keys + balanced `Via`) with the token read from config at action time; `scopeOf`/`act`/reacher unchanged except matching by label; R18's "unrefreshed" branch goes away. Stale snapshot → poke and wait ≤2s, still stale → 409, nothing sent. After an action: poke, tail until a snapshot newer than the poke (≤ `settleWait` 2s), render from it.
- **Web:** server comes from each request (`?server=`), Manager/Session/`POST /server` removed, server picker becomes a GET form; feeds cached per server (dropped after 10 min unused); GET never calls varmq. "Start collector" button in the stale banner (POST behind the existing guards).
- **Label URL:** `Noun.Param` is "pod" (Kubernetes) or "instance"; `pageURL`, picker, and every hidden input use it with the label as value; views filter units by label across snapshots (a process's chart survives its key changing); POSTs resolve the label against the fresh latest snapshot (gone → 409, nothing sent); a label found only in older snapshots → gone banner with a link to the merged view; unknown → "instance-9 is not an instance of {server}". Old `pod=` links show the merged view.
- **TUI:** 1s tick syncs the feed (stale guard by server + feed generation, replacing `scopedMsg`/`*api.Pool`); rows = unit × worker from the latest snapshot (Node = label); header shows `Health`, `BalancerNote`, stale age; switcher health from each server's latest snapshot, switching is instant; `r` pokes (and starts a stopped daemon). Actions keep today's keys on the selected row's unit (balanced unit → refused with the one-instance message); direct client + poke.
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
