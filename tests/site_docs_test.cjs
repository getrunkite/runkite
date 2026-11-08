const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

const source = fs.readFileSync(path.join(__dirname, "../site/support/docs.js"), "utf8");

function handbook(options = {}) {
  const state = { copied: [], fallbackCalls: 0, documentScroll: 987, ancestorScrolls: 0, timers: new Map() };
  let timerId = 0;
  let document;
  class Element {
    constructor(tag, text = "") {
      this.tagName = tag.toUpperCase();
      this.textContent = text;
      this.className = "";
      this.children = [];
      this.attrs = {};
      this.listeners = {};
      this.style = {};
      this.disabled = false;
      this.scrollTop = this.scrollLeft = 0;
      this.clientWidth = this.scrollWidth = 300;
      this.clientHeight = this.scrollHeight = 600;
      this.rect = { left: 0, right: 300, top: 0, bottom: 600 };
      this.classList = {
        contains: (name) => this.className.split(/\s+/).includes(name),
        add: (...names) => { this.className = [...new Set([...this.className.split(/\s+/), ...names])].filter(Boolean).join(" "); },
        remove: (...names) => { this.className = this.className.split(/\s+/).filter((n) => !names.includes(n)).join(" "); },
      };
    }
    get parentElement() { return this.parentNode; }
    get firstChild() { return this.children[0] || null; }
    get nextSibling() { return this.parentNode?.children[this.parentNode.children.indexOf(this) + 1] || null; }
    appendChild(child) { return this.insertBefore(child, null); }
    insertBefore(child, before) {
      if (child.parentNode) child.parentNode.removeChild(child);
      this.children.splice(before ? this.children.indexOf(before) : this.children.length, 0, child);
      child.parentNode = this;
      return child;
    }
    removeChild(child) { this.children.splice(this.children.indexOf(child), 1); child.parentNode = null; }
    setAttribute(key, value) { this.attrs[key] = String(value); }
    getAttribute(key) { return this.attrs[key] ?? (key === "href" ? this.href : null) ?? null; }
    removeAttribute(key) { delete this.attrs[key]; }
    addEventListener(type, listener) { (this.listeners[type] ||= []).push(listener); }
    querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
    querySelectorAll(selector) {
      const found = [];
      const matches = (el) => selector === "pre" ? el.tagName === "PRE"
        : selector === "nav" ? el.tagName === "NAV"
          : selector === "a.active" ? el.tagName === "A" && el.classList.contains("active")
            : selector === "a[href]" ? el.tagName === "A" && el.getAttribute("href") !== null
              : selector === "[data-live-admin]" && el.getAttribute("data-live-admin") !== null;
      function visit(node) { for (const child of node.children) { if (matches(child)) found.push(child); visit(child); } }
      visit(this);
      return found;
    }
    getBoundingClientRect() { return this.rectProvider ? this.rectProvider() : this.rect; }
    focus(options) { document.activeElement = this; this.lastFocusOptions = options; }
    select() { this.selected = true; }
    scrollIntoView() { state.ancestorScrolls++; state.documentScroll = 0; }
  }
  const body = new Element("body");
  const head = new Element("head");
  const sidebar = body.appendChild(new Element("aside"));
  const nav = sidebar.appendChild(new Element("nav"));
  const active = nav.appendChild(new Element("a", "SDK integrations"));
  active.className = "active";
  active.setAttribute("href", "sdk-integration.html");
  sidebar.scrollHeight = 1200;
  nav.scrollWidth = 1200;
  const activeTop = options.activeTop ?? 900;
  const activeLeft = options.activeLeft ?? 600;
  active.rectProvider = () => ({ top: activeTop - sidebar.scrollTop, bottom: activeTop + 40 - sidebar.scrollTop,
    left: activeLeft - nav.scrollLeft, right: activeLeft + 100 - nav.scrollLeft });
  const topbar = body.appendChild(new Element("div"));
  const product = topbar.appendChild(new Element("a", "Product"));
  product.setAttribute("href", "../");
  const main = body.appendChild(new Element("main"));
  const pre = main.appendChild(new Element("pre", "runkite dev --config examples/echo_agent/langgraph.json"));
  pre.className = "mono-block";
  const pending = new Element("td", "Not yet");
  const storage = new Map(Object.entries(options.saved || {}));
  document = {
    readyState: "complete", body, head, activeElement: body,
    createElement: (tag) => new Element(tag),
    querySelectorAll: (selector) => selector === "pre.mono-block" ? [pre] : selector === "table.matrix td" ? [pending] : [],
    querySelector: (selector) => selector === ".sidebar" ? sidebar : selector === ".sidebar nav" ? nav : selector === ".topbar" ? topbar : null,
    getElementById: (id) => [...head.children, ...body.children].find((el) => el.id === id) || null,
    execCommand: () => { state.fallbackCalls++; if (state.fallbackThrows) throw new Error("copy unavailable"); return state.fallbackResult; },
  };
  state.fallbackResult = options.fallbackResult ?? false;
  state.fallbackThrows = options.fallbackThrows ?? false;
  const context = {
    document,
    window: { addEventListener() {}, getComputedStyle: () => ({ scrollPaddingLeft: "16px", scrollPaddingRight: "32px" }) },
    navigator: { clipboard: options.clipboard ? { writeText: (text) => { state.copied.push(text); return options.clipboard(text); } } : undefined },
    location: { hostname: "getrunkite.com", hash: "#managed" },
    setTimeout: (fn) => { state.timers.set(++timerId, fn); return timerId; },
    clearTimeout: (id) => state.timers.delete(id),
  };
  Object.defineProperty(context, "sessionStorage", { get() {
    if (options.blockedStorage) throw new Error("SecurityError");
    return {
      getItem: (key) => storage.get(key) ?? null,
      setItem: (key, value) => { if (options.blockedWrites) throw new Error("QuotaExceededError"); storage.set(key, value); },
    };
  } });
  vm.runInNewContext(source, context, { filename: "site/support/docs.js" });
  const button = pre.parentElement.children.find((el) => el.tagName === "BUTTON");
  const fire = (element, type) => { for (const fn of element.listeners[type] || []) fn({ currentTarget: element }); };
  return { state, document, sidebar, nav, active, pre, pending, topbar, button, storage, fire,
    click: async () => { button.focus(); fire(button, "click"); await new Promise(setImmediate); },
    resetFeedback: () => { for (const fn of state.timers.values()) fn(); state.timers.clear(); } };
}

test("failed legacy copy never reports success and restores keyboard focus", async () => {
  const page = handbook({ fallbackResult: false });
  await page.click();
  assert.equal(page.button.textContent, "Copy failed");
  assert.equal(page.button.disabled, false);
  assert.equal(page.button.classList.contains("is-copied"), false);
  assert.equal(page.document.activeElement, page.button);
  assert.equal(page.button.lastFocusOptions.preventScroll, true);
  assert.equal(page.document.body.children.some((el) => el.tagName === "TEXTAREA"), false);
});

test("a thrown legacy copy also reports failure without leaving a textarea", async () => {
  const page = handbook({ fallbackThrows: true });
  await page.click();
  assert.equal(page.button.textContent, "Copy failed");
  assert.equal(page.document.body.children.some((el) => el.tagName === "TEXTAREA"), false);
});

test("rejected clipboard permission and failed fallback remain a visible failure", async () => {
  const page = handbook({ clipboard: () => Promise.reject(new Error("NotAllowedError")) });
  await page.click();
  assert.equal(page.state.copied[0], page.pre.textContent);
  assert.equal(page.state.fallbackCalls, 1);
  assert.equal(page.button.textContent, "Copy failed");
  assert.match(page.button.getAttribute("aria-label"), /manually/);
});

test("successful clipboard writes report success without fallback", async () => {
  const page = handbook({ clipboard: () => Promise.resolve() });
  await page.click();
  assert.equal(page.button.textContent, "Copied");
  assert.equal(page.state.fallbackCalls, 0);
  assert.equal(page.button.getAttribute("aria-label"), "Copied code");
  page.resetFeedback();
  assert.equal(page.button.textContent, "Copy");
  assert.equal(page.button.getAttribute("aria-label"), "Copy code");
});

test("a failed fallback can be retried successfully", async () => {
  const page = handbook();
  await page.click();
  page.state.fallbackResult = true;
  await page.click();
  assert.equal(page.button.textContent, "Copied");
  assert.equal(page.button.classList.contains("is-error"), false);
});

test("a pending clipboard write keeps focus and ignores duplicate clicks", async () => {
  let finish;
  const page = handbook({ clipboard: () => new Promise((resolve) => { finish = resolve; }) });
  await page.click();
  await page.click();
  assert.equal(page.state.copied.length, 1);
  assert.equal(page.button.getAttribute("aria-busy"), "true");
  assert.equal(page.button.disabled, false);
  assert.equal(page.document.activeElement, page.button);
  finish();
  await new Promise(setImmediate);
  assert.equal(page.button.textContent, "Copied");
  assert.equal(page.button.getAttribute("aria-busy"), null);
});

test("blocked browser storage does not disable copy, pending badges or Live Admin", async () => {
  const page = handbook({ blockedStorage: true, fallbackResult: true });
  assert.equal(page.pending.getAttribute("data-pending"), "matrix-gap");
  assert.equal(page.topbar.querySelector("[data-live-admin]").textContent, "Live Admin");
  assert.equal(page.nav.querySelector("[data-live-admin]").textContent, "Live Admin");
  assert.doesNotThrow(() => { page.fire(page.sidebar, "scroll"); page.fire(page.nav, "scroll"); });
  await page.click();
  assert.equal(page.button.textContent, "Copied");
});

test("revealing the active nav item keeps the document's deep-link position", () => {
  const page = handbook();
  assert.ok(page.sidebar.scrollTop > 0);
  assert.ok(page.nav.scrollLeft > 0);
  const item = page.active.getBoundingClientRect();
  assert.ok(item.top >= 0 && item.bottom <= page.sidebar.clientHeight);
  assert.ok(item.left >= 16 && item.right <= page.nav.clientWidth - 32);
  assert.equal(page.state.documentScroll, 987);
  assert.equal(page.state.ancestorScrolls, 0);
});

test("vertical and mobile horizontal nav offsets persist independently", () => {
  const page = handbook({ activeTop: 200, activeLeft: 100,
    saved: { "runkite-docs-nav-scroll": "100", "runkite-docs-nav-scroll-x": "40" } });
  assert.equal(page.sidebar.scrollTop, 100);
  assert.equal(page.nav.scrollLeft, 40);
  page.sidebar.scrollTop = 130;
  page.nav.scrollLeft = 120;
  page.fire(page.nav, "scroll");
  assert.equal(page.storage.get("runkite-docs-nav-scroll"), "130");
  assert.equal(page.storage.get("runkite-docs-nav-scroll-x"), "120");
});

test("storage quota errors on navigation remain nonfatal", () => {
  const page = handbook({ blockedWrites: true });
  assert.doesNotThrow(() => { page.fire(page.sidebar, "scroll"); page.fire(page.nav, "scroll"); page.fire(page.active, "click"); });
  assert.equal(page.state.documentScroll, 987);
});

test("every static handbook page has a skip link targeting its focusable main", () => {
  const root = path.join(__dirname, "../site/support");
  const files = fs.readdirSync(root, { recursive: true }).filter((name) => name.endsWith(".html"));
  assert.ok(files.length > 40);
  for (const name of files) {
    const html = fs.readFileSync(path.join(root, name), "utf8");
    const skip = html.match(/<body>\s*<a class="docs-skip" href="#([^"]+)">Skip to content<\/a>/);
    const main = html.match(/<main\b[^>]*>/);
    assert.ok(skip, name + " has no first keyboard bypass");
    assert.ok(main, name + " has no main content");
    assert.ok(main[0].includes('id="' + skip[1] + '"') && main[0].includes('tabindex="-1"'), name);
  }
});

test("the FinOps configuration example is copyable JSON in a bounded code block", () => {
  const html = fs.readFileSync(path.join(__dirname, "../site/support/finops.html"), "utf8");
  const sample = html.match(/<pre class="mono-block">([\s\S]*?)<\/pre>/);
  assert.ok(sample, "FinOps configuration has no styled code block");
  const config = JSON.parse(sample[1]);
  assert.ok(config.finops.pricebook);
  assert.ok(config.finops.budgets.tenants);
});
