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
