/* The page is readable without JavaScript. Animation is explicitly opt-in. */
(function () {
  "use strict";
  var toggle = document.getElementById("walkthrough-toggle");
  var image = document.getElementById("product-image");
  if (!toggle || !image) return;

  var still = image.getAttribute("src");
  var stillAlt = image.getAttribute("alt");
  var playing = false;
  toggle.hidden = false;
  toggle.addEventListener("click", function () {
    playing = !playing;
    image.src = playing ? "./assets/admin-walkthrough.gif" : still;
    image.alt = playing
      ? "Runkite Admin walkthrough using sample data: overview, agents, runs, connectors, approvals and audit."
      : stillAlt;
    toggle.setAttribute("aria-pressed", String(playing));
    toggle.textContent = playing ? "Pause walkthrough ■" : "Play walkthrough ▶";
  });
  image.addEventListener("error", function () {
    if (!playing) return;
    playing = false;
    image.src = still;
    image.alt = stillAlt;
    toggle.setAttribute("aria-pressed", "false");
    toggle.textContent = "Walkthrough unavailable";
    toggle.disabled = true;
  });
})();
