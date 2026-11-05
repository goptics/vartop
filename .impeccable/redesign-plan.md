# vartop web redesign: plan

Status: plan, not started. No code written. Product truth: `PRODUCT.md`.

## 1. Direction

- **World:** category standard, played straight: clean dark ops dashboard. Not a costume, not a template.
- **Craft bar:** Linear (calm dark ground, tight type scale, 1px low-contrast borders, quiet chrome, keyboard focus everywhere) + Temporal UI (dense execution lists, per-run timeline and event history).
- **Theme:** dark first; light built to same craft; follows OS (`prefers-color-scheme`), as now.
- **Anti-goals:** generic SaaS admin (soft shadows, gradient, card soup), terminal costume, sparse, busy.
- **Colour jobs:** neutral tinted greys for shell and surfaces; one accent for selection, focus, current nav; state colours (ok, warn, bad, paused) only where they carry meaning; chart series from the same set.
- **Type:** system UI stack (no font files, no build), `ui-monospace` only for IDs, payloads, errors, endpoints. Tabular numerals for every number. One type scale, ~6 steps.

## 2. Shell (every page)

- **Left side rail** (≥ 1024px): brand, nav (Dashboard, Instance grid, Queues), server picker, freshness ("6/6 instances · 2s · updated 16:13:23") at the rail foot. Collapses to a top bar with a menu button under 1024px.
- **Page header:** title, scope picker (instance: all merged / one), page actions on the right.
- **Banners** (stale, config changed, errors) in one slot under the header, same component everywhere.
- Keep every htmx hook: ids `page`, `live`, `poller`, `status`, `banner`, `banner-merged`, `bulk`, `errors`, `results`, `pick-server`, `pick-unit`; `.history`, `.tip`, `.hit`, `.cross` used by `detail.js`.

## 3. Pages

| Page | Lead | Redesign |
|---|---|---|
| Dashboard | Headline numbers, then workers | 4 stat tiles with sparkline + onset mark; worker table: sortable headers, status dot + label, busy/concurrency bar, pending, completed/s trend; sticky bulk bar on selection. Scales 1 → 100+ workers. |
| Instance grid | Workers × instances | Dense matrix, sticky worker column + instance header, cell = status dot + busy/conc + pending, load as fill; holds 100 instances (horizontal scroll inside the matrix only). |
| Worker page | Totals, charts, actions | Stat row (submitted, completed, successful, failed, success rate); two charts with segmented 1h / 24h / 7d / 30d; actions in a header group (Pause, Resume, Restart, Stop) + concurrency control; instances list as a table. |
| Queues | Queue list | Table: Queue ID (mono), per-state counts, totals, "reported by" chips; unsupported-instances note. |
| Job page | Jobs by state | Temporal-style: state tabs with counts (sticky), dense job list (rows done), open job = summary grid + to-scale timeline + attempts/errors history + payload; pager with range and page numbers. |
| Confirm dialogs | Action confirm | Native `<dialog>`, same component for every action; destructive action in bad colour. |
| Action results | Per-endpoint outcome | Result list: endpoint, outcome icon, message; toast summary. |
| States | Empty, loading, stale, error, waiting | One component each, reused on every page. |

## 4. Constraints

- Binding: one Go binary; `html/template` + vendored htmx + plain CSS/JS; no Node, no build step, no framework.
- Copy renders as templated: no `text-transform: uppercase` (`TestCSSLeavesCopyVerbatim`).
- Behaviour unchanged: routes, query params, htmx live refresh, action guards (stale data, other config), loopback, token server-side.
- a11y best effort; keep #7 fixes (page `h1`, labels, `aria-current`, `<time datetime>`, focusable scroll regions, phone tables).
- ~130 markup assertions in `internal/web/*_test.go` match HTML strings: update per page, never delete coverage.

## 5. Phases (one commit each, before/after shots per phase)

0. **Branch.** After goptics/vartop#6 merges: new branch off `main` (e.g. `redesign/web`), copy `PRODUCT.md` + this plan, write the direction contract (surface brief).
1. **Foundation.** Rewrite `app.css` tokens (colour dark + light, type scale, spacing, radius, borders, focus ring, motion), base elements (table, buttons, inputs, selects, segmented control, dialog, banner, badge/dot), shell in `layout.html`.
2. **Dashboard.**
3. **Instance grid.**
4. **Worker page + charts** (restyle SVG renderers in `render.go`, keep `detail.js` hooks).
5. **Queues + job page** (carry over today's job list, step icons, sticky tabs; add page numbers).
6. **Dialogs, results, banners, empty/stale/error states.**
7. **Finish.** Responsive pass (390, 768, 1024, 1440), keyboard + focus pass, reduced motion; impeccable finish review; DESIGN.md + `.impeccable/design.json` from the built world; new README screenshots (`docs/images/`).

Each phase: `go vet ./...`, `go test -race ./...`, before/after at 375 dark + 1440 dark + 1440 light against the fake fleet and the real varmq demo. One PR at the end with before/after per page (or one PR per phase if review size matters).

## 6. Open decisions

- One PR or one PR per phase.
- Sortable tables: client-side (small JS) or server-side (query param). Default: server-side, no new JS.
- Rail collapse under 1024px: top bar + `<details>` menu (no JS) by default.
