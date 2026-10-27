<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/goptics/vartop/main/assets/vartop-dark.png">
  <source media="(prefers-color-scheme: light)" srcset="https://raw.githubusercontent.com/goptics/vartop/main/assets/vartop-light.png">
  <img alt="vartop" src="https://raw.githubusercontent.com/goptics/vartop/main/assets/vartop-dark.png" width="520">
</picture>

<h3 align="center">
  A beautiful terminal dashboard for monitoring and managing varmq workers.
</h3>

<p align="center">
  <a href="#installation"><strong>Installation</strong></a> ·
  <a href="#quick-start"><strong>Quick Start</strong></a> ·
  <a href="#usage"><strong>Usage</strong></a> ·
  <a href="#daemon"><strong>Daemon</strong></a> ·
  <a href="#configuration"><strong>Configuration</strong></a> ·
  <a href="#kubernetes"><strong>Kubernetes</strong></a> ·
  <a href="#load-balancers"><strong>Load balancers</strong></a> ·
  <a href="#keyboard-shortcuts"><strong>Shortcuts</strong></a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white" alt="Go">
  <img src="https://img.shields.io/badge/license-MIT-blue.svg?style=for-the-badge" alt="License: MIT">
  <img src="https://img.shields.io/badge/platform-terminal-black.svg?style=for-the-badge" alt="Platform: Terminal">
</p>

---

## About

**vartop** is a fast, interactive terminal UI (TUI) that connects to [varmq](https://github.com/goptics/varmq) mux servers and gives you real-time visibility into your worker fleet. View worker status, inspect metrics, tune concurrency, and control workers — all from the comfort of your terminal.

### Features

- **Live Dashboard** — colour-coded table of all workers showing status, concurrency, queue depth, and idle count with auto-refresh
- **Always-On Collector** — the [vartop daemon](#daemon) polls every configured server in the background into a local store (24 hours of history), so the TUI and the web UI open with their trends already drawn and always show the same numbers
- **Trends** — a completed/s trend per worker row and sparklines in the KPI line, plus 1 h completed/failed charts on the worker detail screen
- **Web UI** — `vartop web` serves the same dashboard in your browser on a loopback address: an auto-refreshing worker grid with trend sparklines, per-worker charts, and pause/resume/restart/stop and concurrency controls
- **Worker Detail View** — drill into a single worker to see full configuration, runtime metrics (submitted, completed, successful, failed), and success rate
- **Multi-Server & Multi-Pod Support** — configure multiple varmq servers, each with a static list of pod endpoints. View merged worker stats across all pods, or drill into a single pod via the Switch view
- **Kubernetes Discovery** — point a server at a Service or label selector and vartop watches its EndpointSlices, following pods as they scale, restart, or roll out; pods are labelled by pod name
- **DNS Discovery** — point a server at a hostname that resolves to every instance (headless Service, Docker Compose service, Consul) and vartop re-resolves it every few seconds
- **Load-Balancer Detection** — varmq names the instance behind every response; if one URL is answered by several instances, the daemon reaches each of them and both UIs list every instance behind it
- **Worker Control** — pause, resume, restart, and stop workers directly from the TUI or CLI
- **Concurrency Tuning** — adjust worker pool size in real time without restarting, via `vartop concurrency` (CLI)
- **Interactive Onboarding** — guided first-run wizard to configure your first server connection; it also offers to start the daemon at login
- **Responsive Layout** — adapts to narrow and wide terminal windows with compact and side-by-side layouts
- **Action Mode** — press <kbd>a</kbd> on the selected worker (dashboard row or detail view), then one key to pause, resume, restart, or stop it; in the merged view the action goes to that row's pod
- **Server Health Monitoring** — connection status indicator and automatic background polling with a configurable interval; a banner says so when the data goes stale

## Installation

Requires **Go 1.26+** (needed by the Kubernetes client libraries).

```bash
go install github.com/goptics/vartop@latest
```

Or build from source:

```bash
git clone https://github.com/goptics/vartop.git
cd vartop
make build   # binary at bin/vartop
```

## Quick Start

1. **Launch vartop**. On first run, an interactive wizard will help you set up your first server, then offers to start the [daemon](#daemon) at login:

   ```bash
   vartop
   ```

2. **Add more servers** (optional). `server add` opens the same interactive wizard: pick a name, then a **static endpoint**, **Kubernetes** discovery (context, namespace, service or label selector), or **DNS** discovery (host and port), and an optional token:

   ```bash
   vartop server add
   ```

   For a static multi-pod server, set all endpoints afterwards (comma-separated):

   ```bash
   vartop server set prod endpoint "http://pod-0:8080/varmq,http://pod-1:8080/varmq"
   ```

3. **Set a default server**:

   ```bash
   vartop server use production
   ```

4. **Launch the dashboard**:

   ```bash
   vartop
   ```

   Or connect to a specific server directly:

   ```bash
   vartop -s staging
   ```

vartop starts its [daemon](#daemon) by itself when none is running; there is nothing to set up for it.

## Usage

### CLI Commands

| Command | Description |
| --- | --- |
| `vartop` | Launch the interactive TUI dashboard |
| `vartop -s <name>` | Connect to a specific configured server |
| `vartop --server <name>` | Same as `-s` |
| `vartop web` | Open the dashboard in your browser, see [Web UI](#web-ui) |
| `vartop daemon` | Run or manage the background collector, see [Daemon](#daemon) |

#### Server Management

| Command | Description |
| --- | --- |
| `vartop server list` | List all servers with health status |
| `vartop server add` | Register a new server via the interactive wizard (static, Kubernetes or DNS) |
| `vartop server use <name>` | Set the default server |
| `vartop server remove <name>` | Remove a server |
| `vartop server remove --all` | Remove all servers |
| `vartop server set <name> endpoint\|token\|interval <value>` | Update a server property (`endpoint` accepts a comma-separated list; static servers only) |

#### Worker Operations

| Command | Description |
| --- | --- |
| `vartop action <worker> pause` | Pause a worker |
| `vartop action <worker> resume` | Resume a worker |
| `vartop action <worker> restart` | Restart a worker |
| `vartop action <worker> stop` | Stop a worker |
| `vartop concurrency <worker> <n>` | Set concurrency pool size |

Worker commands run against **every pod** of the active server (static endpoints or live Kubernetes pods) concurrently and print one result line per pod; the command fails if any pod fails. Note that `concurrency` is per pod, so `concurrency orders 8` on 3 pods gives 24 workers in total. To target a single pod, pass its full endpoint URL:

```bash
vartop action orders pause                                        # all pods
vartop action orders pause --origin http://10.0.0.5:8080/varmq    # one pod
vartop -s staging concurrency orders 8
```

### Web UI

`vartop web` serves the dashboard in your browser and opens it. It uses the same configuration as the TUI and reads the same store the [daemon](#daemon) fills, so it shows the same data and the same labels, with trend charts; workers can be controlled from the page too. It starts the daemon when none is running.

```bash
vartop web                          # http://127.0.0.1:7070, default server
vartop web -s staging               # a specific server
vartop web --addr 127.0.0.1:9000    # another port
vartop web --no-open                # print the URL, don't launch a browser
```

| Flag | Description | Default |
| --- | --- | --- |
| `--addr` | Address to listen on (must be a loopback address) | `127.0.0.1:7070` |
| `--no-open` | Don't open the browser | *(opens it)* |
| `-s`, `--server` | Server to show first; the page can switch to any configured server | *(default server, else the first configured)* |

- **Loopback only.** vartop refuses any other `--addr`, and the server only answers requests addressed to its own loopback host name.
- **The token stays on the server.** vartop calls varmq on the page's behalf; the bearer token is never sent to the browser.
- **History comes from the daemon.** Charts cover the last hour of what the daemon stored, so they are already there when the page opens and survive restarting `vartop web`. The page reads the store and never calls varmq to show data.
- **A stopped daemon is visible.** When no new data has arrived for three times the polling interval plus 2 seconds, a banner says how old the data is and why (the daemon is not running, or the server's error). Everything current shows `—` or is hidden (the values, the health count, the load-balancer note, per-instance errors, the list of reporting instances), and the history stays drawn. When no daemon is running the banner has a **Start collector** button. A server with only old history, say after the daemon was down for hours, shows the same banner with its age, not "Waiting for the first snapshot".
- **A daemon collecting with another configuration is refused.** The daemon collects with one configuration, the config path and `KUBECONFIG` of the vartop that started it (see [Environment](#environment)). If a vartop with another one restarts it while this page is open, the banner reads *The vartop daemon is collecting with another configuration — Start collector to use this one.*, the current values show `—`, and every action is refused with that text (HTTP 409) and sends nothing: the daemon's snapshots name endpoints found under the other configuration, and this server's token must not go to them. **Start collector** restarts the daemon with this vartop's configuration. The check reads the last daemon's lease, running or not, so it holds after that daemon stopped or crashed too, until a daemon with this vartop's configuration claims it.
- **Only current data is shown or acted on.** A snapshot is current when it is not stale, the last daemon's configuration is this vartop's, and it was not taken before that daemon started: an older one is a previous daemon's. Right after **Start collector**, or any daemon restart, the page therefore shows `—` and *No new data for {age}.* and refuses actions (HTTP 409, nothing sent) until the new daemon's first snapshot, up to about two polling intervals (longer while Kubernetes discovery connects). An action also needs a running daemon: with none, it is refused at once, with *… — the vartop daemon is not running.* If the daemon's lease cannot be read, actions are refused with *cannot read the vartop daemon's lease: {err}* and the page keeps showing data by the stale rule above.
- **Charts keep the time axis.** The sparklines span a fixed 10 minutes and the detail charts a fixed hour, each point placed at the time it was taken; a line breaks where polls are missing (the daemon was down, or the server unreachable), so a gap shows as a gap.
- **The detail charts have axes and a hover readout.** The two 1 h charts on a worker's page show the clock every 10 minutes along the bottom and a scale of 0, half the peak and the peak on the left, with faint gridlines. Hovering a chart draws a crosshair at the nearest poll and shows its time, value and how many instances answered, at once; over a stretch with no polls it shows nothing. A tap does the same on a touch screen. The dashboard's sparklines stay compact, with no axes.

Outside Kubernetes the UIs say **instance** where Kubernetes says pod, and label each one instance-1, instance-2, … in the order the daemon first sees them, static and DNS endpoints included; hover a label in the web UI for its instance ID and address. A label's number follows the instance ID, so it stays put when an endpoint turns out to be a load balancer, and stays with an address while its instance is down; an instance that comes back with a new ID gets a new number. Labels are stored per server, so the TUI and the web UI show the same ones and they survive daemon restarts; a number not seen for 24 hours is freed, and a new instance takes the lowest free number.

**Behind a load balancer**, each instance that answers through the balancer URL gets its own entry, and actions on **All instances (merged)** go through the balancer until every instance has answered or the retry limit is reached (at most 15 seconds), one result line each. Since the balancer picks which instance answers, an instance reached only through it has no actions of its own, and **Restart** is hidden on the merged view whenever such an instance is in it: restart an instance listed by its own address from its own entry, list the instances' own addresses in `endpoints`, or use [`kubernetes`](#kubernetes) or [`dns`](#dns) discovery. To reach every instance, vartop opens roughly K·(1 + ln K) short-lived connections per worker per refresh through a balancer with K instances behind it; for large fleets prefer `kubernetes` or `dns` discovery, or an interval of 5 seconds or more.

Press <kbd>Ctrl+C</kbd> to stop.

#### URL parameters

A page's address says what it shows, by the names the page shows:

| Parameter | Meaning |
| --- | --- |
| `server=<name>` | The configured server to show, by the key it is listed under in the config (the default server when absent, else the first configured server by name) |
| `instance=<label>` | Off Kubernetes: only that instance, e.g. `?instance=instance-2` |
| `pod=<pod name>` | On Kubernetes: only that pod |

Without `instance` or `pod` the page shows every instance (on Kubernetes, every pod) merged. An old `pod=` link opens the merged view: that is a `pod=` on a server that is not on Kubernetes, or on a Kubernetes one a value that is an endpoint key (`#…` or a URL), not a pod name. A label the server never had says so; one that is no longer reported says that, with a link to the merged view.

### Trends in the TUI

The TUI reads the same store as the web UI, so it shows the same numbers and labels, and draws trends from it:

- a **Trend** column on the dashboard: each row's completed/s over the last 10 minutes, as a sparkline
- sparklines in the KPI line above the table
- two 1 h charts (completed and failed) on the worker detail screen

The sparklines and charts need room, so they appear only when the terminal is wide enough. When the newest data is stale (no new snapshot for three times the polling interval plus 2 seconds) the header says how old it is and why (the daemon is not running, or the server's error), current values show `—`, the header leaves out the units' errors and the load-balancer note (the detail header the unit's address), and the history stays drawn. The same holds for a snapshot taken before the running daemon started, which is what is on screen after <kbd>r</kbd> starts or restarts the daemon, until its first snapshot, up to about two polling intervals. When the daemon collects with another configuration (see [Environment](#environment)), running or not (its last lease is compared), the header says *The vartop daemon is collecting with another configuration — press r to use this one.*, current values show `—`, and every action is refused with that text and sends nothing; <kbd>r</kbd> restarts the daemon with this vartop's configuration. If the daemon's lease cannot be read, actions are refused with *cannot read the vartop daemon's lease: {err}* on the error line.

### Keyboard Shortcuts

Every screen shows its available keys in a help footer at the bottom.

#### Dashboard View

| Key | Action |
| --- | --- |
| <kbd>↑</kbd> / <kbd>↓</kbd> or <kbd>k</kbd> / <kbd>j</kbd> | Navigate worker rows |
| <kbd>Enter</kbd> | View worker details |
| <kbd>a</kbd> | Enter action mode (pause, resume, stop, restart); <kbd>Esc</kbd> leaves it |
| <kbd>r</kbd> | Refresh now: ask the daemon to poll this server at once (starts the daemon if it is stopped) |
| <kbd>Tab</kbd> | Open Switch view (servers and pods) |
| <kbd>q</kbd> / <kbd>Ctrl+C</kbd> | Quit |

#### Worker Detail View

| Key | Action |
| --- | --- |
| <kbd>↑</kbd> / <kbd>↓</kbd> or <kbd>k</kbd> / <kbd>j</kbd> | Scroll one line |
| <kbd>PgUp</kbd> / <kbd>b</kbd>, <kbd>PgDn</kbd> / <kbd>Space</kbd> | Scroll one page |
| <kbd>g</kbd> / <kbd>Home</kbd>, <kbd>G</kbd> / <kbd>End</kbd> | Jump to top / bottom |
| <kbd>a</kbd> | Enter action mode |
| <kbd>r</kbd> | Refresh now (as on the dashboard) |
| <kbd>Esc</kbd> | Return to dashboard |
| <kbd>q</kbd> / <kbd>Ctrl+C</kbd> | Quit |

#### Action Mode (Dashboard & Worker Detail)

An action needs a running daemon with this vartop's configuration and current data. If the data is not current, the action first asks the daemon to poll and waits up to 2 seconds for it; if it is still not current, the action is refused and nothing is sent. With no daemon running it is refused at once, without waiting. A sent action asks the daemon to poll at once, and the screen shows the result when that poll lands, after up to 2 seconds (<kbd>r</kbd> waits the same way).

| Key | Action |
| --- | --- |
| <kbd>p</kbd> | Pause worker |
| <kbd>r</kbd> | Resume worker |
| <kbd>R</kbd> | Restart worker |
| <kbd>s</kbd> | Stop worker |
| <kbd>Esc</kbd> | Exit action mode |

#### Switch View

Servers are listed alphabetically, each with its health from its newest stored snapshot, so switching is instant. A server with more than one pod or instance lists them under it by label: the pod name on Kubernetes, instance-1, instance-2, … elsewhere.

| Key | Action |
| --- | --- |
| <kbd>↑</kbd> / <kbd>↓</kbd> or <kbd>k</kbd> / <kbd>j</kbd> | Navigate servers and pods |
| <kbd>Enter</kbd> | On a multi-pod server: expand to show pods. On an expanded server: select merged view. On a pod: select single-pod view |
| <kbd>h</kbd> / <kbd>Backspace</kbd> | Collapse the expanded server |
| <kbd>Esc</kbd> | Collapse the expanded server, or close the Switch view if nothing is expanded |
| <kbd>q</kbd> / <kbd>Ctrl+C</kbd> | Quit |

While the daemon is still connecting to a server (a Kubernetes watcher can take a few seconds to sync), the dashboard says it is waiting for the server's first snapshot. If the connection fails, the real cause the daemon recorded (for example an RBAC `forbidden` error) is shown with the server's name. A server that has older history but nothing newer shows the stale header with its age instead.

## Daemon

`vartop daemon` is a small background process that polls **every configured server**, all the time, and writes what it sees to a local SQLite store with 24 hours of history. `vartop` and `vartop web` don't poll varmq themselves: they read that store. That is why both show the same numbers and the same instance labels, why trends are already drawn when you open either one, and why history survives closing them.

The daemon runs until it is asked to stop (`vartop daemon stop`) or another daemon takes over, and idles when no server is configured. It also exits, with an error, when its store fails, as when its data directory is deleted under it (`data directory removed`): the login service then starts it again, and without one so does the next `vartop` or `vartop web`. It follows the config file, so adding, editing or removing a server takes effect within a few seconds without restarting it. Deleting `config.json` stops all collection, as if every server were removed; the servers' files stay for 24 hours after their last write, so putting the config back within a day resumes the history. A config that is there but does not parse or validate changes nothing: the daemon keeps collecting with the last good one and logs the error (one that starts on a broken config collects nothing until it is fixed). You rarely start it yourself:

- the first time you run vartop, after the onboarding wizard, it offers to install the [login service](#login-service)
- `vartop` and `vartop web` start a daemon when none is running — through the login service when that is installed, otherwise (or when the service fails, see [Login service](#login-service)) as a detached process of their own. If it cannot be started, `vartop web` prints why, and the TUI shows it on its error line instead of behind its screen: *vartop daemon did not start; see {data directory}/daemon.log*

| Command | Description |
| --- | --- |
| `vartop daemon` | Same as `vartop daemon run` |
| `vartop daemon run` | Collect in the foreground until stopped (<kbd>Ctrl+C</kbd>), unless another daemon already collects; the log goes to the terminal too |
| `vartop daemon start` | Start the daemon unless it is running |
| `vartop daemon stop` | Stop the daemon until your next login or the next time you open vartop |
| `vartop daemon restart` | Restart the daemon with this vartop's build and environment |
| `vartop daemon status` | Show the daemon's state and each server's newest data; exits with code 3 when it is not running |
| `vartop daemon install` | Start the daemon at login, and again after a crash (a login service) |
| `vartop daemon uninstall` | Remove the login service; vartop then starts the daemon itself |

`vartop daemon stop` says so when it is done: *vartop daemon stopped. It starts again at your next login or the next time you open vartop.* It asks the daemon to stop and waits 5 seconds. A daemon still there that keeps beating is killed. One that neither stops nor beats for 6 more seconds is left alone, since its process id may not be its own any more: `stop` fails with exit code 1 and *the vartop daemon is not responding; its lease expires within 15s*. Its lease expires within 15 seconds; `vartop daemon start`, or opening vartop, starts a new daemon, which takes that lease over.

`status` always shows the data directory and its size, the log file and whether the login service is installed. Only while the daemon runs does it also show the daemon's pid, uptime, build, config path, `KUBECONFIG` and `PATH`. For each configured server it shows how old its newest snapshot is, its units (and how many of them are healthy) and its last error.

**After a crash**, the dead daemon's lease stays valid for up to 15 seconds. A daemon that finds it, the login service's restart or one vartop spawns, watches the lease's heartbeat for 6 seconds and takes the lease over if it does not move; if it moves, the other daemon is alive and the new one exits quietly. vartop waits for that, up to 9 seconds instead of 3, before it reports that the daemon did not start. A crash's stack trace is written to `daemon.log`, also under launchd and Task Scheduler, which send a service's own stderr nowhere.

### Login service

`vartop daemon install` registers a per-user service of your OS that runs `vartop daemon run` at login and starts it again after a crash. It does not start it again after a deliberate `vartop daemon stop`.

| OS | Service | Restarts |
| --- | --- | --- |
| Linux | systemd user unit `vartop.service` in `$XDG_CONFIG_HOME/systemd/user` (default `~/.config/systemd/user`) | `Restart=on-failure` |
| macOS | LaunchAgent `dev.goptics.vartop` in `~/Library/LaunchAgents` | `RunAtLoad`; `KeepAlive` on failure |
| Windows | Scheduled task `vartop daemon` | At logon; restarts on failure |
| Any other (FreeBSD, …) | None: `vartop daemon install` fails with *vartop has no login service on {os}* | vartop starts a detached daemon itself, on the next `vartop` or `vartop web` after it ends |

The service file holds the absolute path of the vartop that installed it and a small part of its environment (see below), and is written with mode `0600`. While the service is installed, starting and restarting the daemon goes through your service manager, so the daemon stays supervised; `vartop daemon stop` makes it exit cleanly, which the service manager does not restart.

**When the service cannot start the daemon**, vartop does not leave nothing collecting. If the service manager refuses the service, or does not start it, when `vartop`, `vartop web`, `vartop daemon start` or `restart` go through it (a Linux session with no systemd user instance, say), or no daemon holds the lease within a few seconds of that, vartop spawns a detached daemon itself and warns: *vartop daemon started detached; its login service failed: {error}*. `vartop daemon start`, `restart` and `vartop web` print it, and the TUI shows it on its error line as it opens. If the detached daemon does not start either, the error names both failures. On Linux and macOS, a unit or LaunchAgent file that `install` wrote anew and the manager refused is removed again, so `status` goes on saying the login service is not installed. `vartop daemon install` itself only reports its error when no daemon was running; one that had stopped a running daemon starts it again, detached, and says so: *{error} (the vartop daemon runs detached meanwhile)*.

> **Tested on Linux.** The macOS and Windows service paths were only built and vetted on the development machine, not run.

> **Windows:** run `vartop daemon stop` before upgrading vartop. The running daemon locks the executable.

### Environment

What the daemon can reach depends on the environment it starts with: the config path (`XDG_CONFIG_HOME` or `HOME`), `KUBECONFIG`, the credential plugins on `PATH` that EKS, GKE and AKS kubeconfigs call, proxies, `VARTOP_DATA_DIR`. **The environment of the vartop you run wins.** When `vartop` or `vartop web` finds a daemon built from another binary, or one reading another config path or `KUBECONFIG`, it stops that daemon and starts one with its own environment, so the daemon follows whichever vartop you started last. `PATH` is not compared, so ordinary differences between shells restart nothing. With the login service installed, vartop first rewrites the environment the service saved, then restarts it through the service manager.

That swap happens as a vartop starts. A web page or TUI that was already open, under the old environment, finds its daemon replaced by one collecting with another configuration: it shows *The vartop daemon is collecting with another configuration* and refuses actions (see [Web UI](#web-ui)) until you press **Start collector** or <kbd>r</kbd>, which switches the daemon back to its own environment. Until the new daemon writes its first snapshot, up to about two polling intervals, the page shows `—` and refuses actions. A relative `VARTOP_DATA_DIR` is saved in the service as the absolute path it names, since the service manager runs the daemon from another directory.

The service saves only an allow-list of non-secret variables: `PATH`, `HOME`, `XDG_CONFIG_HOME`, `XDG_CACHE_HOME`, `VARTOP_DATA_DIR`, `KUBECONFIG`, the proxy variables (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` and their lowercase forms), `AWS_PROFILE`, `AWS_REGION`, `AWS_DEFAULT_REGION`, `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`, `CLOUDSDK_CONFIG`, `CLOUDSDK_ACTIVE_CONFIG_NAME`, `GOOGLE_APPLICATION_CREDENTIALS` and `AZURE_CONFIG_DIR`. A name containing `KEY`, `SECRET` or `TOKEN` is never written. `KUBECONFIG`, `XDG_CONFIG_HOME`, `XDG_CACHE_HOME` and `VARTOP_DATA_DIR` are always written, empty when unset, so the service manager's own values never take the place of yours.

### Data directory

The store lives in `VARTOP_DATA_DIR` when that is set, else in `vartop` under your OS user cache directory:

| OS | Default path |
| --- | --- |
| Linux | `$XDG_CACHE_HOME/vartop` (default `~/.cache/vartop`) |
| macOS | `~/Library/Caches/vartop` |
| Windows | `%LocalAppData%\vartop` |

| File | Holds |
| --- | --- |
| `control.db` | The daemon's lease, the UIs' requests to poll now, and each server's last collection error |
| `servers/<name>-<hash>.db` | One file per configured server: its snapshots (24 hours) and its instance labels |
| `daemon.log`, `daemon.log.1` | The daemon's log, with the stack trace of a crash; renamed to `.1` when it passes 5 MiB as a daemon starts and no other daemon is running |

The directory is created `0700` and its files `0600`. Tokens are never stored there: the daemon reads them from the [config](#configuration) when it polls. A server's file is deleted 24 hours after its last write. A configured server is written every poll, so that is about 24 hours after the server left the config: removing a server by mistake and adding it back under the same name within a day keeps its history. A daemon that was down for more than 24 hours deletes the file of a server that is no longer configured as soon as it starts: its history was past retention anyway, and only the server's instance numbers are lost.

### Disk use

A busy server, with 10 instances of 20 workers each, polled every 5 seconds, takes about **149 MB a day** (roughly 8.6 KB on disk per poll). History is kept for 24 hours, and each server's file is capped at **256 MB**: past that, the oldest rows go first. A server about 1.7 times busier than that one, by workers, instances or a shorter interval, therefore keeps less than 24 hours; the charts only cover the last hour, so they are not affected. Smaller fleets and longer intervals scale the figure down roughly in proportion: per day, bytes per poll × 86,400 ÷ the interval in seconds.

## Configuration

Configuration lives in your OS user config directory, created on first run (directory `0755`, file `0600` since it may hold tokens). The [daemon](#daemon) watches the file and picks up changes to it:

| OS | Path |
| --- | --- |
| Linux | `$XDG_CONFIG_HOME/vartop/config.json` (default `~/.config/vartop/config.json`) |
| macOS | `~/Library/Application Support/vartop/config.json` |
| Windows | `%AppData%\vartop\config.json` |

If `config.json` is a symlink, say into a dotfiles repository, vartop saves through it and the link stays.

### Schema

Each server uses **exactly one** of `endpoints` (static list), `kubernetes` or `dns` (live discovery). Setting more than one, or none, is rejected when the config is loaded.

```json
{
  "default_server": "production",
  "servers": {
    "production": {
      "name": "production",
      "endpoints": ["http://pod-0:8080/varmq", "http://pod-1:8080/varmq"],
      "token": "optional-bearer-token",
      "interval": 5
    },
    "cluster": {
      "name": "cluster",
      "kubernetes": {
        "context": "prod-eu",
        "namespace": "workers",
        "service": "varmq-mux",
        "port": 8080,
        "path": "/varmq"
      },
      "token": "optional-bearer-token"
    },
    "headless": {
      "name": "headless",
      "dns": {
        "host": "varmq-headless.workers.svc.cluster.local",
        "port": 8080,
        "path": "/varmq"
      }
    }
  }
}
```

| Field | Description | Default |
| --- | --- | --- |
| `default_server` | Server to use when no `-s` flag is given | *(none)* |
| `servers.<name>.endpoints` | Static list of mux endpoint URLs (one for a single pod, several for multi-pod) | *(one of endpoints / kubernetes / dns)* |
| `servers.<name>.kubernetes` | Kubernetes discovery block, see [Kubernetes](#kubernetes) | *(one of endpoints / kubernetes / dns)* |
| `servers.<name>.dns` | DNS discovery block, see [DNS](#dns) | *(one of endpoints / kubernetes / dns)* |
| `servers.<name>.token` | Bearer token for authentication (sent as `Authorization` header) | *(none)* |
| `servers.<name>.interval` | How often the daemon polls the server, in seconds | `5` |

### Kubernetes

| Field | Description | Default |
| --- | --- | --- |
| `context` | kubeconfig context to use (kubeconfig is read from `$KUBECONFIG` or `~/.kube/config`, in the [environment](#environment) of the vartop that started the daemon) | *(required)* |
| `namespace` | Namespace of the varmq pods | *(required)* |
| `service` | Service name; pods are found via its EndpointSlices (`kubernetes.io/service-name=<service>`) | *(one of service / selector)* |
| `selector` | EndpointSlice label selector, e.g. `app=varmq-mux` | *(one of service / selector)* |
| `port` | Port to dial on each pod | first port on the EndpointSlice |
| `path` | Mux API path on each pod | `/varmq` |

Only **ready** endpoints are used, so pods that are starting up or terminating are skipped. The `server add` wizard asks for context, namespace, and service or selector; to change `port` or `path`, edit the config file.

**Requirements**

- **Reachable pod IPs.** vartop's daemon dials each pod directly at `http://<pod-ip>:<port><path>`. There is no port-forward or API-server proxy, so run it from inside the cluster network or over a VPN or route that reaches pod IPs.
- **RBAC.** Your kube identity needs `list` and `watch` on EndpointSlices in the namespace:

  ```yaml
  apiVersion: rbac.authorization.k8s.io/v1
  kind: Role
  metadata:
    name: vartop
    namespace: workers
  rules:
    - apiGroups: ["discovery.k8s.io"]
      resources: ["endpointslices"]
      verbs: ["list", "watch"]
  ```

- **Plain HTTP.** Pods are dialed over `http://`. If you set a `token`, it travels unencrypted on the pod network, so only use it where that network is trusted.

If the watcher can't sync within 10 seconds (wrong context, no access, API server unreachable), vartop shows the underlying error, and the daemon tries again with a growing delay (5 seconds, doubling up to 5 minutes).

### DNS

Resolves a hostname to **all** of its addresses and dials each one, re-resolving every 5 seconds so instances that scale out or move are picked up. Works anywhere a name returns one address per instance:

- a Kubernetes **headless** Service (`clusterIP: None`) — no kubeconfig or RBAC needed, handy when vartop runs inside the cluster
- a Docker Compose service name with several replicas (`tasks.<service>` on Swarm)
- Consul, Nomad, or AWS Cloud Map service DNS

| Field | Description | Default |
| --- | --- | --- |
| `host` | Hostname to resolve (A/AAAA records) | *(required)* |
| `port` | Port each instance serves the mux on | *(required)* |
| `path` | Mux API path on each instance | `/varmq` |

An unresolvable host fails at connect time with the lookup error; a failed refresh later keeps the last known instances. Like Kubernetes discovery, instances are dialed directly over `http://`, so their addresses must be reachable from vartop. The `server add` wizard asks for host and port; edit the config file to change `path`.

## Load balancers

If a server is configured with a single **load-balancer URL** (an ingress, nginx, a cloud LB, or a normal ClusterIP Service), every request reaches *one* instance chosen by the balancer. For the CLI that means:

- an action (pause, stop, …) reaches one random instance, not all of them; a `pause` and the following `resume` can land on different instances

The CLI can't aggregate through a balancer, so it makes the situation visible. varmq (with this feature) adds an `X-Varmq-Instance` header naming the process that answered every API request, defaulting to the hostname (the pod name on Kubernetes) and settable via `varmq.InstanceID`. vartop uses it to label each CLI result line (`ok (instance pod-c)`) with the instance that served it.

The daemon, which does the polling for the TUI and the web UI, goes further: it sends requests through the balancer on new connections until every instance it knows of has answered or a retry limit is reached. Both UIs then list every instance behind the balancer as an entry of its own, labelled like any other, and the TUI header says how many answered behind each balancer endpoint. Detection is best-effort: a balancer that keeps a client on one backend per connection (most L4 balancers, kube-proxy) is still noticed, since every attempt is a new connection, but only instances that answered can be reported.

An instance reached only through a balancer has no actions of its own in the TUI: it says that a load balancer picks which instance answers, and sends nothing. **To restart or act on one instance from the TUI or the web UI, skip the balancer:** use [`kubernetes`](#kubernetes) discovery, [`dns`](#dns) with a headless Service or service-discovery name, or list the instance URLs in `endpoints`. A cloud load balancer's own hostname resolves to the balancer's nodes, not your instances, so `dns` doesn't help there.

> **Distributed queues:** with a shared (e.g. Redis) queue, every instance reports the same queue length as *Pending*, so don't add Pending up across rows.

## Architecture

```
vartop
├── cmd/                    # Cobra CLI commands
│   ├── root.go             # Root command — launches the TUI, a reader of the store
│   ├── daemon.go           # `vartop daemon` subcommands, ensureDaemon, build identity, status
│   ├── service.go          # Login service: unit/plist/task files, environment allow-list
│   ├── service_*.go        # The login service per OS: systemd (Linux), launchd (macOS), Task Scheduler (Windows)
│   ├── detach_*.go         # Detached daemon process (Unix, Windows)
│   ├── server.go           # Server management subcommands
│   ├── web.go              # `vartop web` — serves the browser dashboard
│   └── workers.go          # Worker action & concurrency subcommands (fan out to all pods)
├── internal/
│   ├── api/
│   │   ├── client.go       # REST client for one varmq mux endpoint (+ instance header tracking)
│   │   └── pool.go         # One client per discovered pod, reconciled from a discovery source
│   ├── config/
│   │   ├── config.go       # Config load/save + validation (<config dir>/vartop/config.json)
│   │   └── prompt.go       # Interactive onboarding / server add wizard
│   ├── discovery/
│   │   ├── discovery.go    # Source interface: live set of ready endpoint URLs
│   │   ├── static.go       # Fixed endpoint list from config
│   │   ├── kubernetes.go   # EndpointSlice informer (client-go) for a service or selector
│   │   └── dns.go          # Re-resolves a hostname to every instance address
│   ├── fleet/              # Model of a varmq fleet, no HTTP
│   │   ├── poller.go       # Collector: polls every unit of a server into snapshots
│   │   ├── history.go      # Snapshot types, rate calculator, 1 h ring
│   │   ├── balancer.go     # Reaches every instance behind a load balancer
│   │   ├── view.go         # Snapshots → dashboard, grid and detail view models
│   │   ├── act.go          # Worker action core, and the notices both UIs share
│   │   ├── noun.go         # pod / instance wording and the URL parameter
│   │   └── fleettest/      # Fake varmq servers and load balancers for tests
│   ├── store/              # SQLite store the daemon writes and the UIs read
│   │   ├── dir.go          # Data dir, file names, pragmas, retention and size cap
│   │   ├── control.go      # control.db: daemon lease, pokes, server errors
│   │   ├── server.go       # One file per server: snapshots, labels, pruning
│   │   ├── codec.go        # Snapshot ↔ compressed record
│   │   └── feed.go         # Reader: loads the 1 h window, then tails new rows
│   ├── daemon/
│   │   ├── daemon.go       # Run: lease, config watcher, pokes, pruning
│   │   ├── collector.go    # One per server: poll → writer → the server's file
│   │   └── config.go       # Watches the config file for changes
│   ├── tui/
│   │   ├── model.go        # Root Bubble Tea model — shared state, update loop, screen stack
│   │   ├── screen.go       # Screen interface + push/pop navigation
│   │   ├── dashboard.go    # Worker table, trend column, KPI line, status bar, stale/waiting states
│   │   ├── detail.go       # Single worker detail: config, metrics, 1 h charts, scrolling
│   │   ├── switcher.go     # Server / pod switcher
│   │   ├── chart.go        # Sparklines and block charts
│   │   ├── keys.go         # Per-screen key bindings
│   │   ├── help.go         # Help footer rendering
│   │   ├── handlers.go     # Async message handlers (feed sync, config, actions, resize)
│   │   ├── commands.go     # Bubble Tea commands (tick, sync, refresh/poke, actions)
│   │   ├── styles.go       # Tokyo Night colour theme & table styles
│   │   └── types.go        # Message types, switcher items, helpers
│   └── web/
│       ├── server.go       # Loopback listener, Host/CSRF guards, page handlers
│       ├── actions.go      # Worker action, concurrency, bulk and daemon-start POST routes
│       ├── render.go       # Template loading and page model
│       ├── templates/      # HTML pages (htmx-driven, server-rendered)
│       └── static/         # Embedded CSS and vendored htmx
├── main.go                 # Entry point
├── Makefile                # Build, run, clean, test targets
├── go.mod
└── go.sum
```

### API Endpoints Used

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/health` | Server health check |
| `GET` | `/workers` | List all workers with status |
| `GET` | `/workers/{name}` | Worker details & runtime metrics |
| `PATCH` | `/workers/{name}/actions/{action}` | Pause, resume, restart, stop |
| `PATCH` | `/workers/{name}/config/concurrency/{n}` | Update concurrency pool size |

### Dependencies

| Library | Purpose |
| --- | --- |
| [Bubble Tea](https://github.com/charmbracelet/bubbletea) | Elm Architecture terminal framework |
| [Bubbles](https://github.com/charmbracelet/bubbles) | Table & list TUI widgets |
| [Lip Gloss](https://github.com/charmbracelet/lipgloss) | Terminal styling & layout |
| [Huh](https://github.com/charmbracelet/huh) | Interactive form prompts |
| [Cobra](https://github.com/spf13/cobra) | CLI command parsing |
| [pterm](https://github.com/pterm/pterm) | Big-text ASCII art banner |
| [client-go](https://github.com/kubernetes/client-go) (+ `k8s.io/api`, `k8s.io/apimachinery`) | Kubernetes EndpointSlice discovery |
| [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) | Pure-Go SQLite (no cgo) for the daemon's store |

## Development

### Commands

| Command | Description |
| --- | --- |
| `make build` | Compile binary to `bin/vartop` |
| `make run` | Build and launch the TUI |
| `make test` | Run all tests |
| `make clean` | Remove build artifacts |

### Go Commands

```bash
go build -o bin/vartop main.go
./bin/vartop
go test ./...
```

## Contributing

Contributions are welcome! Here's how to get started:

1. Fork the repository
2. Create a feature branch (`git checkout -b feat/amazing-feature`)
3. Make your changes
4. Run tests (`make test`)
5. Commit using [conventional commits](https://www.conventionalcommits.org/)
6. Push and open a pull request

Please keep the code idiomatic, the TUI responsive across terminal sizes, and include tests for new functionality.

## License

MIT © [Goptics](https://github.com/goptics)

## Acknowledgements

vartop is built on the incredible [Charmbracelet](https://charm.sh) ecosystem. If you're building terminal tools in Go, check them out — Bubble Tea, Lip Gloss, Glamour, Huh, and friends are a joy to work with.
