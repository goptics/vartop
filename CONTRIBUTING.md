# Contributing to vartop

Contributions are welcome: bug reports, fixes, docs and features.

## Report a bug or ask for a feature

Open an [issue](https://github.com/goptics/vartop/issues). For a bug, give the vartop commit or version, the VarMQ version, how vartop finds the instances (static, DNS or hub) and the steps that show the problem.

Do not report a security problem in a public issue. See [SECURITY.md](SECURITY.md).

## Development

You need Go 1.26 or later. vartop has no cgo and no front-end build step: the pages are Go templates with vendored htmx.

| Command | Description |
| --- | --- |
| `make build` | Compile the binary to `bin/vartop` |
| `make run` | Build and run `vartop web` |
| `make test` | Run all tests |
| `make clean` | Remove build artifacts |

To see the pages without a real VarMQ service, the tests use fake VarMQ servers, load balancers and workflows in `internal/fleet/fleettest`.

## Pull requests

1. Fork the repository.
2. Create a branch from `main` (`git checkout -b feat/my-change`).
3. Make your change, with tests for new behavior.
4. Run `make test`.
5. Commit with [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, …).
6. Open a pull request against `main`.

Keep the code idiomatic Go. Keep the dashboard usable from phone to desktop widths. For a change that you can see on a page, put before and after screenshots in the pull request. For a change that users notice, add a line under `Unreleased` in [CHANGELOG.md](CHANGELOG.md), and update the [docs](docs/) when the behavior changes.

## Project layout

```
vartop
├── cmd/                    # Cobra CLI commands
│   ├── root.go             # Root command, onboarding
│   ├── server.go           # Server management subcommands
│   ├── web.go              # `vartop web`: serves the dashboard and runs the collector while it does
│   └── workers.go          # Worker action & concurrency subcommands (fan out to all instances)
├── internal/
│   ├── api/
│   │   ├── client.go       # REST client for one VarMQ mux endpoint (+ instance header tracking)
│   │   ├── queues.go       # Queue, job, queue pause and job cancel routes
│   │   ├── workflows.go    # Workflow list, read and cancel routes
│   │   └── pool.go         # One client per discovered instance, reconciled from a discovery source
│   ├── config/
│   │   ├── config.go       # Config load/save + validation (<config dir>/vartop/config.json)
│   │   └── prompt.go       # Interactive onboarding / server add wizard
│   ├── hub/
│   │   ├── hub.go          # One hub server's connected instances: streams, heartbeats, replies
│   │   └── router.go       # Hub listener: picks the hub server by token, connection limits
│   ├── discovery/
│   │   ├── discovery.go    # Source interface: live set of ready endpoint URLs
│   │   ├── static.go       # Fixed endpoint list from config
│   │   └── dns.go          # Re-resolves a hostname to every instance address
│   ├── fleet/              # Model of a VarMQ fleet, no HTTP
│   │   ├── poller.go       # Collector: polls every unit of a server into snapshots
│   │   ├── history.go      # Snapshot types, rate calculator, 1 h ring
│   │   ├── day.go          # 5-minute buckets, merged into the 24h, 7d and 30d charts
│   │   ├── balancer.go     # Reaches every instance behind a load balancer
│   │   ├── view.go         # Snapshots → dashboard, grid and detail view models
│   │   ├── act.go          # Worker action core, and the dashboard's notices
│   │   ├── queues.go       # Queues across instances, queue pause, one job and its cancel
│   │   ├── workflows.go    # Workflows merged across instances, and their cancel
│   │   ├── noun.go         # instance wording and the URL parameter
│   │   └── fleettest/      # Fake VarMQ servers, load balancers and workflows for tests and screenshots
│   ├── store/              # SQLite store the collector writes and the pages read
│   │   ├── dir.go          # Data dir, file names, pragmas, retention and size cap
│   │   ├── control.go      # control.db: the collector's lease, pokes, server errors
│   │   ├── server.go       # One file per server: snapshots, labels, pruning
│   │   ├── buckets.go      # Each worker's 5-minute buckets, kept for the server's history
│   │   ├── codec.go        # Snapshot ↔ compressed record
│   │   └── feed.go         # Reader: loads the 1 h window, then tails new rows
│   ├── collect/
│   │   ├── collect.go      # Run: lease, config watcher, pokes, pruning
│   │   ├── collector.go    # One per server: poll → writer → the server's file
│   │   └── config.go       # Watches the config file for changes
│   └── web/
│       ├── server.go       # Loopback listener, Host/CSRF guards, page handlers
│       ├── actions.go      # Worker action, concurrency and bulk POST routes
│       ├── day.go          # The charts' span switch and their longer spans
│       ├── queues.go       # Queue list, queue page and queue pause (read live)
│       ├── job.go          # Job page and job cancel (read live)
│       ├── workflows.go    # Workflow list, workflow page and workflow cancel (read live)
│       ├── dag.go          # Lays out a workflow's task graph as SVG
│       ├── sources.go      # A worker's sources graph
│       ├── refresh.go      # Quiet poll: skips regions the browser already shows
│       ├── render.go       # Template loading, page model, chart axes
│       ├── templates/      # HTML pages (htmx-driven, server-rendered)
│       └── static/         # Embedded CSS, page scripts (chart hover, job timers, queue page, workflow graph) and vendored htmx
├── docs/                   # User docs and their screenshots (docs/images)
├── main.go                 # Entry point
├── Makefile                # Build, run, clean, test targets
├── go.mod
└── go.sum
```

