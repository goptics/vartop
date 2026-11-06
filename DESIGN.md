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

**Imagery and raster provenance.** No generated imagery; the UI has none. `docs/images/*.png` (README shots, dark + light) are Playwright captures: Chrome, 1280x560 viewport, DPR 2, fullPage; `chart-hover-*` hovers the first chart element at 42% width. Subject: `vartop web` reading a synthetic store; a throwaway Go seeder wrote 8 days of daily-rhythm ticks for server "demo-cluster" (3 instances, 10.0.4.11-13; workers billing-sync, emails, thumbnails, webhooks, reports [paused on instance-2], search-index), then kept appending live ticks. Captured 2026-11-05.

## Colors

Cool neutral greys, one teal accent, four state hues. Light values are the `-light` keys; they replace the dark ones under `@media (prefers-color-scheme: light)`.

### Primary
- **varmq Teal** (`accent`): selection, focus ring, links, current-nav icon, brand mark, completed series in charts and sparklines, waiting dot, current state-tab underline, job-row hover/open tint (7%), selected-row tint (8%), `::selection` (35%), native `accent-color` (slider, checkboxes), primary button fill.
- **Teal Ink** (`on-accent`): text on accent fill only.

### State
- **Ok Green** (`ok`): active status, healthy count, ok result, finished step, completed tab.
- **Amber** (`warn`): processing tab, started step, no-change result, absent badge, stale freshness, banners, confirm-panel edge, Restart button text.
- **Coral Red** (`bad`): stopped status, failed series, hot stat tile, errors, failed tab and step, down grid column tint (6%), Stop button text.
- **Periwinkle** (`paused`): paused status only.

### Neutral
- **Ground** (`bg`): page, payload well, chart tooltip.
- **Panel** (`panel`): every container surface.
- **Raised** (`raised`): table head, default button, current nav/tab, chip, bulk bar, notes.
- **Hairline** (`line`): borders, dividers, gridlines, load-bar track.
- **Strong Hairline** (`line-strong`): control borders, empty-state dash, bulk bar edge, scrollbar.
- **Ink** (`fg`) / **Muted Ink** (`subtle`): text; `subtle` also unknown/mixed/transitional status, crosshair, load-bar fill.

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
- **Headline** (600, 20px, -.01em): page `h1`, queue ID (mono), state-tab counts.
- **Title** (600, 16px): confirm heading, waiting line (400), brand (650).
- **Body** (400, 14px, 1.5): default text, table rows, chips list.
- **Label** (500, 13px): tile headings, `.sub` scope lines, controls, figcaptions, pager.
- **Caption** (500, 12px): table heads, chart axes, tooltip, notes, badge, chip, job meta.
- **Mono** (size follows its context, 12-20px): queue/job IDs, previews, payload, attempt errors.

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
- **Overflow:** tables scroll inside their wrapper, never the page. Instance grid capped at `100vh - 140px`, sticky header row and worker column.

## Elevation & Depth

Flat. No drop or offset shadows anywhere, dialogs included (the only modal is the browser's native confirm box). Depth is tonal: `bg` < `panel` < `raised`, separated by 1px hairlines. Sticky elements (rail, state tabs, bulk bar, grid headers) rely on opaque fills, not shadows.

Inset box-shadow appears only as a stroke, three places: hollow status dot (`inset 0 0 0 1.5px`), current range segment ring (`inset 0 0 0 1px line-strong`), current state-tab underline (`inset 0 -2px 0 accent`).

### Named Rules
**The Flat Rule.** A surface is lifted by a step in tone and a hairline, never by a blur.

## Shapes

Small, consistent corners: 4px (focus ring, range segment, tooltip), 6px (controls, nav links, pager, attempt errors, payload), 8px (every panel: stat strip, tables, charts, controls, bulk bar, confirm, results, job list, banners, empty state). Pills (999px) for chip and badge; circles for status dots (7px; 8px in grid cells) and timeline icons (20px). Borders always 1px; dashed only for the empty state, the unreached timeline step, and the job-body top divider.

## Components

### Rail / shell
Brand (20px three-line SVG mark in accent, wordmark 16px/650). Nav: Dashboard, Instance grid, Queues, each with a 16px stroke icon; links `subtle`, hover and current `raised` + `fg`, current icon teal. Scope: server and instance pickers together (`#pick-server`, `#pick-unit`). `#status`: health with 7px dot (ok / warn / bad by healthy share), refresh interval, "updated" (warn when stale), optional `.note`.

### Page header
`h1` + `.sub` scope line on one baseline, wrapping. Worker and job pages add breadcrumbs (`/` separator, 13px, `subtle`).

### Stat strip
One `panel` (8px, hairline) holding auto-fit tiles (min 150px) split by hairlines (each tile draws left+top edge pulled 1px under the frame). Tile: label heading, display number, sparkline at foot. `.hot` (failed > 0): number and onset line in `bad`. Onset line: "since 15:04" under the failed number. Status tile lists status counts as status dots. Worker page counters use the same strip without sparklines.
- **Gap:** tile sparklines are fixed 160x32 (row trends 120x24); they do not stretch with the tile.

### Tables
Inside `.table-wrap` panel. Head `raised`, 12px/500 `subtle`; cells 8px 12px, hairline rows, no zebra. Row hover `raised` 60%; selected row accent 8%. Names `fg`/500, accent + underline on hover. Under 640px worker and queue rows stack into a labelled grid (head moved off-screen, `data-label` captions, status on its own line).
- **Gap:** no column sorting; needs a server-side sort param (no new JS).

### Status dot
7px dot (`--c`) + label in `fg`. Filled: active (`ok`), paused (`paused`), stopped (`bad`). Hollow (1.5px inset ring, `subtle`): transitional, unknown, mixed. Mixed adds a `.split` per-instance line.

### Badge / chip
- **Badge:** warn pill, warn 14% tint, 12px/500; "absent on N instances".
- **Chip:** neutral pill, `raised` + hairline, 12px; instance names a queue is reported by.

### Segmented range switch
`raised` track, 2px inset, `r-2`; segments 12px/500 `subtle`; current = `panel` fill + 1px `line-strong` inset ring + `fg`. Sits at the right of a chart caption; links are htmx `hx-get` with `hx-push-url`.

### Charts
SVG from `render.go`. Sparklines: one polyline per data run, 1.5 non-scaling stroke, teal (bad inside `.failed`), r=2 dots, hollow `.partial` dots where partial data starts, transparent `.hit` rects with `<title>`. 1h/day charts (`.history`, 720x120 viewBox): grid `3em 1fr` x `auto 1.5em`, y-axis labels left, x-axis below, `line` gridlines, dashed `subtle` crosshair (`.cross`) and `.tip` readout (ground fill, hairline, 4px) driven by `detail.js` on hover. Chart panel caption: title, `peak` value in `subtle`, range switch.

### Instance grid cell
Sticky header row and worker column. Cell: status dot (label hidden for active, shown for other states), `busy/conc` in 600, pending right in `subtle`, 2px load bar at the foot (`subtle` fill over `line` track, width = busy/conc). Unreachable instance: column tinted bad 6%, header shows "unreachable". Legend line above; endpoint list (`.units`) below.

### Bulk bar
`#bulk`, sticky 16px from bottom, `raised` + `line-strong`, 8px. Hidden until a worker checkbox is checked (`:has`; always visible without `:has` support). "Selected workers on ..." + Pause / Resume selected.

### Buttons and controls
Default: `raised`, `line-strong` border, 13px/500, 5px 10px, hover border `subtle` + fill `line`, active nudges 1px down, disabled 50%. Primary: teal fill. Warn / danger: tinted text only (danger hover border `bad`). Range slider and number input share the `controls` panel (`.controls input[name="n"]` kept in sync by `detail.js`).
- **Gap:** Restart and Stop use the native `hx-confirm` box, not the confirm panel; only bulk actions use the panel.

### Confirm panel
`panel`, border warn 45% into `line`, 8px, 14px. Title 16px, one `subtle` line, a table of worker x instance ("not on this instance" in `subtle`), right-aligned teal "Confirm <action>".

### Results list
`#results`, one panel, one row per instance, hairline between. 14px SVG outcome icon: check (ok), dash (no change, warn), cross (failed, bad); worker, instance (600), outcome right-aligned in its colour.

### Banners, notes, errors, empty, waiting
- **Banner** (`#banner`): warn text, warn 10% fill, warn 45% edge, 8px; scope-gone banner links `#banner-merged`.
- **Note:** `raised`, hairline, 6px, 12px `subtle`.
- **Errors** (`#errors`, htmx 4xx/5xx target): bad 8% fill, bad 40% edge, `bad` text.
- **Empty:** centred 13px `subtle`, 32px 16px, 1px dashed `line-strong`, 8px.
- **Waiting:** 16px `fg` line with a 10px teal dot; page meta-refreshes every 2s.

### Job page
- **Header:** mono queue ID (20px/600), "Reported by" chips.
- **State tabs:** one sticky strip, four equal columns split by hairlines; label (capitalized, state dot: processing warn, completed ok, failed bad, pending `subtle`) over 20px count; current `raised` + 2px teal underline.
- **Job list:** one panel; list head (state word, "N–M of T"); rows are `<details>`: dot, mono ID 14px/600 (italic `subtle` "(no ID yet)"), time right, 14px chevron rotating 90deg (.15s, off under reduced motion); second line mono preview (bad-tinted when last error), attempts right. Hover/open accent 7%.
- **Timeline:** 20px circular SVG step icons tinted 18% (enqueued teal, started warn, finished ok, failed bad), 1px connector; unreached step hollow dashed at 60%.
  - **Gap:** shows absolute times, not durations between steps.
- **Errors:** per attempt, bad 7% fill, bad 30% edge, 6px; mono 12px, pre-wrap.
- **Payload:** ground well, hairline, 6px, mono 12px, max 24em, scrolls, focusable.
- **Pager:** Previous / numbered pages / Next with 14px SVG arrows; buttons 6px bordered `panel`; numbers borderless `subtle`, current `raised` + border + 600; `…` gap.

## Do's and Don'ts

### Do:
- **Do** use `accent` only for selection, focus, current, completed; `bad` for failed series, `subtle` for load fill.
- **Do** group related numbers in one hairline-split panel (8px radius, 1px `line`).
- **Do** set every number in tabular figures and right-align numeric columns.
- **Do** mark uncertain status (transitional, unknown, mixed) with a hollow dot.
- **Do** draw icons as inline SVG (`.ico`: stroke currentColor, 1.5, round caps/joins), `aria-hidden`.
- **Do** keep tables scrolling inside their wrapper and stack worker/queue rows under 640px.
- **Do** check every new text colour for >= 4.5:1 on `bg`, `panel`, `raised` in both themes.

### Don't:
- **Don't** add drop or offset shadows; inset box-shadow only as a 1-2px stroke.
- **Don't** uppercase text (Go test enforces); `capitalize` is fine.
- **Don't** use unicode glyphs as icons; `—` (no data), `·`, `×`, `…`, `/` as punctuation are fine.
- **Don't** use mono outside IDs, payloads, errors, endpoints.
- **Don't** put state colours on decoration, or teal on anything with a second meaning.
- **Don't** use gradients (the load bar's hard-stop `linear-gradient` is a fill meter, not a gradient), card soup, or terminal styling.
- **Don't** rename htmx ids or detail.js hooks: `page`, `live`, `poller`, `status`, `banner`, `banner-merged`, `bulk`, `errors`, `results`, `pick-server`, `pick-unit`; `.history`, `.tip`, `.hit`, `.cross`, `.controls input[name="n"]`.
