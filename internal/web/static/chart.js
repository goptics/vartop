// Hover readout for the 1h charts (.history): a crosshair at the poll under
// the pointer and its text, at once, in place of the browser's delayed
// tooltip. The text is the hover target's <title>, which stays the tooltip
// where this script does not run. Polls replace the charts every few
// seconds, so the readout is drawn again on each swap, before it is shown.
(() => {
  let pointer = null; // the last pointer position over a chart

  function hide(chart) {
    chart.classList.remove("hovering");
    chart.querySelector(".tip").hidden = true;
  }

  // Moves each target's title into data-tip, so the browser's own tooltip
  // does not show on top of the readout.
  function untitle(svg) {
    if (svg.dataset.untitled) return;
    for (const hit of svg.querySelectorAll(".hit")) {
      const title = hit.querySelector("title");
      if (title) {
        hit.dataset.tip = title.textContent;
        title.remove();
      }
    }
    svg.dataset.untitled = "1";
  }

  function show(chart, clientX) {
    const svg = chart.querySelector("svg");
    const ctm = svg.getScreenCTM();
    const hits = svg.querySelectorAll(".hit");
    if (!hits.length || !ctm) return hide(chart);
    untitle(svg);

    // The pointer in plot units, and the target nearest to it: none where
    // the nearest is more than two targets away, over a stretch with no
    // poll at all (before the history starts, or while nothing collected).
    const x = new DOMPoint(clientX, 0).matrixTransform(ctm.inverse()).x;
    let best = null, bestX = 0, gap = Infinity;
    for (const hit of hits) {
      const mid = +hit.getAttribute("x") + +hit.getAttribute("width") / 2;
      if (Math.abs(mid - x) < gap) {
        gap = Math.abs(mid - x);
        best = hit;
        bestX = mid;
      }
    }
    if (gap > 2 * +best.getAttribute("width")) return hide(chart);

    const cross = svg.querySelector(".cross");
    cross.setAttribute("x1", bestX);
    cross.setAttribute("x2", bestX);
    chart.classList.add("hovering");

    // The text beside the crosshair, flipped to its left near the right
    // edge, and kept over the plot, clear of the scale's labels.
    const tip = chart.querySelector(".tip");
    tip.textContent = best.dataset.tip || "";
    tip.hidden = false;
    const box = chart.getBoundingClientRect();
    const plot = svg.getBoundingClientRect();
    const vb = svg.viewBox.baseVal;
    const at = plot.left + ((bestX - vb.x) / vb.width) * plot.width - box.left;
    const right = at + 8;
    const left = right + tip.offsetWidth > box.width ? at - 8 - tip.offsetWidth : right;
    tip.style.left = Math.max(plot.left - box.left, Math.min(left, box.width - tip.offsetWidth)) + "px";
  }

  // A tap shows the readout too: touch has no hover, and no move without a
  // drag.
  function track(e) {
    const chart = e.target.closest && e.target.closest(".history");
    for (const other of document.querySelectorAll(".history.hovering")) {
      if (other !== chart) hide(other);
    }
    pointer = chart ? { x: e.clientX, y: e.clientY } : null;
    if (chart) show(chart, e.clientX);
  }
  document.addEventListener("pointermove", track);
  document.addEventListener("pointerdown", track);

  // A finger lifted leaves the page too; its readout stays until the next
  // tap elsewhere.
  document.documentElement.addEventListener("pointerleave", (e) => {
    if (e.pointerType === "touch") return;
    pointer = null;
    for (const chart of document.querySelectorAll(".history.hovering")) hide(chart);
  });

  // afterSwap runs before the browser paints the new charts (afterSettle
  // runs ~20ms later), so the readout does not blink on each poll.
  document.addEventListener("htmx:afterSwap", () => {
    if (!pointer) return;
    const el = document.elementFromPoint(pointer.x, pointer.y);
    const chart = el && el.closest(".history");
    if (chart) show(chart, pointer.x);
  });
})();
