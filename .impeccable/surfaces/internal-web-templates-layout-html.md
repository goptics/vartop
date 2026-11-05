---
version: 1
slug: "internal-web-templates-layout-html"
primary_target: "internal/web/templates/layout.html"
related_targets: ["internal/web/static/app.css"]
---

# vartop web UI

Scope: every page of `vartop web` (layout shell, dashboard, instance grid, worker page, queues, job page, dialogs, results, banners, states). Mode: Operate.

Audience and job: Go developers/operators running varmq services; incident triage, daily monitoring, dev/debugging, equal weight. Find the worker, instance or queue at fault, then act. Fleets from 1 worker on 1 instance to Kubernetes scale (50+ workers, 100+ instances).

Constraints: one Go binary, html/template + vendored htmx + plain CSS/JS, no build. Copy renders as templated (no uppercase transform). htmx ids and detail.js hooks unchanged. Plan: `.impeccable/redesign-plan.md`.

## Direction contract

THESIS: vartop is the category standard played straight, a calm dark ops console at Linear and Temporal UI craft; it refuses card soup, gradients, soft shadows and the terminal costume. Precision, density and quiet chrome carry the brand.

OWN-WORLD: neutral cool greys, dark ground first and an equal light theme; 1px low-contrast borders, small radii, no shadows except dialogs; one accent for selection, focus and current nav; ok, warn, bad, paused colours only on state; system UI type on a six-step scale, tabular numerals, mono only for IDs, payloads, errors, endpoints.

STORY: the user opens vartop, reads fleet health in one glance (rates, failures with onset, backlog), sees which worker or instance is off, opens it, acts (pause, resume, restart, stop, resize) and sees the outcome per instance.

FIRST VIEWPORT: left rail (brand, Dashboard, Instance grid, Queues, server picker, freshness at foot); page header with title, instance scope picker; four stat tiles with sparklines, failed/min marking its onset; dense sortable worker table below, bulk bar appearing on selection.

FORM: category standard (canon card), chosen by the user over the dealt hand; craft bar Linear + Temporal UI. seed:ec3c55f78ed31bb2

FINISH: unreviewed and undocumented is unfinished; this build ends with the finish review, the verdict, DESIGN.md, and every shipping raster carrying its provenance
