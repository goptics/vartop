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
- **Worker Detail View** — drill into a single worker to see full configuration, runtime metrics (submitted, completed, successful, failed), and success rate
- **Multi-Server & Multi-Pod Support** — configure multiple varmq servers, each with a static list of pod endpoints. View merged worker stats across all pods, or drill into a single pod via the Switch view
- **Kubernetes Discovery** — point a server at a Service or label selector and vartop watches its EndpointSlices, following pods as they scale, restart, or roll out; pods are labelled by pod name
- **DNS Discovery** — point a server at a hostname that resolves to every instance (headless Service, Docker Compose service, Consul) and vartop re-resolves it every few seconds
- **Load-Balancer Detection** — varmq names the instance behind every response; if one URL is answered by several instances, vartop warns that it is a load balancer and labels each row with the instance that served it
- **Worker Control** — pause, resume, restart, and stop workers directly from the TUI or CLI
- **Concurrency Tuning** — adjust worker pool size in real time without restarting, via `vartop concurrency` (CLI)
- **Interactive Onboarding** — guided first-run wizard to configure your first server connection
- **Responsive Layout** — adapts to narrow and wide terminal windows with compact and side-by-side layouts
- **Action Mode** — press <kbd>a</kbd> on the selected worker (dashboard row or detail view), then one key to pause, resume, restart, or stop it; in the merged view the action goes to that row's pod
- **Server Health Monitoring** — connection status indicator and automatic background polling with configurable interval

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

1. **Launch vartop**. On first run, an interactive wizard will help you set up your first server:

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

## Usage

### CLI Commands

| Command | Description |
| --- | --- |
| `vartop` | Launch the interactive TUI dashboard |
| `vartop -s <name>` | Connect to a specific configured server |
| `vartop --server <name>` | Same as `-s` |

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

### Keyboard Shortcuts

Every screen shows its available keys in a help footer at the bottom.

#### Dashboard View

| Key | Action |
| --- | --- |
| <kbd>↑</kbd> / <kbd>↓</kbd> or <kbd>k</kbd> / <kbd>j</kbd> | Navigate worker rows |
| <kbd>Enter</kbd> | View worker details |
| <kbd>a</kbd> | Toggle action mode (pause, resume, stop, restart) |
| <kbd>r</kbd> | Refresh data (also retries a server connection that failed, e.g. a bad kube context) |
| <kbd>Tab</kbd> | Open Switch view (servers and pods) |
| <kbd>q</kbd> / <kbd>Ctrl+C</kbd> | Quit |

#### Worker Detail View

| Key | Action |
| --- | --- |
| <kbd>↑</kbd> / <kbd>↓</kbd> or <kbd>k</kbd> / <kbd>j</kbd> | Scroll one line |
| <kbd>PgUp</kbd> / <kbd>b</kbd>, <kbd>PgDn</kbd> / <kbd>Space</kbd> | Scroll one page |
| <kbd>g</kbd> / <kbd>Home</kbd>, <kbd>G</kbd> / <kbd>End</kbd> | Jump to top / bottom |
| <kbd>a</kbd> | Toggle action mode |
| <kbd>r</kbd> | Refresh worker details |
| <kbd>Esc</kbd> | Return to dashboard |
| <kbd>q</kbd> / <kbd>Ctrl+C</kbd> | Quit |

#### Action Mode (Dashboard & Worker Detail)

| Key | Action |
| --- | --- |
| <kbd>p</kbd> | Pause worker |
| <kbd>r</kbd> | Resume worker |
| <kbd>R</kbd> | Restart worker |
| <kbd>s</kbd> | Stop worker |
| <kbd>Esc</kbd> | Exit action mode |

#### Switch View

Servers are listed alphabetically. The active Kubernetes server lists its live pods by pod name (other Kubernetes servers show their pods once selected); static servers list their configured endpoints.

| Key | Action |
| --- | --- |
| <kbd>↑</kbd> / <kbd>↓</kbd> or <kbd>k</kbd> / <kbd>j</kbd> | Navigate servers and pods |
| <kbd>Enter</kbd> | On a multi-pod server: expand to show pods. On an expanded server: select merged view. On a pod: select single-pod view |
| <kbd>h</kbd> / <kbd>Backspace</kbd> | Collapse the expanded server |
| <kbd>Esc</kbd> | Collapse the expanded server, or close the Switch view if nothing is expanded |
| <kbd>q</kbd> / <kbd>Ctrl+C</kbd> | Quit |

While a server connection is being set up (a Kubernetes watcher can take a few seconds to sync), the dashboard shows a *Connecting to …* panel. If the connection fails, the real cause (for example an RBAC `forbidden` error) is shown in place of the worker table.

## Configuration

Configuration lives in your OS user config directory, created on first run (directory `0755`, file `0600` since it may hold tokens):

| OS | Path |
| --- | --- |
| Linux | `$XDG_CONFIG_HOME/vartop/config.json` (default `~/.config/vartop/config.json`) |
| macOS | `~/Library/Application Support/vartop/config.json` |
| Windows | `%AppData%\vartop\config.json` |

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
| `servers.<name>.interval` | Polling interval in seconds | `5` |

### Kubernetes

| Field | Description | Default |
| --- | --- | --- |
| `context` | kubeconfig context to use (kubeconfig is read from `$KUBECONFIG` or `~/.kube/config`) | *(required)* |
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

If the watcher can't sync within 10 seconds (wrong context, no access, API server unreachable), vartop shows the underlying error. Press <kbd>r</kbd> to retry.

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

If a server is configured with a single **load-balancer URL** (an ingress, nginx, a cloud LB, or a normal ClusterIP Service), every request reaches *one* instance chosen by the balancer:

- the dashboard shows only that instance's workers, and it can change on every refresh
- an action (pause, stop, …) reaches one random instance, not all of them; a `pause` and the following `resume` can land on different instances

vartop can't aggregate through a balancer, so instead it makes the situation visible. varmq (with this feature) adds an `X-Varmq-Instance` header naming the process that answered every API request, defaulting to the hostname (the pod name on Kubernetes) and settable via `varmq.InstanceID`. vartop uses it to:

- label each worker row, and each CLI result line (`ok (instance pod-c)`), with the instance that served it
- show a **Load-balanced endpoint** warning once one URL has been answered by more than one instance

Detection is best-effort: a balancer that keeps a client on one backend per connection (most L4 balancers, kube-proxy) may never reveal a second instance.

**To see every instance, skip the balancer:** use [`kubernetes`](#kubernetes) discovery, [`dns`](#dns) with a headless Service or service-discovery name, or list the instance URLs in `endpoints`. A cloud load balancer's own hostname resolves to the balancer's nodes, not your instances, so `dns` doesn't help there.

> **Distributed queues:** with a shared (e.g. Redis) queue, every instance reports the same queue length as *Pending*, so don't add Pending up across rows.

## Architecture

```
vartop
├── cmd/                    # Cobra CLI commands
│   ├── root.go             # Root command — launches the TUI
│   ├── server.go           # Server management subcommands
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
│   └── tui/
│       ├── model.go        # Root Bubble Tea model — shared state, update loop, screen stack
│       ├── screen.go       # Screen interface + push/pop navigation
│       ├── dashboard.go    # Worker table, status bar, offline/connecting states
│       ├── detail.go       # Single worker detail: config, metrics, scrolling
│       ├── switcher.go     # Server / pod switcher
│       ├── keys.go         # Per-screen key bindings
│       ├── help.go         # Help footer rendering
│       ├── handlers.go     # Async message handlers (health, workers, server switch, resize)
│       ├── commands.go     # Bubble Tea commands (tick, fetch, actions, pool build)
│       ├── styles.go       # Tokyo Night colour theme & table styles
│       └── types.go        # Message types, switcher items, helpers
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
