// The workflow page's script. The task graph (server-rendered SVG) pans and
// zooms in place: drag, Ctrl + scroll or pinch, the +, - and fit buttons,
// or keys on the focused graph (+ - 0, arrows). A task hovered or focused
// shows its upstream and downstream; a task clicked, or Enter on it, opens
// its details beside the graph (Escape closes). Without this script the
// graph scrolls and each task links to its table row.
//
// While the workflow runs #live polls and is swapped whole: the view, the
// open task and whatever had focus in it are kept across each swap.
(() => {
  const MIN = 0.2, MAX = 2.5;
  let view = null; // {k, x, y}: the pan and zoom of the graph shown
  let shown = ""; // the page the view is of
  let open = null; // the index of the task whose panel is open
  let height = ""; // the graph well's height, kept across swaps
  let drag = null; // the pointer drag under way
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

  // fit shows the whole graph, at most at its own size, centred. readable
  // (the first view): never under READ, so a wide graph on a phone opens at
  // its roots, the rest a drag away.
  const READ = 0.6;
  function fit(p, readable) {
    const w = +p.svg.dataset.w, h = +p.svg.dataset.h;
    const cw = p.port.clientWidth, ch = p.port.clientHeight;
    const whole = Math.min(1, cw / w, ch / h);
    const k = clamp(readable ? Math.max(READ, whole) : whole);
    view = { k, x: w * k > cw ? 0 : (cw - w * k) / 2, y: (ch - h * k) / 2 };
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

  // init takes over a graph just rendered: the first time a well as tall
  // as the graph needs (within bounds) and the whole graph in view; after a
  // swap, the view, panel and focus it had.
  function init() {
    const p = parts();
    // htmx fires afterSwap for the oob parts of a poll too: a graph taken
    // over already is left as it is (its panel, and the focus in it)
    if (!p || p.svg.dataset.taken) return;
    p.svg.dataset.taken = "1";
    p.port.classList.add("pz");
    p.svg.removeAttribute("viewBox"); // user units are px; .view pans and zooms
    p.box.querySelector(".dag-tools").hidden = false;
    const page = location.pathname + location.search;
    if (!view || shown !== page) {
      shown = page;
      const k = Math.max(READ, Math.min(1, p.port.clientWidth / +p.svg.dataset.w));
      const tall = Math.max(300, Math.min(560, window.innerHeight * 0.62, +p.svg.dataset.h * k + 24));
      p.port.style.height = tall + "px";
      fit(p, true);
    } else {
      p.port.style.height = height;
      apply(p);
    }
    height = p.port.style.height;
    if (open !== null) select(open, false);
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
  // edges between them; the rest of the graph fades. null clears it.
  function trace(i) {
    const p = parts();
    if (!p) return;
    const svg = p.svg;
    svg.querySelectorAll(".hl").forEach((el) => el.classList.remove("hl"));
    svg.classList.toggle("trace", i !== null);
    if (i === null) return;
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
    p.panel.querySelector(".body").innerHTML = src.innerHTML;
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

  // reveal brings task i into view in the well as it is now, the first
  // that reads (zoom at least READ): the whole graph; the task with all it
  // waits on and all that waits on it; the task and all that waits on it;
  // else the task centred, its neighbours either side.
  function reveal(p, i) {
    const cw = p.port.clientWidth, ch = p.port.clientHeight, pad = 16;
    const w = +p.svg.dataset.w, h = +p.svg.dataset.h;
    if (Math.min(1, cw / w, ch / h) >= READ) return fit(p);
    const l = lineage(p.svg, i);
    for (const ids of [l.tasks, l.down, new Set([String(i)])]) {
      let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
      for (const id of ids) {
        const n = node(id);
        const box = n && n.querySelector(".box");
        if (!box) continue;
        const [x, y] = n.getAttribute("transform").match(/[-\d.]+/g).map(Number);
        x0 = Math.min(x0, x);
        y0 = Math.min(y0, y);
        x1 = Math.max(x1, x + +box.getAttribute("width"));
        y1 = Math.max(y1, y + +box.getAttribute("height"));
      }
      if (x1 < x0) return;
      let k = Math.min(1, (cw - 2 * pad) / (x1 - x0), (ch - 2 * pad) / (y1 - y0));
      if (ids.size === 1) k = Math.max(READ, Math.min(view.k, 1)); // the task alone: as close as it is
      else if (k < READ) continue;
      view = { k, x: (cw - (x0 + x1) * k) / 2, y: (ch - (y0 + y1) * k) / 2 };
      return apply(p);
    }
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
      zoomBy(p, Math.exp(-e.deltaY * 0.002), ...at(p, e));
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
    const z = t.closest(".dag-tools [data-zoom]");
    if (z) {
      const p = parts();
      if (z.dataset.zoom === "fit") fit(p);
      else zoomBy(p, z.dataset.zoom === "in" ? 1.25 : 0.8);
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
    const n = t.closest(".dag-port.pz .node");
    if (n && (e.key === "Enter" || e.key === " ")) {
      e.preventDefault();
      select(n.dataset.i, true, true);
      return;
    }
    if (!t.matches(".dag-port.pz") || e.ctrlKey || e.metaKey || e.altKey) return;
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

  // hover or focus a task: trace it
  document.addEventListener("pointerover", (e) => {
    const n = e.target.closest && e.target.closest(".dag .node");
    if (n && !drag) trace(n.dataset.i);
  });
  document.addEventListener("pointerout", (e) => {
    const n = e.target.closest && e.target.closest(".dag .node");
    if (!n || (e.relatedTarget && n.contains(e.relatedTarget))) return;
    const f = document.activeElement && document.activeElement.closest && document.activeElement.closest(".dag .node");
    trace(f ? f.dataset.i : null);
  });
  document.addEventListener("focusin", (e) => {
    const n = e.target.closest && e.target.closest(".dag .node");
    if (n) trace(n.dataset.i);
  });
  document.addEventListener("focusout", (e) => {
    const n = e.target.closest && e.target.closest(".dag .node");
    if (n && !(e.relatedTarget && e.relatedTarget.closest && e.relatedTarget.closest(".dag .node"))) trace(null);
  });

  // keyOf names what had focus in #live, which a swap replaces, so the
  // same of the new #live can take focus back: a task, the zoom buttons,
  // the panel, its close and its links, the cancel buttons, the graph well.
  function keyOf(el) {
    if (!el || !el.closest || !el.closest("#live")) return null;
    if (el.matches(".dag .node")) return `.dag .node[data-i="${el.dataset.i}"]`;
    if (el.matches("[data-zoom]")) return `[data-zoom="${el.dataset.zoom}"]`;
    if (el.matches(".wf-panel .close")) return ".wf-panel .close";
    if (el.matches(".wf-panel a[href]")) return `.wf-panel .body a[href="${CSS.escape(el.getAttribute("href"))}"]`;
    if (el.closest(".wf-panel")) return ".wf-panel";
    if (el.matches("#wfctl button")) return el.name ? "#wfctl button[name]" : "#wfctl button:not([name])";
    if (el.matches(".dag-port")) return ".dag-port";
    return null;
  }
  let control = null;

  // the poll swaps #live: keep what had focus, take the new graph over
  // (init reopens the panel first, so its parts can take focus back)
  document.addEventListener("htmx:beforeSwap", () => {
    control = keyOf(document.activeElement);
  });
  document.addEventListener("htmx:afterSwap", () => {
    init();
    if (control) {
      const c = document.querySelector("#live " + control);
      if (c && !c.closest("[hidden]")) c.focus({ preventScroll: true });
      control = null;
    }
    const f = document.activeElement && document.activeElement.closest && document.activeElement.closest(".dag .node");
    if (f) trace(f.dataset.i);
  });

  init(); // deferred: the DOM is parsed
})();
