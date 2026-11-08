/* Autoplay respects reduced motion; the still remains usable without JavaScript. */
(function () {
  "use strict";
  var toggle = document.getElementById("walkthrough-toggle");
  var image = document.getElementById("product-image");
  if (!toggle || !image) return;

  var still = image.getAttribute("src");
  var stillAlt = image.getAttribute("alt");
  var reduced = typeof window.matchMedia === "function"
    ? window.matchMedia("(prefers-reduced-motion: reduce)") : { matches: true };
  var playing = !reduced.matches;
  var unavailable = false;

  function render() {
    image.src = playing ? "./assets/admin-walkthrough.gif" : still;
    image.alt = playing
      ? "Runkite Admin walkthrough using sample data: overview, agents, runs, connectors, approvals and audit."
      : stillAlt;
    toggle.setAttribute("aria-pressed", String(playing));
    toggle.textContent = unavailable ? "Walkthrough unavailable"
      : playing ? "Pause walkthrough ■" : "Play walkthrough ▶";
    toggle.disabled = unavailable;
  }

  toggle.hidden = false;
  toggle.addEventListener("click", function () {
    if (unavailable) return;
    playing = !playing;
    render();
  });
  image.addEventListener("error", function () {
    if (!playing) return;
    playing = false;
    unavailable = true;
    render();
  });
  if (typeof reduced.addEventListener === "function") {
    reduced.addEventListener("change", function () {
      if (!reduced.matches) return;
      playing = false;
      render();
    });
  }
  render();
})();
