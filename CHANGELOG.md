# Changelog

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions: [Semantic Versioning](https://semver.org/).

## [Unreleased]

First release. Full features need VarMQ v1.11.0 (REST API 1.6.0, hub spec 1.0.0); VarMQ v1.10.0 gets dashboard, grid, worker pages and worker actions.

### Added

- **`vartop web`**: loopback-only dashboard, collects every configured server while it runs. History in local SQLite store: 24 h of polls, each worker's rates in 5-minute buckets for the server's `history` (`1d`–`30d`).
- **Dashboard**: completed/s, failed/min, pending, workers by status, 10-minute trend per worker; bulk pause/resume.
- **Instance grid**: every worker on every instance.
- **Worker page**: totals, success rate, completed/s and failed/s charts with 1h, 24h, 7d, 30d spans, hover readout; sources graph with strategy and priorities (#15); pause, resume, restart, stop, concurrency slider.
- **Queues**: one row per Queue ID, backend and kind, counts, workers; job list per state; pause/resume (#10, #12).
- **Job page**: state, wait/run timeline with live timers, attempt errors, payload, result; cancel (#14).
- **Workflows**: list with state tabs and counts, task DAG with pan/zoom and dependency trace, cancel with or without undo (#11, #18).
- **CLI**: `vartop action`, `vartop concurrency` fan out to every endpoint; `vartop server add|list|use|remove|set`.
- **Discovery**: static endpoints, DNS (re-resolved every 5 s), load-balancer detection.
- **Hub mode**: VarMQ instances dial `vartop web --hub-listen` with the server's token; no inbound port, no addresses (#20, #21).
- **Safety**: stale-data banner; actions refused on stale data or another configuration; token never sent to browser.
- **Docs**: README as landing page; reference in `docs/`; `CONTRIBUTING.md`, `SECURITY.md`, this changelog.

### Removed

- Kubernetes discovery (#9): use `endpoints`, `dns` or `hub`. A config with a `kubernetes` block fails to load.
