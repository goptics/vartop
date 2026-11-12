// Every page's script.
(() => {
  // Polls send back the hash pair of what the page shows (X-Seen), so the
  // server can answer 204, or the status alone, where nothing changed. Any
  // other htmx response may have changed the page: the pair is forgotten.
  let seen = "";
  const polls = (elt) => (elt.getAttribute("hx-trigger") || "").startsWith("every ");
  document.addEventListener("htmx:configRequest", (e) => {
    if (seen && polls(e.detail.elt)) e.detail.headers["X-Seen"] = seen;
  });
  document.addEventListener("htmx:afterRequest", (e) => {
    const xhr = e.detail.xhr;
    seen = (polls(e.detail.elt) && xhr && xhr.getResponseHeader("X-Seen")) || "";
  });
  document.addEventListener("htmx:historyRestore", () => { seen = ""; });

  // A poll's hx-select="#live" would filter an error reply to nothing, and
  // a failed send has no reply: either way the page would go on showing old
  // numbers as current. Say so in #errors instead; the next good poll
  // clears it.
  const errors = () => document.getElementById("errors");
  const pollFailed = (msg) => {
    const box = errors();
    if (!box) return;
    const p = document.createElement("p");
    p.className = "banner stale";
    p.textContent = msg;
    box.replaceChildren(p);
    box.dataset.poll = "1";
  };
  document.addEventListener("htmx:beforeSwap", (e) => {
    const d = e.detail;
    if (!polls(d.elt)) return;
    if (d.isError) {
      d.shouldSwap = false;
      // DOMParser runs no scripts or handlers; only the text is kept.
      const body = new DOMParser().parseFromString(d.xhr.responseText, "text/html").body;
      const why = body.textContent.trim().replace(/\s+/g, " ");
      pollFailed(`Refresh failed (HTTP ${d.xhr.status})${why ? ": " + why : ""}. The values shown may be old.`);
    } else if (errors() && errors().dataset.poll) {
      errors().replaceChildren();
      delete errors().dataset.poll;
    }
  });
  // #ctl (worker controls, dashboard bulk form) sits outside #live, so a
  // poll brings it out of band; it is swapped only when what it shows
  // changed (data-key), so a slider being dragged is not reset each poll.
  document.addEventListener("htmx:oobBeforeSwap", (e) => {
    const d = e.detail, next = d.fragment && d.fragment.firstElementChild;
    if (d.target.id === "ctl" && next && next.dataset.key === d.target.dataset.key) d.shouldSwap = false;
  });
  document.addEventListener("htmx:sendError", (e) => {
    if (polls(e.detail.elt)) pollFailed("Cannot reach vartop web. The values shown may be old.");
  });

  // Confirm panels: Keep, or Escape inside one, closes it and sends
  // nothing; focus goes back to the control that asked for it (htmx
  // focuses Keep, autofocus, when the panel comes in).
  let asker = null;
  document.addEventListener("htmx:beforeRequest", (e) => {
    if (!polls(e.detail.elt) && !e.detail.elt.closest(".confirm")) asker = e.detail.elt;
  });
  const keep = (panel) => {
    if (!panel) return;
    panel.remove();
    if (asker && asker.isConnected) asker.focus();
  };
  document.addEventListener("click", (e) => {
    const b = e.target.closest && e.target.closest("[data-keep]");
    if (b) keep(b.closest(".confirm"));
  });
  document.addEventListener("keydown", (e) => {
    const at = document.activeElement;
    if (e.key === "Escape" && at && at.closest) keep(at.closest(".confirm"));
  });
})();
