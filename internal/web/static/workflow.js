// The workflow page's script. The task graph (server-rendered SVG) pans and
// zooms in place: drag, Ctrl + scroll or pinch, the +, - and fit buttons,
// or keys on the focused graph or a task (+ - 0, arrows). On touch a finger
// down the graph scrolls the page; the Pan button gives the graph every
// drag. A task hovered or focused shows its upstream and downstream, and
// its table row (and the row its task). A task clicked, or Space on it,
// opens its details beside the graph (Escape closes); Enter opens its job.
// A state chip of the legend shows the tasks of that state only, in the
// graph and the table. Without this script the graph scrolls and each task
// links to its table row.
//
// While the workflow runs #live polls and is swapped whole: the view, the
// open task, the chips pressed and whatever had focus in it are kept
// across each swap. The panel is hx-preserve'd: while its task's details
// do not change, its text stays, and its scroll and text selection are
// set again (htmx moves it: both reset). The page's scroll is CSS's to
// keep (app.css, #live).
(() => {
  const MIN = 0.2, MAX = 2.5;
  let view = null; // {k, x, y}: the pan and zoom of the graph shown
  let shown = ""; // the page the view is of
  let open = null; // the index of the task whose panel is open
  let free = false; // touch: the graph takes every drag (the Pan button)
  const chips = new Set(); // the state chips pressed
  let drag = null; // the pointer drag under way
  let swapping = false; // a poll's swap under way: the focus it moves is not a Tab
  let panelTop = 0; // the panel's scroll before a swap
  let kept = null; // a text selection in the panel before a swap: [anchor, offset, focus, offset]
  let marked = null; // the task trace last marked
  const pointers = new Map(); // pointers down on the graph: pinch

  function parts() {
    const port = document.querySelector(".dag-port");
    const svg = port && port.querySelector("svg.dag");
    if (!svg) return null;
    const box = port.closest(".dagbox");
    return { port, svg, g: svg.querySelector(".view"), box, panel: box.querySelector(".wf-panel") };
  }

  const clamp = (k) => Math.max(MIN, Math.min(MAX, k));

  function apply(p) {
    p.g.setAttribute("transform", `translate(${view.x.toFixed(1)} ${view.y.toFixed(1)}) scale(${view.k.toFixed(4)})`);
  }

  // fit shows the whole graph, at most at its own size, centred.
  // READ: the least zoom that reads (a 13px name at 11px).
  const READ = 0.85;
  function fit(p) {
    const w = +p.svg.dataset.w, h = +p.svg.dataset.h;
    const cw = p.port.clientWidth, ch = p.port.clientHeight;
    const k = clamp(Math.min(1, cw / w, ch / h));
    view = { k, x: (cw - w * k) / 2, y: (ch - h * k) / 2 };
    apply(p);
  }

  // rect is task node n's box in the graph's own units.
  function rect(n) {
    const [x, y] = n.getAttribute("transform").match(/[-\d.]+/g).map(Number);
    const box = n.querySelector(".box");
    return { x, y, w: +box.getAttribute("width"), h: +box.getAttribute("height") };
  }

  // first is the view a graph opens with: whole when that reads, else at
  // READ on the first task not ended (the nodes are in topological
  // order), or the first that failed, a third in from the left; the rest
  // is a drag away.
  function first(p) {
    fit(p);
    if (view.k >= READ) return;
    const w = +p.svg.dataset.w, h = +p.svg.dataset.h;
    const cw = p.port.clientWidth, ch = p.port.clientHeight, k = READ;
    const n = p.svg.querySelector(".node.live, .node.t-waiting") || p.svg.querySelector(".node.undo-failed, .node.t-failed");
    const r = n ? rect(n) : { x: 0, y: 0, w: 0, h: 0 };
    // within the graph's edges; a side that fits is centred
    const along = (c, size, at) => (size * k <= c ? (c - size * k) / 2 : Math.min(0, Math.max(c - size * k, at)));
    // the tool buttons cover the well's top: the graph's top starts under them
    const t = p.box.querySelector(".dag-tools").offsetHeight + 10;
    view = { k, x: along(cw, w, n ? cw / 3 - (r.x + r.w / 2) * k : 0), y: t + along(ch - t, h, n ? (ch - t) / 2 - (r.y + r.h / 2) * k : 0) };
    apply(p);
  }

  // show pans the least that brings task node n whole into the well.
  function show(p, n) {
    const r = rect(n), pad = 12;
    const cw = p.port.clientWidth, ch = p.port.clientHeight;
    const x0 = r.x * view.k + view.x, y0 = r.y * view.k + view.y;
    const x1 = x0 + r.w * view.k, y1 = y0 + (r.h + 10) * view.k; // its undo badge with it
    if (x0 < pad) view.x += pad - x0;
    else if (x1 > cw - pad) view.x -= x1 - (cw - pad);
    if (y0 < pad) view.y += pad - y0;
    else if (y1 > ch - pad) view.y -= y1 - (ch - pad);
    apply(p);
  }

  // zoomBy scales the view by f around (cx, cy), a point of the port (its centre by default).
  function zoomBy(p, f, cx = p.port.clientWidth / 2, cy = p.port.clientHeight / 2) {
    const k = clamp(view.k * f);
    view.x = cx - ((cx - view.x) * k) / view.k;
    view.y = cy - ((cy - view.y) * k) / view.k;
    view.k = k;
    apply(p);
  }

  // init takes over a graph just rendered: the first time its first
  // view; after a swap, the view, chips, panel and focus it had. The
  // well's height is CSS's (--gh): nothing moves when the script starts.
  function init() {
    const p = parts();
    // htmx fires afterSwap for the oob parts of a poll too: a graph taken
    // over already is left as it is (its panel, and the focus in it)
    if (!p || p.svg.dataset.taken) return;
    p.svg.dataset.taken = "1";
    p.port.classList.add("pz");
    p.svg.removeAttribute("viewBox"); // user units are px; .view pans and zooms
    p.box.querySelector(".dag-tools").hidden = false;
    p.box.querySelector(".dag-help").hidden = false;
    for (const c of p.box.querySelectorAll(".dag-legend [data-chip]")) c.disabled = c.dataset.n === "0";
    const page = location.pathname + location.search;
    if (!view || shown !== page) {
      shown = page;
      open = null;
      chips.clear();
      first(p);
    } else {
      apply(p);
    }
    // a chip pressed whose tasks are gone is disabled: no way to unpress
    // it, so it lets go
    for (const k of [...chips]) {
      const c = p.box.querySelector(`.dag-legend [data-chip="${CSS.escape(k)}"]`);
      if (!c || c.disabled) chips.delete(k);
    }
    pan(p);
    filter(p);
    if (open !== null && !document.getElementById("task-detail-" + open)) open = null; // its task is gone
    if (open === null) {
      p.panel.hidden = true; // the panel kept across the swap closes
      return;
    }
    select(open, false);
    p.panel.scrollTop = panelTop;
    if (kept && p.panel.contains(kept[0]) && p.panel.contains(kept[2])) getSelection().setBaseAndExtent(...kept);
    kept = null;
  }

  // pan sets the Pan button and the well as free says.
  function pan(p) {
    p.port.classList.toggle("free", free);
    p.box.querySelector("[data-pan]").setAttribute("aria-pressed", free);
  }

  // filter shows the tasks of the pressed state chips only: the others
  // fade in the graph and leave the table. jump: a chip was pressed, so
  // the view moves to the tasks it shows.
  function filter(p, jump) {
    const on = chips.size > 0;
    for (const c of p.box.querySelectorAll(".dag-legend [data-chip]")) c.setAttribute("aria-pressed", chips.has(c.dataset.chip));
    p.svg.classList.toggle("filter", on);
    const hit = [];
    for (const n of p.svg.querySelectorAll(".node")) {
      const m = on && chips.has(n.dataset.chip);
      n.classList.toggle("match", m);
      if (m) hit.push(n.dataset.i);
    }
    const rows = [...document.querySelectorAll(".tasks tbody tr")];
    for (const r of rows) r.hidden = on && !chips.has(r.dataset.chip);
    const note = document.getElementById("task-shown");
    if (note) note.textContent = on ? `${hit.length} of ${rows.length} shown` : "";
    if (jump && hit.length && !frame(p, hit)) {
      view.k = Math.max(view.k, READ);
      const r = rect(node(hit[0]));
      view.x = p.port.clientWidth / 3 - (r.x + r.w / 2) * view.k;
      view.y = p.port.clientHeight / 2 - (r.y + r.h / 2) * view.k;
      apply(p);
    }
  }

  const node = (i) => document.querySelector(`.dag .node[data-i="${i}"]`);

  // lineage is task i and every task upstream and downstream of it (ids as
  // strings), and the edges of svg with whether each lies on those paths.
  function lineage(svg, i) {
    const edges = [...svg.querySelectorAll(".edge, .flow")].map((e) => ({ e, from: e.dataset.from, to: e.dataset.to }));
    const walk = (start, next) => {
      const seen = new Set([start]);
      const todo = [start];
      while (todo.length) {
        const cur = todo.pop();
        for (const x of next(cur)) {
          if (!seen.has(x)) {
            seen.add(x);
            todo.push(x);
          }
        }
      }
      return seen;
    };
    const id = String(i);
    const up = walk(id, (c) => edges.filter((x) => x.to === c).map((x) => x.from));
    const down = walk(id, (c) => edges.filter((x) => x.from === c).map((x) => x.to));
    for (const x of edges) x.on = (up.has(x.to) && up.has(x.from)) || (down.has(x.from) && down.has(x.to));
    return { tasks: new Set([...up, ...down]), down, edges };
  }

  // trace marks task i, every task upstream and downstream of it, and the
  // edges between them; the rest of the graph fades. Its table row is
  // marked with it. null clears it.
  function trace(i) {
    const p = parts();
    if (!p) return;
    marked = i;
    const svg = p.svg;
    document.querySelectorAll("#live .hl").forEach((el) => el.classList.remove("hl"));
    svg.classList.toggle("trace", i !== null);
    if (i === null) return;
    const row = document.getElementById("task-" + i);
    if (row) row.classList.add("hl");
    const l = lineage(svg, i);
    for (const n of svg.querySelectorAll(".node")) {
      if (l.tasks.has(n.dataset.i)) n.classList.add("hl");
    }
    for (const x of l.edges) if (x.on) x.e.classList.add("hl");
  }

  // select opens task i's details in the panel; focus moves into it when
  // a key opened it. move: the user opened it (not a swap reopening it),
  // so the view may move to show the task.
  function select(i, focus, move) {
    const p = parts();
    const src = document.getElementById("task-detail-" + i);
    if (!p || !src) return;
    open = i;
    p.svg.querySelectorAll(".node.sel").forEach((n) => n.classList.remove("sel"));
    const n = node(i);
    if (n) n.classList.add("sel");
    // the same details: left as they are, so a text selection in them stays
    const body = p.panel.querySelector(".body");
    if (body.innerHTML !== src.innerHTML) body.innerHTML = src.innerHTML;
    p.panel.hidden = false;
    // the panel takes its own column beside the well (below it on a
    // phone): the well narrows, nothing hides under the panel. The view
    // then shows the whole graph if it still reads, else centres the task,
    // its neighbours either side.
    p.box.classList.add("panel-open");
    p.panel.style.maxHeight = getComputedStyle(p.panel).position === "absolute" ? p.port.clientHeight + "px" : "";
    if (move) reveal(p, i);
    if (focus) p.panel.focus({ preventScroll: true });
  }

  // frame shows the tasks ids (indexes) whole in the well when that
  // reads (zoom at least READ); false when it does not.
  function frame(p, ids) {
    const cw = p.port.clientWidth, ch = p.port.clientHeight, pad = 16;
    let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
    for (const id of ids) {
      const n = node(id);
      if (!n) continue;
      const r = rect(n);
      x0 = Math.min(x0, r.x);
      y0 = Math.min(y0, r.y);
      x1 = Math.max(x1, r.x + r.w);
      y1 = Math.max(y1, r.y + r.h + 10);
    }
    if (x1 < x0) return false;
    const k = Math.min(1, (cw - 2 * pad) / (x1 - x0), (ch - 2 * pad) / (y1 - y0));
    if (k < READ) return false;
    view = { k, x: (cw - (x0 + x1) * k) / 2, y: (ch - (y0 + y1) * k) / 2 };
    apply(p);
    return true;
  }

  // reveal brings task i into view in the well as it is now, the first
  // that reads: the whole graph; the task with all it waits on and all
  // that waits on it; the task and all that waits on it; else the task
  // centred, as close as it is, its neighbours either side.
  function reveal(p, i) {
    const w = +p.svg.dataset.w, h = +p.svg.dataset.h;
    if (Math.min(1, p.port.clientWidth / w, p.port.clientHeight / h) >= READ) return fit(p);
    const l = lineage(p.svg, i);
    if (frame(p, l.tasks) || frame(p, l.down)) return;
    const n = node(i);
    if (!n) return;
    const r = rect(n), k = Math.max(READ, Math.min(view.k, 1));
    view = { k, x: p.port.clientWidth / 2 - (r.x + r.w / 2) * k, y: p.port.clientHeight / 2 - (r.y + r.h / 2) * k };
    apply(p);
  }

  function close() {
    const p = parts();
    if (!p || open === null) return;
    const i = open;
    open = null;
    p.panel.hidden = true;
    p.box.classList.remove("panel-open");
    p.svg.querySelectorAll(".node.sel").forEach((n) => n.classList.remove("sel"));
    const n = node(i);
    if (n) n.focus({ preventScroll: true });
  }

  const portOf = (e) => e.target.closest && e.target.closest(".dag-port.pz");
  const at = (p, e) => {
    const r = p.port.getBoundingClientRect();
    return [e.clientX - r.left, e.clientY - r.top];
  };

  document.addEventListener(
    "wheel",
    (e) => {
      if (!portOf(e) || !(e.ctrlKey || e.metaKey)) return; // a plain wheel scrolls the page
      e.preventDefault();
      const p = parts();
      const d = e.deltaY * (e.deltaMode === 1 ? 16 : 1); // Firefox: lines
      zoomBy(p, Math.exp(-d * 0.002), ...at(p, e));
    },
    { passive: false },
  );

  document.addEventListener("pointerdown", (e) => {
    if (!portOf(e) || e.button !== 0) return;
    pointers.set(e.pointerId, [e.clientX, e.clientY]);
    drag = { id: e.pointerId, x: e.clientX, y: e.clientY, moved: false };
  });
  document.addEventListener("pointermove", (e) => {
    if (!pointers.has(e.pointerId)) return;
    const p = parts();
    if (!p) return; // the graph went with a swap mid-drag
    const prev = pointers.get(e.pointerId);
    pointers.set(e.pointerId, [e.clientX, e.clientY]);
    if (pointers.size === 2) {
      // pinch: zoom by the change of the fingers' distance, around their middle
      const [a, b] = [...pointers.values()];
      const other = [...pointers.entries()].find(([id]) => id !== e.pointerId)[1];
      const before = Math.hypot(prev[0] - other[0], prev[1] - other[1]);
      const after = Math.hypot(a[0] - b[0], a[1] - b[1]);
      const r = p.port.getBoundingClientRect();
      if (before > 0) zoomBy(p, after / before, (a[0] + b[0]) / 2 - r.left, (a[1] + b[1]) / 2 - r.top);
      if (drag) drag.moved = true;
      return;
    }
    if (!drag || drag.id !== e.pointerId) return;
    if (!drag.moved && Math.hypot(e.clientX - drag.x, e.clientY - drag.y) < 4) return; // a click, so far
    if (!drag.moved) {
      drag.moved = true;
      p.port.classList.add("dragging");
      p.port.setPointerCapture(e.pointerId);
    }
    view.x += e.clientX - prev[0];
    view.y += e.clientY - prev[1];
    apply(p);
  });
  const lift = (e) => {
    pointers.delete(e.pointerId);
    if (!drag || drag.id !== e.pointerId) return;
    const p = parts();
    if (p) p.port.classList.remove("dragging");
    if (drag.moved) {
      // the click that ends a drag opens nothing
      const swallow = (c) => {
        c.preventDefault();
        c.stopPropagation();
      };
      document.addEventListener("click", swallow, { capture: true, once: true });
      setTimeout(() => document.removeEventListener("click", swallow, { capture: true }), 0);
    }
    drag = null;
  };
  document.addEventListener("pointerup", lift);
  document.addEventListener("pointercancel", lift);

  document.addEventListener("click", (e) => {
    const t = e.target;
    if (!t.closest) return;
    const n = t.closest(".dag-port.pz .node");
    if (n) {
      e.preventDefault(); // the panel, not the table row
      select(n.dataset.i, false, true);
      return;
    }
    const p = parts();
    if (!p) return;
    // a row's task name: its node, in view, and its details
    const r = t.closest('.tasks a[href^="#node-"]');
    if (r) {
      const i = r.getAttribute("href").slice(6);
      e.preventDefault();
      select(i, false, true);
      p.box.scrollIntoView({ block: "nearest" });
      const to = node(i);
      if (to) to.focus({ preventScroll: true });
      return;
    }
    const z = t.closest(".dag-tools [data-zoom]");
    if (z) {
      if (z.dataset.zoom === "fit") fit(p);
      else zoomBy(p, z.dataset.zoom === "in" ? 1.25 : 0.8);
      return;
    }
    if (t.closest(".dag-tools [data-pan]")) {
      free = !free;
      pan(p);
      return;
    }
    const c = t.closest(".dag-legend [data-chip]");
    if (c) {
      if (!chips.delete(c.dataset.chip)) chips.add(c.dataset.chip);
      filter(p, true);
      return;
    }
    if (t.closest(".wf-panel .close")) close();
  });

  document.addEventListener("keydown", (e) => {
    const t = e.target;
    if (!t.closest) return;
    if (e.key === "Escape" && t.closest(".dagbox") && open !== null) {
      e.preventDefault();
      close();
      return;
    }
    if (!t.closest(".dag-port.pz") || e.ctrlKey || e.metaKey || e.altKey) return;
    const n = t.closest(".node");
    // Enter: the task's job, as a link goes somewhere (its details when
    // it has no job page); Space: its details
    if (n && e.key === "Enter" && n.dataset.job) {
      e.preventDefault();
      location.assign(n.dataset.job);
      return;
    }
    if (n && (e.key === "Enter" || e.key === " ")) {
      e.preventDefault();
      select(n.dataset.i, true, true);
      return;
    }
    const p = parts();
    const step = 48;
    const keys = {
      "+": () => zoomBy(p, 1.25),
      "=": () => zoomBy(p, 1.25),
      "-": () => zoomBy(p, 0.8),
      0: () => fit(p),
      ArrowLeft: () => (view.x += step),
      ArrowRight: () => (view.x -= step),
      ArrowUp: () => (view.y += step),
      ArrowDown: () => (view.y -= step),
    };
    if (!keys[e.key]) return;
    e.preventDefault();
    keys[e.key]();
    apply(p);
  });

  // hover or focus a task, in the graph or its table row: trace it
  const taskOf = (el) => {
    if (!el || !el.closest) return null;
    const n = el.closest(".dag .node");
    if (n) return n.dataset.i;
    const r = el.closest(".tasks tbody tr[id]");
    return r ? r.id.slice(5) : null; // task-N
  };
  let hover = null; // the task under the pointer: a swap keeps its trace
  const traced = () => hover ?? taskOf(document.activeElement);
  document.addEventListener("pointerover", (e) => {
    const i = taskOf(e.target);
    if (i !== null) hover = i;
    if (i !== null && !drag && i !== marked) trace(i); // each child of a node: once
  });
  document.addEventListener("pointerout", (e) => {
    const i = taskOf(e.target);
    if (i === null || taskOf(e.relatedTarget) === i) return;
    hover = null;
    trace(traced());
  });
  document.addEventListener("focusin", (e) => {
    const i = taskOf(e.target);
    if (i === null) return;
    trace(i);
    // a task tabbed to outside the view: bring it in; not one a swap
    // focused again (htmx by id, or keyOf), or the view snaps back
    const p = parts();
    if (p && view && !swapping && e.target.matches(".dag .node:focus-visible")) show(p, e.target);
  });
  document.addEventListener("focusout", (e) => {
    if (taskOf(e.target) !== null && taskOf(e.relatedTarget) === null) trace(hover);
  });

  // keyOf names what had focus in #live, which a swap replaces, so the
  // same of the new #live can take focus back: [selector, index among its
  // matches]. A task, the graph's buttons, the panel, its close and its
  // links, the cancel buttons, the graph well; else a link or button by
  // its place under the nearest element with an id (a table row, the
  // rollback summary, the legend).
  function keyOf(el) {
    if (!el || !el.closest || !el.closest("#live")) return null;
    if (el.matches(".dag .node")) return [`.dag .node[data-i="${el.dataset.i}"]`, 0];
    if (el.matches("[data-zoom]")) return [`[data-zoom="${el.dataset.zoom}"]`, 0];
    if (el.matches("[data-pan]")) return ["[data-pan]", 0];
    if (el.matches(".wf-panel .close")) return [".wf-panel .close", 0];
    if (el.matches(".wf-panel a[href]")) return [`.wf-panel .body a[href="${CSS.escape(el.getAttribute("href"))}"]`, 0];
    if (el.closest(".wf-panel")) return [".wf-panel", 0];
    if (el.matches("#wfctl button")) return [el.name ? "#wfctl button[name]" : "#wfctl button:not([name])", 0];
    if (el.matches(".dag-port")) return [".dag-port", 0];
    const host = el.parentElement.closest("[id]");
    if (!host || host.id === "live" || !el.matches("a[href], button")) return null;
    const sel = `#${CSS.escape(host.id)} :is(a[href], button)`;
    return [sel, [...document.querySelectorAll("#live " + sel)].indexOf(el)];
  }
  let control = null;

  // the poll swaps #live: keep what had focus, take the new graph over
  // (init reopens the panel first, so its parts can take focus back)
  document.addEventListener("htmx:beforeSwap", (e) => {
    if (e.detail.shouldSwap === false) return; // no swap: no afterSettle to undo the flag
    control = keyOf(document.activeElement);
    swapping = true;
    const p = parts();
    const s = getSelection();
    panelTop = p ? p.panel.scrollTop : 0;
    kept = p && s.rangeCount && p.panel.contains(s.anchorNode) ? [s.anchorNode, s.anchorOffset, s.focusNode, s.focusOffset] : null;
  });
  document.addEventListener("htmx:afterSwap", () => {
    init();
    if (control) {
      const c = document.querySelectorAll("#live " + control[0])[control[1]];
      if (c && !c.closest("[hidden]")) c.focus({ preventScroll: true });
      control = null;
    }
    trace(traced());
  });
  // htmx settles a swapped element that has an id (a node, a row): its
  // class turns back to the server's after the swap, so the marks set
  // here (selected, filtered, traced) go on again
  document.addEventListener("htmx:afterSettle", (e) => {
    swapping = false; // every focus of the swap is done
    const p = parts();
    if (e.target.id !== "live" || !p || !p.svg.dataset.taken) return;
    filter(p);
    const n = open !== null && node(open);
    if (n) n.classList.add("sel");
    trace(traced());
  });

  init(); // deferred: the DOM is parsed
})();
