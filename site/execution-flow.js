(function () {
  "use strict";
  var root = document.querySelector(".execution-flow");
  if (!root) return;
  var svg = root.querySelector(".execution-wires");
  var button = root.querySelector(".execution-toggle");
  var animations = svg && svg.querySelectorAll("animateMotion");
  if (!button || !svg || !animations.length || typeof svg.pauseAnimations !== "function" ||
      typeof svg.unpauseAnimations !== "function" ||
      Array.from(animations).some(function (animation) { return typeof animation.beginElement !== "function"; })) return;
  var reduced = window.matchMedia("(prefers-reduced-motion: reduce)");
  var desktop = window.matchMedia("(min-width: 1001px)");
  var requested = !reduced.matches && desktop.matches;
  var inView = !window.IntersectionObserver;
  function update() {
    var playing = requested && desktop.matches && inView && !document.hidden;
    if (playing) svg.unpauseAnimations();
    else svg.pauseAnimations();
    button.textContent = playing ? "Pause flow" : "Play flow";
    button.setAttribute("aria-pressed", String(playing));
  }
  svg.pauseAnimations();
  animations.forEach(function (animation) { animation.beginElement(); });
  root.classList.add("execution-ready");
  button.hidden = false;
  button.addEventListener("click", function () { requested = !requested; update(); });
  reduced.addEventListener("change", function () { if (reduced.matches) requested = false; update(); });
  desktop.addEventListener("change", update);
  document.addEventListener("visibilitychange", update);
  if (window.IntersectionObserver) {
    new IntersectionObserver(function (entries) {
      inView = entries[0].isIntersecting;
      update();
    }, { threshold: 0.1 }).observe(root);
  }
  update();
})();
