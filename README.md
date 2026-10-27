<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/goptics/vartop/main/assets/vartop-dark.png">
  <source media="(prefers-color-scheme: light)" srcset="https://raw.githubusercontent.com/goptics/vartop/main/assets/vartop-light.png">
  <img alt="vartop" src="https://raw.githubusercontent.com/goptics/vartop/main/assets/vartop-dark.png" width="520">
</picture>

<h3 align="center">
  Monitor and manage varmq workers from your browser and the command line.
</h3>

<p align="center">
  <a href="#installation"><strong>Installation</strong></a> ·
  <a href="#quick-start"><strong>Quick Start</strong></a> ·
  <a href="#usage"><strong>Usage</strong></a> ·
  <a href="#collection"><strong>Collection</strong></a> ·
  <a href="#configuration"><strong>Configuration</strong></a> ·
  <a href="#kubernetes"><strong>Kubernetes</strong></a> ·
  <a href="#load-balancers"><strong>Load balancers</strong></a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/go-%2300ADD8.svg?style=for-the-badge&logo=go&logoColor=white" alt="Go">
  <img src="https://img.shields.io/badge/license-MIT-blue.svg?style=for-the-badge" alt="License: MIT">
  <img src="https://img.shields.io/badge/platform-browser%20%2B%20cli-black.svg?style=for-the-badge" alt="Platform: browser and CLI">
</p>

---

## About

**vartop** connects to [varmq](https://github.com/goptics/varmq) mux servers and gives you real-time visibility into your worker fleet: a web dashboard with live trends, and command-line tools to pause, resume, restart, stop and tune workers.

### Features

- **Web Dashboard** — `vartop web` serves an auto-refreshing worker grid in your browser on a loopback address, with trend sparklines, per-worker 1 h charts with axes and a hover readout, and pause/resume/restart/stop and concurrency controls
- **History** — while `vartop web` runs it polls every configured server into a local SQLite store with 24 hours of history, so the charts are still there after you restart it
- **Worker Detail** — drill into a single worker to see its runtime metrics (submitted, completed, successful, failed) and success rate
- **Multi-Server & Multi-Pod Support** — configure multiple varmq servers, each with a static list of pod endpoints. View merged worker stats across all pods, or a single pod or instance
- **Kubernetes Discovery** — point a server at a Service or label selector and vartop watches its EndpointSlices, following pods as they scale, restart, or roll out; pods are labelled by pod name
- **DNS Discovery** — point a server at a hostname that resolves to every instance (headless Service, Docker Compose service, Consul) and vartop re-resolves it every few seconds
- **Load-Balancer Detection** — varmq names the instance behind every response; if one URL is answered by several instances, vartop web reaches each of them and lists every instance behind it
- **Worker Control from the CLI** — `vartop action` and `vartop concurrency` run against every pod of a server at once
- **Interactive Onboarding** — guided first-run wizard to configure your first server connection
- **Stale Data Made Visible** — a banner says when the data goes stale, and why

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

1. **Start the dashboard**. On first run, an interactive wizard will help you set up your first server:

   ```bash
   vartop web
   ```

   It opens http://127.0.0.1:7070 in your browser and collects every configured server until you stop it with <kbd>Ctrl+C</kbd>.

2. **Add more servers** (optional). `server add` opens the same interactive wizard: pick a name, then a **static endpoint**, **Kubernetes** discovery (context, namespace, service or label selector), or **DNS** discovery (host and port), and an optional token. A running `vartop web` picks the new server up within a few seconds:

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

4. **Open a specific server** first:

   ```bash
   vartop web -s staging
   ```

## Usage

### CLI Commands

| Command | Description |
| --- | --- |
| `vartop` | Print help |
| `vartop web` | Serve the dashboard in your browser and collect every server while it runs, see [Web UI](#web-ui) |
| `-s`, `--server <name>` | The server `web`, `action` and `concurrency` use (overrides the default) |

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

`vartop web` serves the dashboard in your browser and opens it. While it runs it collects every configured server (see [Collection](#collection)), so switching servers on the page is instant and their trend charts are already drawn; workers can be controlled from it too.

```bash
vartop web                             # http://127.0.0.1:7070, default server
vartop web -s staging                  # a specific server first
vartop web --port 9000                 # another port
vartop web --host localhost --port 0   # any free port
vartop web --no-open                   # print the URL, don't launch a browser
```

| Flag | Description | Default |
| --- | --- | --- |
| `--host` | Host to listen on (must be a loopback address) | `127.0.0.1` |
| `--port` | Port to listen on; `0` picks any free port | `7070` |
| `--no-open` | Don't open the browser | *(opens it)* |
| `-s`, `--server` | Server to show first; the page can switch to any configured server | *(default server, else the first configured)* |

- **Loopback only.** The dashboard has no login, and whoever reaches it can stop your workers, so vartop refuses any `--host` that is not a loopback address (`127.0.0.1`, `localhost`, `::1`), and the server only answers requests addressed to its own loopback host name.
- **The token stays on the server.** vartop calls varmq on the page's behalf; the bearer token is never sent to the browser.
- **History survives a restart.** Charts cover the last hour of what was stored, so they are back when you start `vartop web` again. The page reads the store and never calls varmq to show data.
- **Stale data is visible.** When no new data has arrived for three times the polling interval plus 2 seconds, a banner says how old the data is and why (vartop web is not collecting, or the server's error). Everything current shows `—` or is hidden (the values, the health count, the load-balancer note, per-instance errors, the list of reporting instances), and the history stays drawn. A server with only old history, say after vartop web was not running for hours, shows the same banner with its age, not "Waiting for the first snapshot".
- **Another configuration is refused.** Two `vartop web` on one machine share the [data directory](#data-directory), and only one collects (see [Collection](#collection)). If the one collecting reads another config file or `KUBECONFIG`, the other's pages read *Another vartop (pid {pid}) is collecting with another configuration; this vartop web collects once it stops.*, the current values show `—`, and every action is refused with that text (HTTP 409) and sends nothing: its snapshots name endpoints found under the other configuration, and this server's token must not go to them. The check reads the last collector's lease, running or not, so it holds after that one stopped or crashed too, until this vartop web collects.
- **Only current data is shown or acted on.** A snapshot is current when it is not stale, the collector's configuration is this vartop's, and it was not taken before that collector started: an older one is a previous collector's. Right after vartop web starts, or takes over from another, the page therefore shows `—` and *No new data for {age}.* and refuses actions (HTTP 409, nothing sent) until its first snapshot, up to about two polling intervals (longer while Kubernetes discovery connects). With nothing collecting, an action is refused at once, with *… — vartop web is not collecting.* If the collector's lease cannot be read, actions are refused with *cannot read the collector's lease: {err}* and the page keeps showing data by the stale rule above. After a server's endpoints (or Kubernetes or DNS settings) are edited, its newest snapshot still names the old units until the collector restarts it, a few seconds; an action meanwhile is refused with *The configuration of {server} changed; actions wait for its first snapshot under it. Try again in a few seconds.*, and nothing is sent: this server's token must not go to endpoints the config no longer lists.
- **Charts keep the time axis.** The sparklines span a fixed 10 minutes and the detail charts a fixed hour, each point placed at the time it was taken; a line breaks where polls are missing (vartop web was not running, or the server unreachable), so a gap shows as a gap.
- **The detail charts have axes and a hover readout.** The two 1 h charts on a worker's page show the clock every 10 minutes along the bottom and a scale of 0, half the peak and the peak on the left, with faint gridlines. Hovering a chart draws a crosshair at the nearest poll and shows its time, value and how many instances answered, at once; over a stretch with no polls it shows nothing. A tap does the same on a touch screen. The dashboard's sparklines stay compact, with no axes.
- **Each chart shows the hour or the day.** Completed /s and Failed /s on a worker's page each have a **1h | 24h** switch of their own. At 24h a chart spans the last day in 5-minute buckets: each point is the bucket's average rate, hovering one shows *13:05–13:10 · avg 12.4/s · peak 15.1/s · 3/3 instances* (how many answered at the poll in that window that missed the most; a poll at which none answered shows only on the 1h chart), the caption's peak is the day's highest single poll, and the clock is marked every 6 hours. The choice is in the address (`?completed=24h&failed=24h`), so a reload, the refresh, the instance picker and a shared link keep it. The dashboard's 10-minute trends stay as they are.
- **Connecting takes a moment.** While vartop web is still connecting to a server (a Kubernetes watcher can take a few seconds to sync), the page says it is waiting for the server's first snapshot. If the connection fails, the real cause (for example an RBAC `forbidden` error) is shown with the server's name.

Outside Kubernetes the page says **instance** where Kubernetes says pod, and labels each one instance-1, instance-2, … in the order vartop first sees them, static and DNS endpoints included; hover a label for its instance ID and address. A label's number follows the instance ID, so it stays put when an endpoint turns out to be a load balancer, and stays with an address while its instance is down; an instance that comes back with a new ID gets a new number. Labels are stored per server, so they survive restarting vartop web; a number not seen for 24 hours is freed, and a new instance takes the lowest free number.

**Behind a load balancer**, each instance that answers through the balancer URL gets its own entry, and actions on **All instances (merged)** go through the balancer until every instance has answered or the retry limit is reached (at most 15 seconds), one result line each. Since the balancer picks which instance answers, an instance reached only through it has no actions of its own, and **Restart** is hidden on the merged view whenever such an instance is in it: restart an instance listed by its own address from its own entry, list the instances' own addresses in `endpoints`, or use [`kubernetes`](#kubernetes) or [`dns`](#dns) discovery. To reach every instance, vartop opens roughly K·(1 + ln K) short-lived connections per worker per refresh through a balancer with K instances behind it; for large fleets prefer `kubernetes` or `dns` discovery, or an interval of 5 seconds or more.

Press <kbd>Ctrl+C</kbd> to stop; collection stops with it.

#### URL parameters

A page's address says what it shows, by the names the page shows:

| Parameter | Meaning |
| --- | --- |
| `server=<name>` | The configured server to show, by the key it is listed under in the config (the default server when absent, else the first configured server by name) |
| `instance=<label>` | Off Kubernetes: only that instance, e.g. `?instance=instance-2` |
| `pod=<pod name>` | On Kubernetes: only that pod |

Without `instance` or `pod` the page shows every instance (on Kubernetes, every pod) merged. An old `pod=` link opens the merged view: that is a `pod=` on a server that is not on Kubernetes, or on a Kubernetes one a value that is an endpoint key (`#…` or a URL), not a pod name. A label the server never had says so; one that is no longer reported says that, with a link to the merged view.

## Collection

`vartop web` polls **every configured server** while it runs, not only the one on screen, and writes what it sees to a local SQLite store with 24 hours of history. Its pages read that store. When it stops, collection stops too: nothing keeps running in the background.

- **It follows the config file.** Adding, editing or removing a server (with `vartop server add` in another terminal, say) takes effect within a few seconds. Deleting `config.json` stops all collection, as if every server were removed; the servers' files stay for 24 hours after their last write, so putting the config back within a day resumes the history. A config that stops parsing or validating while vartop web runs changes nothing: it keeps collecting with the last good one and logs the error. (A broken config keeps vartop web from starting.)
- **One collector per data directory.** A second `vartop web` on the same machine (on another `--port`) does not collect: it logs *another vartop is collecting into this data directory; this one shows what it collects, and takes over once it stops* and shows what the first one collects. Within about 5 seconds of the first one stopping (about 11 if it crashed), it collects itself.
- **After a crash**, the dead process's lease stays valid for up to 15 seconds. A `vartop web` started meanwhile watches the lease's heartbeat for 6 seconds and takes it over if it does not move.
- **Its data directory deleted** while it runs (by a cache cleaner, say), vartop web stops, whether it collects or only shows what another collects, with *data directory removed: {dir} no longer holds the control.db …; start vartop web again*.
- **History stored by an older vartop is filled into the 24h charts once.** A server file from before the 5-minute buckets has its stored polls summed into them in the background after vartop web starts: 50 polls at a time, each batch followed by a pause at least as long, and without holding anything the collector's own polls or the lease need, so collection carries on meanwhile. That takes about 15 seconds for 12,000 polls of a small fleet, and about 45 seconds per server for a full day of a 10 × 20 server polled every 5 seconds. Until it is done, the 24h charts show the part already filled.
- **It logs state changes only**, to its terminal: servers added, removed or restarted, a server that cannot be polled at all (discovery or connection failing) and its recovery, store problems, and who collects. An instance that stops answering shows on the page, not in the log.

### Environment

What vartop web can reach depends on the environment you start it in: the config path (`XDG_CONFIG_HOME` or `HOME`), `KUBECONFIG`, the credential plugins on `PATH` that EKS, GKE and AKS kubeconfigs call, proxies, `VARTOP_DATA_DIR`. Two `vartop web` that differ in config path or `KUBECONFIG` share the data directory but not each other's configuration: see *Another configuration is refused* under [Web UI](#web-ui).

### Data directory

The store lives in `VARTOP_DATA_DIR` when that is set, else in `vartop` under your OS user cache directory:

| OS | Default path |
| --- | --- |
| Linux | `$XDG_CACHE_HOME/vartop` (default `~/.cache/vartop`) |
| macOS | `~/Library/Caches/vartop` |
| Windows | `%LocalAppData%\vartop` |

| File | Holds |
| --- | --- |
| `control.db` | The collector's lease, the pages' requests to poll now, and each server's last collection error |
| `servers/<name>-<hash>.db` | One file per configured server: its snapshots (24 hours), each worker's rates in 5-minute buckets for the 24h charts (summed as each poll is stored), and its instance labels |

The directory is created `0700` and its files `0600`. Tokens are never stored there: vartop web reads them from the [config](#configuration) when it polls. A server's file is deleted 24 hours after its last write. A configured server is written every poll while vartop web runs, so that is about 24 hours after the server left the config: removing a server by mistake and adding it back under the same name within a day keeps its history. A vartop web started after more than 24 hours deletes the file of a server that is no longer configured as soon as it starts: its history was past retention anyway, and only the server's instance numbers are lost.

### Disk use

A busy server, with 10 instances of 20 workers each, polled every 5 seconds, takes about **149 MB a day** (roughly 8.6 KB on disk per poll). History is kept for 24 hours, and each server's file is capped at **256 MB**: past that, the oldest rows go first. A server about 1.7 times busier than that one, by workers, instances or a shorter interval, therefore keeps less than 24 hours of snapshots. The 1h charts are not affected, and neither are the 24h charts: they read the 5-minute buckets, which the cap leaves alone and which take about **4 MB a day** for that server. Smaller fleets and longer intervals scale the figure down roughly in proportion: per day, bytes per poll × 86,400 ÷ the interval in seconds.

## Configuration

Configuration lives in your OS user config directory, created on first run (directory `0755`, file `0600` since it may hold tokens). A running `vartop web` watches the file and picks up changes to it (see [Collection](#collection)):

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
| `servers.<name>.interval` | How often vartop web polls the server, in seconds | `5` |

### Kubernetes

| Field | Description | Default |
| --- | --- | --- |
| `context` | kubeconfig context to use (kubeconfig is read from `$KUBECONFIG` or `~/.kube/config`, in the [environment](#environment) vartop web runs in) | *(required)* |
| `namespace` | Namespace of the varmq pods | *(required)* |
| `service` | Service name; pods are found via its EndpointSlices (`kubernetes.io/service-name=<service>`) | *(one of service / selector)* |
| `selector` | EndpointSlice label selector, e.g. `app=varmq-mux` | *(one of service / selector)* |
| `port` | Port to dial on each pod | first port on the EndpointSlice |
| `path` | Mux API path on each pod | `/varmq` |

Only **ready** endpoints are used, so pods that are starting up or terminating are skipped. The `server add` wizard asks for context, namespace, and service or selector; to change `port` or `path`, edit the config file.

**Requirements**

- **Reachable pod IPs.** vartop dials each pod directly at `http://<pod-ip>:<port><path>`. There is no port-forward or API-server proxy, so run it from inside the cluster network or over a VPN or route that reaches pod IPs.
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

If the watcher can't sync within 10 seconds (wrong context, no access, API server unreachable), vartop shows the underlying error, and vartop web tries again with a growing delay (5 seconds, doubling up to 5 minutes).

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

vartop web goes further: it sends requests through the balancer on new connections until every instance it knows of has answered or a retry limit is reached. The dashboard then lists every instance behind the balancer as an entry of its own, labelled like any other, and its header says how many answered behind each balancer endpoint. Detection is best-effort: a balancer that keeps a client on one backend per connection (most L4 balancers, kube-proxy) is still noticed, since every attempt is a new connection, but only instances that answered can be reported.

An instance reached only through a balancer has no actions of its own on the dashboard, since a load balancer picks which instance answers. **To restart or act on one instance, skip the balancer:** use [`kubernetes`](#kubernetes) discovery, [`dns`](#dns) with a headless Service or service-discovery name, or list the instance URLs in `endpoints`. A cloud load balancer's own hostname resolves to the balancer's nodes, not your instances, so `dns` doesn't help there.

> **Distributed queues:** with a shared (e.g. Redis) queue, every instance reports the same queue length as *Pending*, so don't add Pending up across rows.

## Architecture

```
vartop
├── cmd/                    # Cobra CLI commands
│   ├── root.go             # Root command, onboarding
│   ├── server.go           # Server management subcommands
│   ├── web.go              # `vartop web` — serves the dashboard and runs the collector while it does
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
│   │   ├── act.go          # Worker action core, and the dashboard's notices
│   │   ├── noun.go         # pod / instance wording and the URL parameter
│   │   └── fleettest/      # Fake varmq servers and load balancers for tests
│   ├── store/              # SQLite store the collector writes and the pages read
│   │   ├── dir.go          # Data dir, file names, pragmas, retention and size cap
│   │   ├── control.go      # control.db: the collector's lease, pokes, server errors
│   │   ├── server.go       # One file per server: snapshots, labels, pruning
│   │   ├── codec.go        # Snapshot ↔ compressed record
│   │   └── feed.go         # Reader: loads the 1 h window, then tails new rows
│   ├── collect/
│   │   ├── collect.go      # Run: lease, config watcher, pokes, pruning
│   │   ├── collector.go    # One per server: poll → writer → the server's file
│   │   └── config.go       # Watches the config file for changes
│   └── web/
│       ├── server.go       # Loopback listener, Host/CSRF guards, page handlers
│       ├── actions.go      # Worker action, concurrency and bulk POST routes
│       ├── render.go       # Template loading and page model
│       ├── templates/      # HTML pages (htmx-driven, server-rendered)
│       └── static/         # Embedded CSS, the detail page's script (chart hover, concurrency inputs) and vendored htmx
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
| [Huh](https://github.com/charmbracelet/huh) | Interactive form prompts (onboarding, `server add`) |
| [Lip Gloss](https://github.com/charmbracelet/lipgloss) | Styling of those prompts |
| [Cobra](https://github.com/spf13/cobra) | CLI command parsing |
| [pterm](https://github.com/pterm/pterm) | Big-text ASCII art banner |
| [client-go](https://github.com/kubernetes/client-go) (+ `k8s.io/api`, `k8s.io/apimachinery`) | Kubernetes EndpointSlice discovery |
| [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) | Pure-Go SQLite (no cgo) for the history store |
| [htmx](https://htmx.org) (vendored) | The dashboard's live updates, without a build step |

## Development

### Commands

| Command | Description |
| --- | --- |
| `make build` | Compile binary to `bin/vartop` |
| `make run` | Build and run `vartop web` |
| `make test` | Run all tests |
| `make clean` | Remove build artifacts |

### Go Commands

```bash
go build -o bin/vartop main.go
./bin/vartop web
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

Please keep the code idiomatic, the dashboard usable from phone to desktop widths, and include tests for new functionality.

## License

MIT © [Goptics](https://github.com/goptics)

## Acknowledgements

vartop's onboarding wizard is built on [Charmbracelet](https://charm.sh)'s Huh and Lip Gloss, and the dashboard on [htmx](https://htmx.org).
