const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

const source = fs.readFileSync(path.join(__dirname, "../site/landing.js"), "utf8");

function element(attributes = {}) {
  const events = new Map();
  return {
    hidden: false, disabled: false, events, attributes: { ...attributes },
    getAttribute(name) { return this.attributes[name] ?? null; },
    setAttribute(name, value) { this.attributes[name] = String(value); },
    addEventListener(name, listener) { events.set(name, listener); },
  };
}

function walkthrough(options = {}) {
  const toggle = element();
  toggle.hidden = true;
  const image = element({ src: "./assets/ui-runs.png", alt: "Original sample-data still" });
  image.src = image.getAttribute("src");
  image.alt = image.getAttribute("alt");
  const reduced = { ...element(), matches: Boolean(options.reduced) };
  const window = options.noMedia ? {} : { matchMedia: () => reduced };
  const document = { getElementById: id => options.missing ? null : id === "walkthrough-toggle" ? toggle : image };
  vm.runInNewContext(source, { window, document });
  return {
    toggle, image, reduced,
    click() { toggle.events.get("click")(); },
    error() { image.events.get("error")(); },
    preference(value) { reduced.matches = value; reduced.events.get("change")(); },
  };
}

test("walkthrough starts playing with a visible Pause control", () => {
  const page = walkthrough();
  assert.equal(page.image.src, "./assets/admin-walkthrough.gif");
  assert.equal(page.toggle.hidden, false);
  assert.equal(page.toggle.getAttribute("aria-pressed"), "true");
  assert.match(page.toggle.textContent, /^Pause walkthrough/);
  assert.match(page.image.alt, /sample data/);
});

test("Pause restores the original image and Play restarts the walkthrough", () => {
  const page = walkthrough();
  page.click();
  assert.equal(page.image.src, "./assets/ui-runs.png");
  assert.equal(page.image.alt, "Original sample-data still");
  assert.equal(page.toggle.getAttribute("aria-pressed"), "false");
  page.click();
  assert.equal(page.image.src, "./assets/admin-walkthrough.gif");
  assert.equal(page.toggle.getAttribute("aria-pressed"), "true");
});

test("reduced motion starts static and still allows an explicit Play", () => {
  const page = walkthrough({ reduced: true });
  assert.equal(page.image.src, "./assets/ui-runs.png");
  assert.equal(page.toggle.getAttribute("aria-pressed"), "false");
  page.click();
  assert.equal(page.image.src, "./assets/admin-walkthrough.gif");
});

test("enabling reduced motion pauses and disabling it preserves that pause", () => {
  const page = walkthrough();
  page.preference(true);
  assert.equal(page.image.src, "./assets/ui-runs.png");
  page.preference(false);
  assert.equal(page.toggle.getAttribute("aria-pressed"), "false");
  page.click();
  assert.equal(page.image.src, "./assets/admin-walkthrough.gif");
});

test("a failed animation restores the still and exposes an unavailable control", () => {
  const page = walkthrough();
  page.error();
  assert.equal(page.image.src, "./assets/ui-runs.png");
  assert.equal(page.image.alt, "Original sample-data still");
  assert.equal(page.toggle.getAttribute("aria-pressed"), "false");
  assert.equal(page.toggle.textContent, "Walkthrough unavailable");
  assert.equal(page.toggle.disabled, true);
  page.error();
  assert.equal(page.toggle.textContent, "Walkthrough unavailable");
  page.preference(true);
  page.preference(false);
  assert.equal(page.toggle.textContent, "Walkthrough unavailable");
  assert.equal(page.toggle.disabled, true);
});

test("an error from a paused image does not disable explicit playback", () => {
  const page = walkthrough({ reduced: true });
  page.error();
  assert.equal(page.toggle.disabled, false);
  page.click();
  assert.equal(page.image.src, "./assets/admin-walkthrough.gif");
});

test("missing markup and missing media-query support retain readable stills", () => {
  assert.doesNotThrow(() => walkthrough({ missing: true }));
  const page = walkthrough({ noMedia: true });
  assert.equal(page.image.src, "./assets/ui-runs.png");
  assert.equal(page.toggle.getAttribute("aria-pressed"), "false");
  page.click();
  assert.equal(page.image.src, "./assets/admin-walkthrough.gif");
});
