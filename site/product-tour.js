/* Keep every product view readable unless the complete tab set can be enhanced. */
(function () {
  "use strict";
  if (typeof document === "undefined" || typeof document.getElementById !== "function" ||
      typeof document.querySelectorAll !== "function") return;
  var root = document.getElementById("product-tour");
  if (!root || typeof root.querySelectorAll !== "function" || typeof root.setAttribute !== "function") return;

  var controls, tabs, panels, orderedPanels;
  try {
    var controlSets = Array.prototype.slice.call(root.querySelectorAll("[data-tour-controls]"));
    if (controlSets.length !== 1) return;
    controls = controlSets[0];
    tabs = Array.prototype.slice.call(root.querySelectorAll("[data-tour-tab]"));
    panels = Array.prototype.slice.call(root.querySelectorAll("[data-tour-panel]"));
    if (tabs.length < 2 || tabs.length !== panels.length ||
        typeof controls.setAttribute !== "function" || typeof controls.contains !== "function") return;

    var identified = Array.prototype.slice.call(document.querySelectorAll("[id]"));
    function uniqueId(element) {
      if (typeof element.getAttribute !== "function") return false;
      var id = element.getAttribute("id");
      return Boolean(id) && !/[\t\n\f\r ]/.test(id) && identified.filter(function (entry) {
        return entry.getAttribute("id") === id;
      }).length === 1;
    }
    if (!panels.every(function (panel) {
      return panel.tagName === "SECTION" && uniqueId(panel) &&
        typeof panel.setAttribute === "function";
    })) return;

    orderedPanels = tabs.map(function (tab) {
      if (tab.tagName !== "BUTTON" || tab.disabled || !uniqueId(tab) || !controls.contains(tab) ||
          typeof tab.setAttribute !== "function" || typeof tab.addEventListener !== "function" ||
          typeof tab.focus !== "function") return null;
      var matches = panels.filter(function (panel) {
        return panel.getAttribute("id") === tab.getAttribute("aria-controls");
      });
      return matches.length === 1 ? matches[0] : null;
    });
    if (orderedPanels.some(function (panel, index) {
      return !panel || orderedPanels.indexOf(panel) !== index;
    })) return;
  } catch (error) {
    return;
  }

  function select(index, focus) {
    tabs.forEach(function (tab, position) {
      var selected = position === index;
      tab.setAttribute("aria-selected", String(selected));
      tab.setAttribute("tabindex", selected ? "0" : "-1");
      orderedPanels[position].hidden = !selected;
    });
    if (focus) tabs[index].focus();
  }

  controls.setAttribute("role", "tablist");
  controls.setAttribute("aria-label", "Admin product views");
  tabs.forEach(function (tab, index) {
    tab.setAttribute("role", "tab");
    // Keep a tab click from submitting if the gallery is later placed in a form.
    tab.setAttribute("type", "button");
    orderedPanels[index].setAttribute("role", "tabpanel");
    orderedPanels[index].setAttribute("aria-labelledby", tab.getAttribute("id"));
    orderedPanels[index].setAttribute("tabindex", "0");
    tab.addEventListener("click", function () { select(index, false); });
    tab.addEventListener("keydown", function (event) {
      var next;
      switch (event.key) {
        case "ArrowRight": next = (index + 1) % tabs.length; break;
        case "ArrowLeft": next = (index + tabs.length - 1) % tabs.length; break;
        case "Home": next = 0; break;
        case "End": next = tabs.length - 1; break;
        default: return;
      }
      event.preventDefault();
      select(next, true);
    });
  });
  select(0, false);
  controls.hidden = false;
  root.setAttribute("data-enhanced", "");
})();
