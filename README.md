<div align="center">

<h1><picture><source media="(prefers-color-scheme: dark)" srcset="docs/images/logo-dark.svg"><img alt="vartop" src="docs/images/logo-light.svg" height="72"></picture></h1>

<p><strong>A live dashboard and control panel for <a href="https://github.com/goptics/varmq">VarMQ</a> worker fleets.</strong></p>

<p>See every worker on every instance, chart its throughput from the last hour to the last 30 days,<br>
and pause, resume, restart, stop or resize it, from your browser or the command line.</p>

<p>
  <img src="https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat-square&logo=go" alt="Go 1.26+">
  <a href="#license"><img src="https://img.shields.io/badge/license-MIT-blue.svg?style=flat-square" alt="License: MIT"></a>
  <a href="https://github.com/goptics/varmq"><img src="https://img.shields.io/badge/for-varmq-44BF9E.svg?style=flat-square" alt="For VarMQ"></a>
</p>

<p>
  <a href="#quick-start"><strong>Quick start</strong></a> ·
  <a href="docs/tour.md"><strong>Tour</strong></a> ·
  <a href="docs/usage.md"><strong>Usage</strong></a> ·
  <a href="docs/configuration.md"><strong>Configuration</strong></a> ·
  <a href="docs/hub.md"><strong>Hub mode</strong></a> ·
  <a href="docs/how-it-works.md"><strong>How it works</strong></a>
</p>

</div>

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/dashboard-dark.png">
  <img alt="The vartop dashboard: completed/s, failed/min, pending and workers by status for a server of three instances, with a row and a 10-minute trend per worker" src="docs/images/dashboard-light.png">
</picture>

## What is vartop?

[VarMQ](https://github.com/goptics/varmq) is a typed worker pool and job queue for Go. Each process that uses it can serve its workers over a small REST API, `varmq.Handler`. **vartop** talks to that API on every instance of your service and puts the whole fleet in one place:

- **`vartop web`**, a dashboard in your browser. It merges every instance (or shows one), refreshes at each poll, and keeps the history in a local SQLite store. It lets you act on workers, queues, jobs and workflows.
- **`vartop action`** and **`vartop concurrency`**, the same actions from a terminal or a script, against every endpoint of a server at once: each static URL, or each address DNS discovery finds. A hub server's instances are reachable only through `vartop web`, so the CLI refuses hub servers.

There is no agent to deploy and no database to run: vartop is one binary on your machine. It finds your instances from a fixed list of URLs or the addresses a DNS name resolves to, or lets them connect to it ([hub mode](docs/hub.md)), and it collects only while `vartop web` runs.

## Features

**Watch**

- **Fleet dashboard.** Completed per second, failed per minute, pending jobs and workers by status, with a 10-minute trend per worker.
- **Instance grid.** Every worker on every instance at a glance: status, busy/concurrency, pending.
- **Worker page.** A graph of the queues and topics it pulls from, sorted by priority, with its strategy (VarMQ API 1.5.0). Totals and success rate. Two charts (completed /s, failed /s) with axes and a hover readout. Each chart has its own **1h | 24h** switch, plus **7d** and **30d** as far as the server's `history` reaches.
- **Queues.** One row per Queue ID: its name, backend icon and kind, its counts, and the workers that pull from it. A paused queue shows a **paused** badge. A queue's page lists its jobs, one tab per state.
- **Workflows** (VarMQ API 1.3.0). Every workflow, newest first, with its state, tasks ended and duration. A workflow that several instances report shows once. Its task graph is laid out left to right, with pan and zoom. Hover a task to trace what it waits for and what waits on it. Click it for its queue, job, reason, result size and undo job. A running workflow's page follows it live.
- **Job page** (VarMQ API 1.4.0). A job's state and a timeline of how long it waited and ran, with timers that tick while it does. Each failed attempt's error, its payload and its result. Linked from a queue's job list and a workflow's tasks.
- **History that survives restarts.** 24 hours of polls, and up to 30 days of each worker's rates per server, in a local SQLite store.
- **Stale data called out.** A banner says how old the data is and why, instead of showing old numbers as current.

**Act**

- **Pause, resume, restart, stop**, and a concurrency slider, on every instance or on one; pause or resume several workers at once.
- **Pause or resume a queue** (VarMQ API 1.2.0), after a confirm. vartop sends it to every instance that reports the queue and serves the pause route.
- **Cancel a workflow**, with or without its undo jobs, after a confirm.
- **Cancel a job** that waits or runs, after a confirm. Cooperative: a running job stops only if its handler watches its context.
- **The same from the CLI**, fanned out to every endpoint concurrently, one result line each.

**Find every instance**

- **Static endpoints or DNS**, DNS followed live as instances scale, restart or move.
- **Hub mode.** The instances connect to vartop instead, with the server's token: no inbound port on them, and no addresses to configure. See [Hub mode](docs/hub.md).
- **Load-balancer detection.** One URL answered by several instances is recognised, and each instance behind it is listed.

**Safe by default**

- **Loopback only.** The dashboard has no login, so it only listens on a loopback address (`127.0.0.1`, `localhost`, `::1`, …).
- **The token stays on the server.** vartop calls VarMQ on the page's behalf; the bearer token never reaches the browser.
- **Actions only on current data.** An action is refused, and nothing is sent, while the data is stale or was collected under another configuration.

## Requirements

**Each instance of your service serves VarMQ's REST API.** Mount `varmq.Handler` on any `http.ServeMux` (see [VarMQ's REST API](https://github.com/goptics/varmq#rest-api)), or connect the instance to vartop in [hub mode](docs/hub.md):

```go
mux := http.NewServeMux()
mux.Handle("/varmq/", varmq.Handler("/varmq"))
http.ListenAndServe(":8080", mux)
```

vartop then calls `http://<instance>:8080/varmq`. The API is unauthenticated by design: keep it on a trusted network, or put it behind a proxy that checks a bearer token and give vartop that `token` (sent as `Authorization: Bearer <token>`).

**A VarMQ that serves the pages you want:**

| VarMQ | What works |
| --- | --- |
| v1.10.0 (latest release) | Dashboard, instance grid, worker pages and charts, worker actions |
| v1.11.0 (not released yet; until then `github.com/goptics/varmq@main`) | Everything: queues, jobs, workflows, the sources graph and hub mode (REST API 1.6.0) |

Each page says which API version it needs. See [VarMQ API](docs/how-it-works.md#varmq-api) for the routes and what each version adds.

**Network reach.** With static endpoints or [DNS](docs/configuration.md#dns), vartop dials each instance directly: run it inside the network, or over a VPN or route that reaches their addresses. In hub mode, the instances dial vartop instead.

**Go 1.26+** to install vartop.

## Quick start

1. **Install**

   ```bash
   go install github.com/goptics/vartop@latest
   ```

   Or build from source: `git clone https://github.com/goptics/vartop.git && cd vartop && make build` (binary at `bin/vartop`).

2. **Open the dashboard.** On first run a short wizard sets up your first server: a name, then a **static endpoint**, **DNS** discovery (host and port) or **hub** mode, and a token (optional, except in hub mode).

   ```bash
   vartop web
   ```

   It opens http://127.0.0.1:7070 and collects every configured server until you stop it with <kbd>Ctrl+C</kbd>.

3. **Add more servers**, from another terminal; a running `vartop web` picks them up within a few seconds:

   ```bash
   vartop server add                      # the same wizard
   vartop server set prod endpoint "http://pod-0:8080/varmq,http://pod-1:8080/varmq"
   vartop server set prod history 30d     # 7d and 30d charts for prod
   vartop server use prod                 # the default server
   ```

4. **Act from the terminal**:

   ```bash
   vartop action orders pause             # every endpoint of the default server
   vartop -s staging concurrency orders 8 # 8 per instance on staging
   ```

## Documentation

| Page | What it covers |
| --- | --- |
| [Tour](docs/tour.md) | Every page of the dashboard, with screenshots |
| [Usage](docs/usage.md) | `vartop web` flags, CLI commands, URL parameters |
| [Configuration](docs/configuration.md) | Config file, schema, DNS discovery, environment |
| [Hub mode](docs/hub.md) | Instances that dial vartop: setup, VarMQ options, limits, security |
| [How it works](docs/how-it-works.md) | Collection, charts, safety rules, load balancers, storage, disk use, VarMQ API |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report security problems as [SECURITY.md](SECURITY.md) says, not in a public issue. Changes are listed in [CHANGELOG.md](CHANGELOG.md).

## License

MIT © [Fahim Faisaal](https://github.com/fahimfaisaal). See [LICENSE](LICENSE).

## Acknowledgements

vartop's onboarding wizard is built on [Charmbracelet](https://charm.sh)'s Huh and Lip Gloss, and the dashboard on [htmx](https://htmx.org).
