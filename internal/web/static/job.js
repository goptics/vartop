// The job page's script: ticks the spans that run on (a waiting job's
// wait, a running job's run, their totals). The server renders each as it
// was then, with its ms in data-ms; this adds the time since the page
// loaded, by the browser's monotonic clock, so a skewed clock does not
// matter. Without it the page shows the server's values.
(() => {
  const live = document.querySelectorAll(".dur[data-ms]");
  if (!live.length) return;
  const t0 = performance.now();
  const pad = (n) => String(n).padStart(2, "0");
  // as span in workflows.go
  const fmt = (ms) => {
    const s = ms / 1000;
    if (s < 10) return s.toFixed(1) + "s";
    if (s < 60) return Math.floor(s) + "s";
    const m = Math.floor(s / 60), h = Math.floor(s / 3600);
    if (s < 3600) return m + "m " + pad(Math.floor(s) % 60) + "s";
    if (s < 86400) return h + "h " + pad(m % 60) + "m";
    return Math.floor(h / 24) + "d " + (h % 24) + "h";
  };
  const tick = () => {
    const d = performance.now() - t0;
    for (const el of live) el.textContent = fmt(Number(el.dataset.ms) + d);
  };
  setInterval(tick, 1000);
})();
