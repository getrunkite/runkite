const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

const source = fs.readFileSync(path.join(__dirname, "../site/product-tour.js"), "utf8");

function fixture(mutate = () => {}) {
  const state = { focus: null };
  function element(tagName, attributes = {}) {
    return {
      tagName, attributes: { ...attributes }, hidden: false, disabled: false,
      events: new Map(),
      getAttribute(name) { return this.attributes[name] ?? null; },
      setAttribute(name, value) { this.attributes[name] = String(value); },
      addEventListener(name, handler) { this.events.set(name, handler); },
      focus() { state.focus = this; },
    };
  }
  const controls = element("DIV");
  controls.hidden = true;
  const tabs = ["run", "review", "audit"].map(name => element("BUTTON", {
    id: `tour-${name}-tab`, "data-tour-tab": "", "aria-controls": `tour-${name}`,
  }));
  const panels = ["run", "review", "audit"].map(name => element("SECTION", {
    id: `tour-${name}`, "data-tour-panel": "",
  }));
  // The panel DOM order need not match the buttons' order.
  const root = element("SECTION", { id: "product-tour" });
  const data = { controls: [controls], tabs, panels, extraIds: [] };
  controls.contains = tab => tabs.includes(tab);
  root.querySelectorAll = selector => selector === "[data-tour-controls]" ? data.controls
    : selector === "[data-tour-tab]" ? data.tabs : data.panels;
  const document = {
    getElementById: id => id === "product-tour" ? root : null,
    querySelectorAll: () => [root, controls, ...data.tabs, ...data.panels, ...data.extraIds],
  };
  const page = { state, element, data, root, controls, tabs, panels, document };
  mutate(page);
  vm.runInNewContext(source, { document });
  page.click = index => tabs[index].events.get("click")();
  page.key = (index, key) => {
    let prevented = 0;
    tabs[index].events.get("keydown")({ key, preventDefault() { prevented++; } });
    return prevented;
  };
  return page;
}

function selected(page, index) {
  page.tabs.forEach((tab, position) => {
    assert.equal(tab.getAttribute("aria-selected"), String(position === index));
    assert.equal(tab.getAttribute("tabindex"), position === index ? "0" : "-1");
    const panel = page.panels.find(entry => entry.getAttribute("id") === tab.getAttribute("aria-controls"));
    assert.equal(panel.hidden, position !== index);
  });
}

function staticFallback(page) {
  assert.equal(page.root.getAttribute("data-enhanced"), null);
  assert.equal(page.controls.hidden, true);
  for (const panel of page.panels) {
    assert.equal(panel.hidden, false);
    assert.equal(panel.getAttribute("role"), null);
  }
  for (const tab of page.tabs) assert.equal(tab.events.size, 0);
}

test("valid gallery exposes named tabs and focusable panels with the first selected", () => {
  const page = fixture();
  assert.equal(page.root.getAttribute("data-enhanced"), "");
  assert.equal(page.controls.hidden, false);
  assert.equal(page.controls.getAttribute("role"), "tablist");
  assert.equal(page.controls.getAttribute("aria-label"), "Admin product views");
  selected(page, 0);
  page.tabs.forEach((tab, index) => {
    assert.equal(tab.getAttribute("role"), "tab");
    assert.equal(tab.getAttribute("type"), "button");
    assert.equal(page.panels[index].getAttribute("role"), "tabpanel");
    assert.equal(page.panels[index].getAttribute("aria-labelledby"), tab.getAttribute("id"));
    assert.equal(page.panels[index].getAttribute("tabindex"), "0");
  });
  assert.equal(page.state.focus, null, "enhancement must not steal page focus");
});

test("click selects the mapped panel, including a different panel DOM order", () => {
  const page = fixture(p => p.data.panels.reverse());
  page.click(1);
  selected(page, 1);
  assert.equal(page.state.focus, null);
  page.click(2);
  selected(page, 2);
});

test("arrow keys wrap, select, focus and prevent their native scrolling", () => {
  const page = fixture();
  for (const [from, key, to] of [[0, "ArrowLeft", 2], [2, "ArrowRight", 0], [0, "ArrowRight", 1], [1, "ArrowLeft", 0]]) {
    assert.equal(page.key(from, key), 1);
    selected(page, to);
    assert.equal(page.state.focus, page.tabs[to]);
  }
});

test("Home and End select the boundary tabs and focus them", () => {
  const page = fixture();
  for (const [from, key, to] of [[0, "End", 2], [2, "Home", 0]]) {
    assert.equal(page.key(from, key), 1);
    selected(page, to);
    assert.equal(page.state.focus, page.tabs[to]);
  }
});

test("unhandled keys keep native button activation, focus and tab behavior", () => {
  const page = fixture();
  for (const key of ["Enter", " ", "Tab", "Escape", "ArrowUp", "ArrowDown", "x"]) {
    assert.equal(page.key(0, key), 0);
    selected(page, 0);
    assert.equal(page.state.focus, null);
  }
  page.click(1);
  selected(page, 1);
});

test("two mapped panels are sufficient", () => {
  const page = fixture(p => { p.tabs.pop(); p.panels.pop(); });
  selected(page, 0);
  assert.equal(page.controls.hidden, false);
  page.key(0, "ArrowLeft");
  selected(page, 1);
});

test("mapping failures preserve all static panels and hidden controls", () => {
  const cases = {
    "missing controls": p => { p.data.controls = []; },
    "duplicate controls": p => { p.data.controls.push(p.element("DIV")); },
    "single view": p => { p.tabs.splice(1); p.panels.splice(1); },
    "unmapped panel": p => { p.tabs.pop(); },
    "missing panel": p => { p.panels.pop(); },
    "missing target": p => { delete p.tabs[1].attributes["aria-controls"]; },
    "unknown target": p => { p.tabs[1].attributes["aria-controls"] = "foreign"; },
    "reused target": p => { p.tabs[1].attributes["aria-controls"] = "tour-run"; },
    "missing tab ID": p => { delete p.tabs[1].attributes.id; },
    "duplicate tab ID": p => { p.tabs[1].attributes.id = p.tabs[0].attributes.id; },
    "duplicate panel ID": p => { p.panels[1].attributes.id = p.panels[0].attributes.id; },
    "external ID collision": p => { p.data.extraIds.push(p.element("DIV", { id: "tour-run" })); },
    "whitespace ID": p => { p.tabs[1].attributes.id = "two words"; },
    "nonbutton tab": p => { p.tabs[1].tagName = "A"; },
    "nonsection panel": p => { p.panels[1].tagName = "DIV"; },
    "disabled tab": p => { p.tabs[1].disabled = true; },
    "tab outside controls": p => { p.controls.contains = tab => tab !== p.tabs[1]; },
  };
  for (const [name, mutate] of Object.entries(cases)) {
    assert.doesNotThrow(() => staticFallback(fixture(mutate)), name);
  }
});

test("missing DOM APIs or absent gallery leave the static document alone", () => {
  for (const mutate of [
    p => { p.document.getElementById = undefined; },
    p => { p.document.querySelectorAll = undefined; },
    p => { p.document.getElementById = () => null; },
    p => { p.root.querySelectorAll = undefined; },
    p => { p.root.setAttribute = undefined; },
    p => { p.controls.contains = undefined; },
    p => { p.controls.setAttribute = undefined; },
    p => { p.tabs[1].addEventListener = undefined; },
    p => { p.tabs[1].focus = undefined; },
    p => { p.panels[1].setAttribute = undefined; },
    p => { p.root.querySelectorAll = () => { throw new Error("unavailable"); }; },
  ]) assert.doesNotThrow(() => staticFallback(fixture(mutate)));
  assert.doesNotThrow(() => vm.runInNewContext(source, {}));
});

test("authored gallery keeps all three named image views readable without JavaScript", () => {
  const html = fs.readFileSync(path.join(__dirname, "../site/index.html"), "utf8");
  const controls = html.match(/<[^>]+\bdata-tour-controls\b[^>]*>/g) || [];
  assert.equal(controls.length, 1);
  assert.match(controls[0], /\shidden(?:\s|>|=)/);
  const tabs = [...html.matchAll(/<button\b([^>]*\bdata-tour-tab\b[^>]*)>/g)];
  const panels = [...html.matchAll(/<section\b([^>]*\bdata-tour-panel\b[^>]*)>([\s\S]*?)<\/section>/g)];
  assert.equal(tabs.length, 3);
  assert.equal(panels.length, 3);
  for (const name of ["run", "review", "audit"]) {
    const panel = panels.find(match => match[1].includes(`id="tour-${name}"`));
    assert.ok(panel, name);
    assert.doesNotMatch(panel[1], /\shidden(?:\s|$|=)/);
    assert.match(panel[2], /<h3\b/);
    assert.match(panel[2], /<img\b[^>]*src="[^"?#]+\.(?:png|webp|jpe?g)"/);
    assert.equal(tabs.filter(match => match[1].includes(`aria-controls="tour-${name}"`)).length, 1);
  }
});
