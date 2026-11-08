/* Both views remain readable when JavaScript or SVG motion is unavailable. */
(function () {
  "use strict";
  var root = document.querySelector(".execution-flow");
  if (!root) return;
  var panels = Array.from(root.querySelectorAll(".execution-panel"));
  var choices = Array.from(root.querySelectorAll(".execution-view-button"));
  var controls = root.querySelector(".execution-controls");
  var toggle = root.querySelector(".execution-toggle");
  if (!panels.length || choices.length !== panels.length || !controls || !toggle) return;

  var reduced = typeof window.matchMedia === "function"
    ? window.matchMedia("(prefers-reduced-motion: reduce)") : { matches: true };
  var desktop = typeof window.matchMedia === "function"
    ? window.matchMedia("(min-width: 1001px)") : { matches: false };
  var requested = !reduced.matches && desktop.matches;
  var inView = !window.IntersectionObserver;
  var current = 0;
  var motion = panels.map(function (panel) {
    var svg = panel.querySelector(".execution-wires");
    var animations = svg ? Array.from(svg.querySelectorAll("animateMotion")) : [];
    var capable = svg && animations.length && typeof svg.pauseAnimations === "function" &&
      typeof svg.unpauseAnimations === "function" && animations.every(function (animation) {
        return typeof animation.beginElement === "function";
      });
    if (capable) {
      try {
        svg.pauseAnimations();
        animations.forEach(function (animation) { animation.beginElement(); });
        panel.classList.add("execution-ready");
      } catch (error) {
        capable = false;
      }
    }
    return { svg: svg, capable: Boolean(capable) };
  });

  function update() {
    var active = motion[current];
    var playing = requested && active.capable && desktop.matches && inView && !document.hidden;
    motion.forEach(function (entry, index) {
      if (!entry.capable) return;
      if (playing && index === current) entry.svg.unpauseAnimations();
      else entry.svg.pauseAnimations();
    });
    toggle.hidden = !active.capable || !desktop.matches;
    toggle.textContent = playing ? "Pause flow" : "Play flow";
    toggle.setAttribute("aria-pressed", String(playing));
  }

  function select(index) {
    current = index;
    panels.forEach(function (panel, position) { panel.hidden = position !== index; });
    choices.forEach(function (choice, position) {
      choice.setAttribute("aria-pressed", String(position === index));
    });
    root.setAttribute("data-execution-view", index === 0 ? "protocol" : "native");
    update();
  }

  choices.forEach(function (choice, index) {
    choice.addEventListener("click", function () { select(index); });
  });
  toggle.addEventListener("click", function () { requested = !requested; update(); });
  if (typeof reduced.addEventListener === "function") {
    reduced.addEventListener("change", function () {
      if (reduced.matches) requested = false;
      update();
    });
  }
  if (typeof desktop.addEventListener === "function") desktop.addEventListener("change", update);
  document.addEventListener("visibilitychange", update);
  if (window.IntersectionObserver) {
    new IntersectionObserver(function (entries) {
      inView = entries[0].isIntersecting;
      update();
    }, { threshold: 0.1 }).observe(root);
  }
  controls.hidden = false;
  root.classList.add("execution-enhanced");
  select(0);
})();
