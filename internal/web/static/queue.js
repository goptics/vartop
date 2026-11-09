// The queue page's script: the copy button of the full Queue ID, shown
// only where the browser can copy.
(() => {
  if (!navigator.clipboard) return;
  for (const b of document.querySelectorAll("button[data-copy]")) {
    b.hidden = false;
    b.addEventListener("click", async () => {
      try {
        await navigator.clipboard.writeText(b.dataset.copy);
        b.textContent = "Copied";
      } catch {
        b.textContent = "Copy failed";
      }
      setTimeout(() => { b.textContent = "Copy ID"; }, 1500);
    });
  }
})();
