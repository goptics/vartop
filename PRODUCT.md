# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Go developers and operators who run services built on VarMQ (typed worker pool + job queue). One user, one machine: vartop runs on their laptop or a box inside the network, never as a shared deployment. Three moments, equal weight:

- **Incident triage:** backlog grows or jobs fail; find which worker, instance or queue, then pause, resume, restart, stop or resize it.
- **Daily monitoring:** page left open on a second screen; glanced at for throughput, backlog and failure trends.
- **Dev / debugging:** while building a varmq service locally; watch workers, queues, job payloads and errors as code changes.

## Product Purpose

Live dashboard and control panel for varmq worker fleets. Talks to varmq's REST API (`varmq.Handler`) on every instance of a service, or, in hub mode, through the stream each instance opens to vartop, and shows the whole fleet in one place: workers, instances, queues and jobs, with history from the last hour to 30 days. Success: the user sees the fleet's real state at a glance and acts on the right worker without leaving the page.

## Positioning

- No agent, no database to run: one binary on the user's machine; collects only while `vartop web` runs.
- Finds instances from static URLs or DNS, followed live, or lets them connect to it (hub mode: no inbound port, no addresses); detects one URL answered by several instances behind a load balancer.
- Merges every instance, or shows one; queue data grouped by Queue ID, never summed across instances.
- Names queues as people do: a redisq, sqliteq or in-memory Queue ID is shown as its name, kind and namespace (an in-memory queue by its worker, from varmq API 1.4.0 `queue_ids`); other storage's IDs as sent. The full ID is one hover or copy away.

## Operating Context

- `vartop web` serves the dashboard on a loopback address; browser on the same machine.
- `vartop action` / `vartop concurrency`: same actions from a terminal, fanned out to every endpoint.
- Config: servers with endpoints or discovery; switch server in the page.
- History in a local SQLite store (24h of polls, up to 30 days of per-worker rates).

## Capabilities and Constraints

- Pages: Dashboard (fleet totals + per-worker rows with 10-min trend), Instance grid (every worker on every instance), Worker page (its sources fanning into it in priority order, with store glyphs, priorities and the pick strategy (varmq API 1.5.0; `queue_ids` before), totals, success rate, completed/failed charts with 1h | 24h | 7d | 30d), Queues list (by name: store icon, FIFO/Priority, namespace; paused badge; the workers consuming each), Queue job page (full ID with copy, workers on the queue, jobs by state incl. cancelled, paged, payload, per-attempt errors; queue pause/resume), One-job page (varmq API 1.4.0: state, timeline with waited / ran / total timers, live while it waits or runs; per-attempt errors, payload, result; cancel; linked from queue lists and workflow tasks), Workflows list (varmq API 1.3.0: merged across instances, one row per workflow, state tabs, progress, duration), Workflow page (task DAG laid out server-side as SVG: pan/zoom, upstream/downstream trace, task panel, task table; live while it runs; cancel). Confirm dialogs and action results.
- Actions: pause, resume, restart, stop, concurrency; bulk pause/resume; queue pause/resume (varmq API 1.2.0+, storage-wide: every process reading the queue); workflow cancel, with or without undo jobs (API 1.3.0, stored with the workflow: every process working on it); job cancel (API 1.4.0, cooperative: sent to the first instance that has the job). No job is retried, removed or purged.
- **Stack (binding):** one Go binary. Server-rendered `html/template` + vendored htmx + plain CSS/JS, embedded with `go:embed`. No Node toolchain, no build step, no front-end framework.
- Safety behavior (product function, not styling): loopback only; bearer token never reaches the browser; actions refused and nothing sent while data is stale or from another configuration; stale data called out with its age and reason.
- Pages refresh live at each poll via htmx (job page does not, so an open job stays open; a workflow page polls only while the workflow runs).
- One job has a permalink (`/queues/{id}/jobs/{jobId}`), but for IDs `.`, `..` and none: a browser cannot address them. An in-memory queue keeps no ended job unless it uses `WithEndedJobs`: its page says so.

## Brand Commitments

- Name: vartop (lowercase). Companion to varmq.
- No color, font or logo commitment confirmed; current look is replaceable.
- Standing visual preference (chosen 2026-11-05 over three dealt directions): the category standard, a clean dark ops dashboard, played straight. Craft bar: Linear (calm dark UI, tight type, subtle borders, keyboard-friendly) and Temporal UI (job/execution lists, per-run timeline and event history).
- Dark first; light theme built to the same craft; follows the OS setting.

## Evidence on Hand

- README screenshots: `docs/images/` (dashboard light/dark etc.).
- Real fleet available locally for checks: fake varmq fixtures (`internal/fleet/fleettest`) and a real varmq + Redis demo.
- No users, testimonials or metrics to cite.

## Product Principles

1. Truth over polish: never show old or merged data as if current and exact; say how old and why.
2. Find, then act: every view leads to the worker, instance or queue at fault and the action on it.
3. One fleet, many instances: merge by default, one instance on demand; never double count.
4. Zero setup: nothing to deploy, build or sign in to.

## Accessibility & Inclusion

Best effort, no formal standard. Keep what goptics/vartop#7 fixed: page `h1`, labelled controls, `aria-current`, `<time datetime>`, focusable scroll regions, tables that stack on phones.
