---
name: vartop
description: Calm dark ops console for varmq fleets; dark first, light equal, chosen by prefers-color-scheme.
colors:
  bg: "#0D0F14"
  panel: "#14171E"
  raised: "#1A1E27"
  line: "#232833"
  line-strong: "#2E3441"
  fg: "#E3E6ED"
  subtle: "#969DAE"
  accent: "#44BF9E"
  on-accent: "#062019"
  ok: "#5CC48A"
  warn: "#E8A94E"
  bad: "#F2707F"
  paused: "#8E9BFA"
  bg-light: "#F5F6F8"
  panel-light: "#FFFFFF"
  raised-light: "#EEF0F4"
  line-light: "#E2E5EA"
  line-strong-light: "#CDD2DB"
  fg-light: "#171A21"
  subtle-light: "#5B6272"
  accent-light: "#0C7760"
  on-accent-light: "#FFFFFF"
  ok-light: "#19733F"
  warn-light: "#8F5B00"
  bad-light: "#BE3446"
  paused-light: "#4655CF"
typography:
  display:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", Roboto, sans-serif"
    fontSize: "28px"
    fontWeight: 600
    lineHeight: 1.15
    letterSpacing: "-0.02em"
    fontFeature: "\"tnum\""
  headline:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", Roboto, sans-serif"
    fontSize: "20px"
    fontWeight: 600
    letterSpacing: "-0.01em"
  title:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", Roboto, sans-serif"
    fontSize: "16px"
    fontWeight: 600
  body:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", Roboto, sans-serif"
    fontSize: "14px"
    fontWeight: 400
    lineHeight: 1.5
    fontFeature: "\"tnum\""
  label:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", Roboto, sans-serif"
    fontSize: "13px"
    fontWeight: 500
  caption:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", Roboto, sans-serif"
    fontSize: "12px"
    fontWeight: 500
  mono:
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace"
    fontWeight: 400
rounded:
  r-1: "4px"
  r-2: "6px"
  r-3: "8px"
  pill: "999px"
  dot: "50%"
spacing:
  2xs: "4px"
  xs: "8px"
  pad-tight: "10px"
  sm: "12px"
  pad-panel: "14px"
  md: "16px"
  lg: "24px"
  xl: "32px"
  rail: "232px"
components:
  button:
    backgroundColor: "{colors.raised}"
    textColor: "{colors.fg}"
    rounded: "{rounded.r-2}"
    padding: "5px 10px"
    typography: "{typography.label}"
  button-hover:
    backgroundColor: "{colors.line}"
  button-primary:
    backgroundColor: "{colors.accent}"
    textColor: "{colors.on-accent}"
    rounded: "{rounded.r-2}"
    padding: "5px 10px"
  button-warn:
    backgroundColor: "{colors.raised}"
    textColor: "{colors.warn}"
    rounded: "{rounded.r-2}"
  button-danger:
    backgroundColor: "{colors.raised}"
    textColor: "{colors.bad}"
    rounded: "{rounded.r-2}"
  nav-link:
    textColor: "{colors.subtle}"
    rounded: "{rounded.r-2}"
    padding: "6px 10px"
  nav-link-current:
    backgroundColor: "{colors.raised}"
    textColor: "{colors.fg}"
  chip:
    backgroundColor: "{colors.raised}"
    textColor: "{colors.fg}"
    rounded: "{rounded.pill}"
    padding: "1px 8px"
    typography: "{typography.caption}"
  badge:
    textColor: "{colors.warn}"
    rounded: "{rounded.pill}"
    padding: "0 7px"
    typography: "{typography.caption}"
  seg-current:
    backgroundColor: "{colors.panel}"
    textColor: "{colors.fg}"
    rounded: "{rounded.r-1}"
    padding: "2px 10px"
  tab-current:
    backgroundColor: "{colors.raised}"
    textColor: "{colors.fg}"
    padding: "10px 14px"
  panel:
    backgroundColor: "{colors.panel}"
    rounded: "{rounded.r-3}"
    padding: "14px"
---

# Design System: vartop

## Overview

**Creative North Star: "The category standard, played straight"**

Calm dark ops console, Linear / Temporal UI craft bar. Precision, density, quiet chrome carry the brand. Neutral cool greys, 1px low-contrast borders, small radii, flat surfaces. One accent (varmq teal). State colours only where state or action severity is meant. System UI type on a six-step scale, tabular numerals everywhere.

Dark ground first; light theme equal, not an afterthought; picked by `prefers-color-scheme`, no toggle. Every text token holds >= 4.5:1 on `bg`, `panel`, `raised` in both themes. Refuses card soup, gradients, soft shadows, terminal costume.

Stack constraint shapes the system: one Go binary, `html/template` + vendored htmx + plain CSS/JS, no build. Copy renders exactly as templated.

**Key Characteristics:**
- Rail shell (232px) on wide screens, top bar under 1024px.
- One panel per group, split by hairlines, not tiles floating on the ground.
- Status = coloured dot + foreground label; hollow dot for uncertain states.
- Flat: no drop shadows anywhere; inset box-shadow only as a stroke.
- SVG stroke icons (1.5 stroke, round caps), no glyph icons.
- Mono only for IDs, payloads, errors.

**Imagery and raster provenance.** No generated imagery; the UI has none. `docs/images/*.png` (README and docs shots, dark + light) are headless Chrome captures over CDP: 1280x560 viewport, DPR 2, full page, `prefers-color-scheme` emulated; `chart-hover-*` hovers the first chart at 42% width and clips to it. Subject: `vartop web` (interval 2s, history 7d) on server "demo-cluster": three `fleettest` fake VarMQ instances (orders-api-6f9c-*) behind a round-robin balancer at http://varmq.localhost:8080/varmq. A throwaway Go test seeded 8 days of one-a-minute polls (work-hours hump, quieter weekend, a 40-minute thumbnails dip two days back), then advanced the fakes' counters live; workers billing-sync, emails, reports (paused on instance-2), search-index, thumbnails, webhooks; queues and jobs from `fleettest.SampleJobs`, workflows from `fleettest.SampleWorkflows`. Job and workflow shots taken in the first minute, the rest after 10 minutes of live polls. Captured 2026-11-14.

## Colors

Cool neutral greys, one teal accent, four state hues. Light values are the `-light` keys; they replace the dark ones under `@media (prefers-color-scheme: light)`.

### Primary
- **varmq Teal** (`accent`): selection, focus ring, selected graph task (2px edge, 8% fill), traced edges on graph hover/focus, links, current-nav icon, brand mark, completed series in charts and sparklines, waiting dot, current state-tab underline, job-row hover/open tint (7%), selected-row tint (8%), `::selection` (35%), native `accent-color` (slider, checkboxes), primary button fill.
- **Teal Ink** (`on-accent`): text on accent fill only.

### State
- **Ok Green** (`ok`): active status, healthy count, ok result, finished step, completed tab.
- **Amber** (`warn`): processing tab, started step, running workflow and in-flight task (pending/processing) with its edge, pulse ring and flowing edge, compensating workflow (hollow), no-change result, absent badge, stale freshness, banners, confirm-panel edge, Restart button text.
- **Coral Red** (`bad`): stopped status, failed series, failed and compensation-failed workflow, failed task node (cross, 7% fill, edge), failed undo badge, rollback-failed summary (7% fill, 30% edge), cancelled workflow and task (hollow), hot stat tile, errors, failed tab and step, down grid column tint (6%), Stop button text.
- **Periwinkle** (`paused`): paused status only (worker status, queue paused badge).

### Neutral
- **Ground** (`bg`): page, payload well, chart tooltip.
- **Panel** (`panel`): every container surface.
- **Raised** (`raised`): table head, default button, current nav/tab, chip, bulk bar, notes.
- **Hairline** (`line`): borders, dividers, gridlines, load-bar track.
- **Strong Hairline** (`line-strong`): control borders, empty-state dash, bulk bar edge, scrollbar.
- **Ink** (`fg`) / **Muted Ink** (`subtle`): text; `subtle` also unknown/mixed/transitional status, crosshair, load-bar fill, graph edges, node box edge (`--mark`, 70% into `bg`), waiting task (hollow dot, dashed box), compensated workflow (ended, rolled back: neutral filled dot).

### Named Rules
**The One Accent Rule.** Teal means "selected, focused, current, or completed". Never a second meaning; failed series use `bad`, grid load fill uses `subtle`, so teal never sits beside ok green as a peer.

**The State Colour Rule.** `ok`, `warn`, `bad`, `paused` appear only on state or action severity (Restart in warn, Stop in bad, caution banners in warn). Never decorative. Tints are `color-mix` into `panel` or transparent, 6-18%.

## Typography

**Display Font:** system-ui (with -apple-system, "Segoe UI", Roboto, sans-serif)
**Body Font:** same stack
**Label/Mono Font:** ui-monospace (with SFMono-Regular, Menlo, Consolas, monospace)

**Character:** native UI sans, no webfont; the world's own choice (no build, one binary), not a craft-floor default. Weight and size carry hierarchy; mono marks machine strings.

### Hierarchy
Six-step scale: 12 / 13 / 14 / 16 / 20 / 28px (`--fs-1`..`--fs-6`).
- **Display** (600, 28px, 1.15, -.02em): stat numbers (`.big`); 20px under 640px.
- **Headline** (600, 20px, -.01em): page `h1`, queue name (sans), workflow ID (mono), state-tab counts.
- **Title** (600, 16px): confirm heading, waiting line (400), brand (650).
- **Body** (400, 14px, 1.5): default text, table rows, chips list.
- **Label** (500, 13px): tile headings, `.sub` scope lines, controls, figcaptions, pager.
- **Caption** (500, 12px): table heads, chart axes, tooltip, notes, badge, chip, job meta.
- **Mono** (size follows its context, 12-20px): full Queue IDs, a custom storage's queue name, job IDs, previews, payload, attempt errors.

### Named Rules
**The Tabular Rule.** `font-variant-numeric: tabular-nums` on `body`; numeric columns right-aligned (`.num`).

**The As-Templated Rule.** No uppercase. `text-transform: uppercase` is banned by a Go test (`render_test.go`). `capitalize` is allowed and used (state tab labels, list head state word).

**The Mono-for-Machines Rule.** Mono only for IDs, payloads, errors, endpoints. Instance addresses currently render in sans; that is allowed, not required to change.

## Layout

- **Shell:** under 1024px, rail is a wrapping top bar (`panel`, bottom hairline, 10px 16px). From 1024px: `body` grid `232px minmax(0,1fr)`; rail sticky, full height, column, right hairline, freshness pinned to its foot above a hairline.
- **Main column:** 16px 16px 32px; from 1024px 24px 32px 40px. `main` max-width 1280px.
- **Rhythm (observed, not declared as tokens):** 4 / 8 / 12 / 16 / 24 / 32; inner padding 10 and 14 (cards `12px 14px`, panels `14px`, rows `9px 14px`). Block gap below groups: 16px.
- **Breakpoints:**
  - **480px (max):** state tabs tighten (6px 8px, label 12px, count 16px); job body drops its 34px indent.
  - **640px (max):** rail one row, nav scrolls sideways, nav icons hidden; pickers side by side (1fr / 1.6fr), labels stacked; status line full width, 12px; stat tiles 10px 12px, numbers 20px; worker and queue tables stack per row with `data-label` captions.
  - **760px (min):** job body two columns (1fr / 1.3fr): times + errors | payload.
  - **1024px (min):** rail becomes left column (see Shell).
  - **1200px (min):** worker page splits into main + 300px side column (controls, results), side sticky at 24px.
- **Touch (`pointer: coarse`, any width):** buttons, selects, number box, slider, nav links, crumbs, segments, pager, sort headers, row name links, job rows and IDs, source nodes, worker chips: min-height 44px; row checkbox 20px inside a 44px `label.hit`. Mouse density unchanged.
- **Overflow:** tables scroll inside their wrapper, never the page (0px page overflow at 375px on dashboard, grid, worker, queues, queue, job pages). Instance grid capped at `100vh - 140px`, sticky header row and worker column.

## Elevation & Depth

Flat. No drop or offset shadows anywhere. No modal: every confirm is an in-page panel. Depth is tonal: `bg` < `panel` < `raised`, separated by 1px hairlines. Sticky elements (rail, state tabs, bulk bar, grid headers) rely on opaque fills, not shadows.

Inset box-shadow appears only as a stroke, four places: hollow status dot (`inset 0 0 0 1.5px`; also the cancelled job-state dot and timeline icon, in `bad`), current range segment ring (`inset 0 0 0 1px line-strong`), current state-tab underline (`inset 0 -2px 0 accent`).

### Named Rules
**The Flat Rule.** A surface is lifted by a step in tone and a hairline, never by a blur.

## Shapes

Small, consistent corners: 4px (focus ring, range segment, tooltip), 6px (controls, nav links, pager, attempt errors, payload), 8px (every panel: stat strip, tables, charts, controls, bulk bar, confirm, results, job list, banners, empty state). Pills (999px) for chip and badge; circles for status dots (7px; 8px in grid cells) and timeline icons (20px). Borders always 1px; dashed only for the empty state, the unreached timeline step, and the job-body top divider.

## Components

### Rail / shell
Brand (20px three-line SVG mark in accent, wordmark 16px/650). Nav: Dashboard, Instance grid, Queues, Workflows (three linked nodes), each with a 16px stroke icon; Workflows always shown (a note on the page when no instance serves the routes, not a probe per page); links `subtle`, hover and current `raised` + `fg`, current icon teal. Scope: server and instance pickers together (`#pick-server`, `#pick-unit`). `#status`: health with 7px dot (ok / warn / bad by healthy share), refresh interval, "updated" (warn when stale), optional `.note`.
- **Live poll:** `#poller` sends back `X-Seen` (hash of `#live` - hash of `#banner`+`#status`, `app.js`). Same pair: 204, no swap. Same `#live`: `#banner` + `#status` only, `HX-Reswap: none` (focus, selection, scroll in `#live` kept). Else the page. Text bodies >= 1 KB gzipped (`Vary: Accept-Encoding`). Static files: weak content `ETag` + `no-cache` (304 on revalidate; a new binary never serves old CSS/JS). `favicon.svg`: the teal three-line mark. On a real swap, text selection inside `#live` is lost; focus survives only on elements with stable ids.

### Page header
`h1` + `.sub` scope line on one baseline, wrapping. Worker, queue and job pages add breadcrumbs (`/` separator, 13px, `subtle`).

### Stat strip
One `panel` (8px, hairline) holding auto-fit tiles (min 150px) split by hairlines (each tile draws left+top edge pulled 1px under the frame). Tile: label heading, display number, sparkline at foot. `.hot` (failed > 0): number and onset line in `bad`. Onset line: "since 15:04" under the failed number. Status tile lists counts by status, the same lower-case words as the rows ("3 running", "1 paused"), each with its dot; Pending tile: "Pending", `.sub` under the number "each worker's max per instance, summed" (was "max/instance": a sum of maxima). Worker page counters use the same strip without sparklines: Completed, Successful, Failed, Success rate (no Submitted: varmq API 1.1.0 dropped it; a queue's is its `totals.submitted`).

### Worker sources
Under the worker's counters, `h2.section` "Sources" ("N, in the worker's order; the strategy picks among them") + a fan (`.fan`, `bg` well, hairline, 8px; no JS): left, one node per source in the worker's order (the list the strategy picks from) (`.srcn`: `panel`, `line-strong` 1px, 8px, as a DAG task box): `qname` (glyph, name cut in the middle past 28 chars, kind badge) + priority chip "p N" (`.prio`, 12px `subtle` pill) only under the `priority` strategy and when set + live state, 12px: "N pending" / "empty" in `subtle`, or the paused status dot + "paused" (queues read live as `/queues` does; none shown when the read fails); a link to the queue page, `aria-label` = full name, kind, priority, state. The `ol` (labelled "Sources of <worker>, in the worker's order") is the text alternative: the bus and arrow are `aria-hidden` CSS. Topic: broadcast glyph + "topic"; storage with no Queue ID: italic `subtle` "custom storage"; neither links. Edges (1.5px `subtle` 85%: >= 3:1 on `bg` in both themes) join one bus, then a 7px arrow into the worker node (`.wnode`: `panel`, 1.5px `accent`, status dot + name 600, strategy in 12px `subtle`: "Priority: lowest number first", "Round robin: each source in turn", "Max length: longest queue first", "Min length: shortest non-empty first"; "Pulls directly" with one source). Under 640px the bus runs down the left, the worker below. Instances merge by kind + Queue ID (first instance's order, then what others add; first instance's strategy). Older VarMQ (no `sources`): nodes from `queue_ids`, no label, a `subtle` note. Hidden when stale.
- **Gap:** tile sparklines are fixed 160x32 (row trends 120x24); they do not stretch with the tile.

### Queue name
`qname` partial (`layout.html`), the one way a Queue ID is shown: 14px store glyph (`.sico`, `subtle`) + visually hidden store word ("Redis: ", "SQLite: ", "In memory: ") + name + kind badge; `title` = full Queue ID. Parsed in Go (`parseQueueID`): redisq `redisq:[ns:]fifo|prio:{key}` -> key, kind, namespace; sqliteq `sqliteq:<h>:fifo|prio:<name>` -> name, kind (hash only in the title); in-memory `memory:<instance>[:fifo|prio]:<n>` -> "<worker> #n" from the workers' `queue_ids`, else "in-memory #n", then "on <instance>" (`.qinst`, 12px `subtle`, after the kind badge: one queue per process, so three instances' "orders #1" differ); anything else -> the full ID in mono, no glyph, no badge. Links (`a.ql`) keep `fg`, the name turns accent + underline on hover. URLs always carry the full ID.
- **Store glyphs:** filled, not stroked (the one exception to `.ico`): Redis and SQLite are Simple Icons (CC0), the memory chip is the varmq docs site's; path data copied from the varmq docs (`Storage.astro`, `heromap/render.ts`).
- **DAG node:** the name, cut at 12 chars, after the state.

### Tables
Inside `.table-wrap` panel. Head `raised`, 12px/500 `subtle`; cells 8px 12px, hairline rows, no zebra. Row hover `raised` 60%; selected row accent 8%. Names `fg`/500, accent + underline on hover. Queue list: the page `.sub` says what the numbers are (Pending to Failed held now, retention may drop old ended ones; "Submitted (all time)"); "Failed" stays `bad` when non-zero (no rate to tell recent from old). "Workers" column: a chip per worker whose `queue_ids` hold the queue (link to its page, title "runs on <instances>"); none known: a 12px `subtle` "instances" then the instance chips. Under 640px worker and queue rows stack into a labelled grid (head moved off-screen, `data-label` captions, status on its own line).
- **Dashboard order:** default "Trouble first": failing now (Failed /min > 0), then paused or stopped with pending, then the rest; inside a bucket by its measure, name breaks ties. `?sort=` name | pending | failed: header links (`aria-sort` on the active one, 4px caret) and a `caption` `.seg` switch ("Order"); poll URL, pickers and action refresh keep it. "Failed /min" column: mean over the last minute, `bad` 600 when > 0. Grid rows follow the same order. Row ids stable, checkboxes `hx-preserve`.

### Status dot
7px dot (`--c`) + label in `fg`, always both. Worker label: varmq's status in lower case ("running", "idle", "paused"; raw per instance in `title`), "active" / "transitional" when instances differ inside one class, "mixed" across classes; same words on dashboard, grid, worker page, queue workers table. Filled: active (`ok`), paused (`paused`), stopped (`bad`). Hollow (1.5px inset ring, `subtle`): transitional, unknown, mixed. Mixed adds a `.split` per-instance line.

### Badge / chip
- **Badge:** warn pill, warn 14% tint, 12px/500; "absent on N instances".
- **Queue paused:** not a badge: the paused status dot + "paused" (12px/500 in the queues list name cell, title = paused at; "paused since HH:MM" on the job page).
- **Kind badge** (`.qkind`): neutral pill, `line-strong` edge, no fill, 12px/500 `subtle`; "FIFO" or "Priority" after a queue name.
- **Chip:** neutral pill, `raised` + hairline, 12px; instance names a queue is reported by.

### Segmented range switch
`raised` track, 2px inset, `r-2`; segments 12px/500 `subtle`; current = `panel` fill + 1px `line-strong` inset ring + `fg`. Sits at the right of a chart caption; links are htmx `hx-get` with `hx-push-url`.

### Charts
SVG from `render.go`. Sparklines: one polyline per data run, 1.5 non-scaling stroke, teal (bad inside `.failed`), r=2 dots, hollow `.partial` dots where partial data starts, transparent `.hit` rects with `<title>`. 1h/day charts (`.history`, 720x120 viewBox): grid `3em 1fr` x `auto 1.5em`, y-axis labels left, x-axis below, `line` gridlines, dashed `subtle` crosshair (`.cross`) and `.tip` readout (ground fill, hairline, 4px) driven by `detail.js` on hover. Chart panel caption: title, `peak` value in `subtle`, range switch.

### Instance grid cell
Sticky header row and worker column. Cell: status dot + word on its own line in every state (never colour alone; every cell one height), `busy/conc` in 600, pending right in `subtle`, 2px load bar at the foot (`subtle` fill over `line` track, width = busy/conc). Unreachable instance: column tinted bad 6%, header shows "unreachable". Legend line above; endpoint list (`.units`) below.

### Bulk bar
`#bulk`, sticky 16px from bottom, `raised` + `line-strong`, 8px. Hidden until a worker checkbox is checked (`:has`; always visible without `:has` support). "Selected workers on ..." + Pause / Resume selected.

### Buttons and controls
Default: `raised`, `line-strong` border, 13px/500, 5px 10px, hover border `subtle` + fill `line`, active nudges 1px down, disabled 50%. Primary: teal fill. Warn / danger: tinted text only (danger hover border `bad`). A button that opens a confirm panel ends in `…` ("Restart…", "Stop…").
- **Concurrency:** one form: slider + number box (`.controls input[name="n"]`, kept in sync by `detail.js`) + Set. Only Set applies; the box (sent last) is exact past the slider's max.
- **Valid actions only:** worker page shows Pause when active, Resume when paused, neither (nor Stop) when stopped, all when mixed/unknown/transitional. CSS `:has` on the status dot in `#live`, so it follows each poll with no swap of the controls. Bulk bar keeps both (selection may mix). Queue page: same rule server-side (`#qstate`).

### Confirm panel
One flow for every destructive or disruptive action: worker restart / stop, bulk pause / resume, queue pause / resume, job cancel, workflow cancel. The route answers the panel into `#results` and sends nothing until the form says `confirm=1` (server-side gate, not only the template). `panel`, border warn 45% into `line`, 8px, 14px. Title 16px names action, target and scope as a question ("Stop orders on all instances of shop?"); one `subtle` line; worker panels a table of worker x instance ("not on this instance" in `subtle`). Foot (`confirm_acts`), right-aligned: neutral "Keep" first in tab order and focused (`autofocus`), then verb + object ("Stop orders", "Pause 2 workers", "Cancel job") in its tone: danger for stop / cancel, warn for restart / pause, primary for resume; tone buttons get a 55% tone edge. Keep, or Escape inside the panel, removes it with no request and focuses the control that asked (`app.js`). No native `confirm()`.

### Results list
`#results`, one panel, one row per instance, hairline between. 14px SVG outcome icon: check (ok), dash (no change, warn), cross (failed, bad); worker, instance (600), outcome right-aligned in its colour.

### Banners, notes, errors, empty, waiting
- **Banner** (`#banner`): warn text, warn 10% fill, warn 45% edge, 8px; scope-gone banner links `#banner-merged`.
- **Note:** `raised`, hairline, 6px, 12px `subtle`.
- **Errors** (`#errors`, htmx 4xx/5xx target): bad 8% fill, bad 40% edge, `bad` text.
- **Empty:** centred 13px `subtle`, 32px 16px, 1px dashed `line-strong`, 8px.
- **Waiting:** 16px `fg` line with a 10px teal dot; page meta-refreshes every 2s.

### Job page
- **Header:** `h1.qtitle`: queue name (20px/600, 18px glyph, kind badge); under it the full Queue ID in mono 12px `subtle` with a "Copy ID" button (`queue.js`, `data-copy`; hidden where `navigator.clipboard` is missing); "Namespace" chip (redisq); "Read from instance-1, …" in 12px `subtle` (the workers table names the workers). Breadcrumb ends in the queue name.
- **Workers on this queue:** below the job list (jobs first), `h2.section` + table (instance, worker link to its page, status dot, concurrency) of the workers whose `queue_ids` hold the queue; stacks under 640px like queue rows. None with an older varmq (no `queue_ids`).
- **Queue controls** (`#qstate`, under the header): paused status dot ("paused since"), default buttons "Pause queue" / "Resume queue" (Resume alone when every instance says paused, Pause alone when none does, both when mixed or unknown); hidden, with a `subtle` line, when no instance reports `paused` (varmq < API 1.2.0). Confirm panel lists instance x sent, then results; `#qstate` refreshed out of band.
- **State tabs:** one sticky strip, five equal columns split by hairlines (scrolls sideways under 640px); label (capitalized, state dot: processing warn, completed ok, failed bad, cancelled hollow `bad` ring, pending `subtle`) over 20px count, `—` in `subtle` when varmq does not count the state (cancelled); current `raised` + 2px teal underline.
- **Empty list:** "No <state> jobs." + what shows there (pending, processing); under a non-zero tab count: "The count says N <state>, but no instance listed any" + reload, never a bare "none". Long IDs cut with ellipsis, full in `title`.
- **Job list:** one panel; list head (state word, "N–M of T"); rows are `<details>`: dot, mono ID 14px/600 (italic `subtle` "(no ID yet)"), time right, 14px chevron rotating 90deg (.15s, off under reduced motion); second line mono preview (bad-tinted when last error), attempts right. Hover/open accent 7%.
- **Timeline:** 20px circular SVG step icons tinted 18% (enqueued teal, started warn, finished ok, failed bad; cancelled: dash on a hollow `bad` ring, labelled "Cancelled"), 1px connector; unreached step hollow dashed at 60%.
  - **Gap:** shows absolute times, not durations between steps.
- **Errors:** per attempt, bad 7% fill, bad 30% edge, 6px; mono 12px, pre-wrap.
- **Payload:** ground well, hairline, 6px, mono 12px, max 24em, scrolls, focusable.
- **Pager:** Previous / numbered pages / Next with 14px SVG arrows (workflow list: Previous / "N–M" / Next, no total); buttons 6px bordered `panel`; numbers borderless `subtle`, current `raised` + border + 600; `…` gap.

### One-job page
- **Header:** breadcrumb Queues / queue name (link) / mono job ID; `h1.qid` mono ID + state badge (`.jstate`: pill, `line-strong` edge, state dot as in the tabs, capitalized); Copy ID + full Queue ID 12px `subtle`; `dl.facts`: Waiting for / Running for / Took, Attempts, Read from. `#jobctl`: danger "Cancel job" only while pending or processing; confirm panel, then results ("cancel requested", "already ended", "not supported by this instance"). Cancel is cooperative: the badge stays as read.
- **Timeline panel:** the job list's step icons, a span row between each pair (Waited, Ran: label 13px `subtle`, value `fg`/500, on the connector), "Total" under a hairline (600). Missing time or out of order: `—`, never negative. Live spans (`data-ms`) tick each second in `job.js` from the server's value plus the browser's monotonic clock; without JS the render-time value stands. Errors under it.
- **Data:** Payload and Result panels (`panel`, hairline, 8px), each a `.payload` well; Result "None." with a 64 KiB note on a completed job. Two columns from 900px (timeline 1fr, data 1.3fr), stacked below.
- **Not found:** `.empty` dashed panel: no such job on any instance reporting the queue, then why (in-memory: ended jobs gone without `WithEndedJobs`; shared storage: record dropped), link back. Every instance without the route: one `.empty` line naming varmq API 1.4.0.
- **Links in:** queue list rows (the mono ID alone is the link; the rest of the row expands it), workflow task table and task panel Job IDs. No link for IDs `.`, `..` or none.

### Workflows list
- **State tabs:** the `.states` strip as `.wfstates` (flex, wraps; hairlines top + left, no breakpoint): All, Running, Completed, Failed, Cancelled, then a group (`.grp`, `role="group"`, `line-strong` left edge) "Rollback:" (12px `subtle`) + In progress, Done, Failed (`compensating`, `compensated`, `compensation_failed`). Labels as written, no `capitalize`. Count under each label (20px/600, `1,284`) from `GET /workflow-counts`, "All" their sum; 0 in `subtle`/400 (`.zero`); tab name "Failed, 3 workflows". No counts at all when any instance lacks the route (404), fails, or sends an entry without counts: never a partial sum. Shared stores counted once (two reads differ: the first, in client order), in-memory entries summed; unknown states ignored. No counts either on a negative count, an int64 overflow, more than 256 entries from one instance, or counts slower than 2s (the list does not wait longer). URLs `?state=<state>` as before. Under 640px: not sticky, the group a row of its own; 44px tall under `pointer: coarse`.
- **State words:** one set on every workflow page: `compensating` "rolling back", `compensated` "rolled back", `compensation_failed` "rollback failed"; other `_` read as a space; unknown states shown as sent, hollow `subtle` dot.
- **Rows** (`table.wfs`): mono ID link, state (`.wst` dot + words; rolling back: "1 of 2 undone", rollback failed: "1 of 2 not rolled back" in `bad`, 12px under it), progress, created (short time, full in title), duration (`so far` in `subtle` while running), "Reported by" chips. Stack under 640px like queue rows. Empty page, per tab: "No workflows.", "No failed workflows.", "No workflows rolling back.", "No workflows rolled back.", "No workflows whose rollback failed." One row per (ID, created_at): shared durable ones once, an in-memory twin its own row (link carries `at=`).
- **Progress** (`.prog`): 72x6 bar, 3px radius, `line` track; segments completed `ok`, failed `bad`, cancelled `bad` 35% into `line`, in flight `warn`; then `ended/total` 13px `subtle`. Has an `aria-label` with the counts.

### Workflow page
- **Header:** mono ID (20px/600) + state; "Reported by" chips; facts row (`dl.facts`: 12px `subtle` term over value: Created, Running for / Took, Tasks ended, On cancel); `#wfctl` danger buttons "Cancel workflow" and "Cancel without undo" (the second only while `undo_on_cancel`), none once ended. `#results` sits in `#live` with `hx-preserve`, so a poll keeps an open confirm panel.
- **Rollback summary** (`#rollback`, over the graph; rolling back or rollback failed only): `panel` box, hairline, 8px; h2 16px/600. Failed (`.bad`): `bad` 7% fill, 30% edge, `bad` title "Rollback failed", "N of M steps were not rolled back. The changes their tasks made are still in place."; lists "Not rolled back" (task link, undo state, "Undo job <id>" link, reason mono `bad`), "Rolled back" ("undone in 0.3s"), "Why it rolled back" (failed tasks + reason). Rolling back: "Rolling back", "N of M undone.", "Still to undo". Counts and lists only completed tasks with an undo job: varmq undoes no other (a task that never completed shows a waiting undo record all the same). Reasons cut at 500 chars + "…" (the panel has them whole).
- **Graph well** (`.dagbox` > `.dag-port`): `bg` well, hairline, 8px. 300–560px tall (graph height + 24 via `--gh`, 62vh cap), the same with and without workflow.js, so nothing moves when it starts or a poll swaps. With workflow.js (`.pz`) pans/zooms in place; without, it scrolls. Tool buttons top-left (zoom in / out / fit; Pan toggle on `any-pointer: coarse`; 14px icons, `panel` fill; 14px padding on coarse). Touch: one finger down the well scrolls the page (`touch-action: pan-y`), across pans, two pinch; Pan pressed (`.free`) gives the well every drag. First view: whole graph when it reads at >= 0.85, else 0.85 on the first task in flight or waiting (else the first failed), a third in from the left, its top under the tool buttons.
- **Legend** (`.dag-legend`, 12px `subtle`): a chip per task state (waiting, "pending or processing", completed, failed, cancelled: the node words) with its task count (`fg`/600; 0 `subtle`/400): with workflow.js a toggle (`aria-pressed`, pressed: `accent` edge, `raised`) that fades other tasks in the graph and hides their table rows ("N of M shown" by the Tasks heading); without, disabled. A pressed chip whose count drops to 0 lets go. Then edge keys: solid, dashed (runs always), dotted (stopped), slow mark. Help line under it: drag or arrows pan, Ctrl/Cmd + scroll or + - zoom, 0 fit, Enter job, Space details.
- **Layout** (Go, `dag.go`): left to right; column = longest path from a root; dummy slots (10px) carry long edges so none crosses a box; order by barycenter, 4 sweeps (each re-places only the layer it sorts). Past 5000 tasks + edges + dummy slots no graph is drawn: a note, and the table. Boxes 184x56, columns 64px apart, rows 30px apart, 20px pad. Edges: horizontal-tangent cubics, straight through dummy columns, 7px arrowhead in the edge's colour (`context-stroke`).
- **Task node** (SVG `<a id="node-N" href="#task-N">`, `data-chip`, `data-job`; `aria-label` "name, state, after deps, runs always, ran 2.1s, undo state"): `panel` box, 1px `--mark` (`subtle` 70% into `bg`, >= 3:1 on the well both themes), 8px; 9px state dot; name 13px/600 `fg` (cut at 20 chars with "…", full in `<title>`); "state · run time" 12px/500 `subtle`, "state · queue name" when no times (name cut at 12 chars). Failed: `bad` cross instead of the dot, `bad` 7% fill, `bad` edge. Cancelled: hollow `bad` dot struck through, name `subtle`. In flight: `warn` edge + pulse ring. Waiting: hollow dot, dashed box, no fill. Slowest task on the path that set the end (ended workflows, not rolled back: undo jobs set that end; a same-ms tie goes to the dependent): 12px clock, `subtle` stroke. Hover/focus: `accent` edge, focus 3px; selected or `:target`: 2px `accent` + 8% fill. Tab order: by column, then down it (topological).
- **Undo badge:** 72x18 pill on the box's bottom edge, right: `raised` + `--mark`, `fg` stroke undo arrow, "undo" 12px, 6px dot in the undo job's state colour. Undo job failed: 96x18 "undo failed" in `bad`/600, `bad` edge, and the node's box edge `bad` (`.undo-failed`).
- **Edges:** 1.5px `subtle`; into an always task dashed 5 4; out of a failed or cancelled task into one that does not run always dotted 1 5 (`.dead`: nothing passes); into an in-flight task a `warn` 2px overlay dashed 2 10 flowing (dashoffset, .9s). Hover/focus a task, in the graph or its table row: its upstream and downstream stay, traced edges `accent`, the rest fade (nodes 32%, edges 15%), its row `.hl` (accent 8%).
- **Keys** (on the well or a task): Tab walks tasks; Enter opens the task's job (its details when it has none); Space its details; Escape closes them; arrows pan 48px; + / - zoom; 0 fits.
- **Task panel** (`.wf-panel`): a 340px column right of the well (`panel`, `line-strong`, 8px, no shadow); the well narrows by 352px while it is open (`.panel-open`), so no task hides under it; as tall as the well at most, scrolls; under the graph below 760px. Facts as `dl.kv` (6.5em terms, 13px), reason in mono `bad`, "Undo job" block under a hairline; close button top-right (44px under `pointer: coarse`). `#wf-panel` is `hx-preserve`d: while its task's details do not change, a poll leaves its text alone and sets its scroll and text selection back. Facts add Waited / Ran (Running for) when the record has times. Opening it shows the first view that reads (zoom >= 0.85): the whole graph, else the task's lineage (upstream + downstream), else the task and its downstream, else the task centred.
- **Task table** (`table.tasks`): the accessible fallback, row `id="task-N"` `data-chip` (`:target` and `.hl` accent 8%): name (mono 13px, a link to `#node-N` when a graph is drawn; `always` chip), state + reason (12px `subtle`), deps, queue name link (to its job page), job ID, Waited and Ran (`.num`; Waited counts from the first enqueue, earlier attempts in it (header `title`); "so far" while running, kept on the time's line, `slowest` chip; "<0.1s" under 50ms; "—" for a task without times, or a time not to trust (<= 0, or over a day past now); both columns only when some task has times: varmq < contract 0.9.0 or a store without times shows none, no "0s"), result size, undo state + its run time + "undo job" link + reason. Stacks under 640px (queue and undo full width).
- **Motion:** pulse ring 1.8s ease-out, flowing edge .9s linear, box stroke .12s. All off under `prefers-reduced-motion` (ring shown static at 50%).
- **Live:** workflow.js loads on every workflow page (`OwnPoll`), so a graph a later poll brings in still pans and zooms. `#live` polls itself (`hx-trigger="every Ns"`, `hx-disinherit="*"`) while the workflow runs; the first answer of an ended (or missing, or ambiguous) workflow has no trigger, so polling stops. workflow.js keeps the view, open panel, pressed chips, traced task, focused task and focused control across each swap (the focus a swap hands back does not pan the view; a Tab does); htmx settles id'd elements (nodes, rows) back to the server's class, so the marks go on again on `htmx:afterSettle`. The page's `#live` is `.wf-live`: `overflow-anchor: none`, or Chrome anchors on the focused element in the old `#live` during the swap and scrolls the page by the new one's height. Unlike the dashboard and worker page (`TestLiveRegionHoldsNoControls`), the cancel buttons, zoom buttons and panel live inside `#live`: their state follows the record (no cancel once it ended), and the script hands focus back.

## Do's and Don'ts

### Do:
- **Do** use `accent` only for selection, focus, current, completed; `bad` for failed series, `subtle` for load fill.
- **Do** group related numbers in one hairline-split panel (8px radius, 1px `line`).
- **Do** set every number in tabular figures and right-align numeric columns.
- **Do** mark uncertain status (transitional, unknown, mixed, a waiting task, a compensating workflow) with a hollow dot. One exception, in `bad`: cancelled, of a job, task or workflow (stopped before it ran, not failed).
- **Do** draw icons as inline SVG (`.ico`: stroke currentColor, 1.5, round caps/joins), `aria-hidden`; store glyphs (`.sico`) are filled.
- **Do** keep tables scrolling inside their wrapper and stack worker/queue rows under 640px.
- **Do** check every new text colour for >= 4.5:1 on `bg`, `panel`, `raised` in both themes.

### Don't:
- **Don't** add drop or offset shadows; inset box-shadow only as a 1-2px stroke.
- **Don't** uppercase text (Go test enforces); `capitalize` is fine.
- **Don't** use unicode glyphs as icons; `—` (no data), `·`, `×`, `…`, `/` as punctuation are fine.
- **Don't** use mono outside IDs, payloads, errors, endpoints.
- **Don't** put state colours on decoration, or teal on anything with a second meaning.
- **Don't** use gradients (the load bar's hard-stop `linear-gradient` is a fill meter, not a gradient), card soup, or terminal styling.
- **Don't** rename htmx ids or detail.js hooks: `page`, `live`, `poller`, `status`, `banner`, `banner-merged`, `bulk`, `errors`, `results`, `qstate`, `wfctl`, `pick-server`, `pick-unit`; `.history`, `.tip`, `.hit`, `.cross`, `.controls input[name="n"]`; workflow.js: `.dagbox`, `.dag-port`, `svg.dag` (`data-w`, `data-h`), `.view`, `.node[data-i]` (`data-chip`, `data-job`, `#node-N`), `.edge`/`.flow` (`data-from`, `data-to`), `#task-detail-N`, `.wf-panel` (`#wf-panel`), `.dag-tools [data-zoom]`, `[data-pan]`, `.dag-legend [data-chip]`, `.dag-help`, `#task-shown`, `.tasks tr[data-chip]`, `.wf-live`.
