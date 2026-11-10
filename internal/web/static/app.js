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
})();
