const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

const site = path.resolve(__dirname, "../site");
const admin = fs.readFileSync(path.join(site, "admin/index.html"), "utf8");
const notFound = fs.readFileSync(path.join(site, "404.html"), "utf8");

function attributes(tag) {
  return Object.fromEntries([...tag.matchAll(/([\w-]+)="([^"]*)"/g)].map((m) => [m[1], m[2]]));
}

function anchors(html) {
  return [...html.matchAll(/<a\b[^>]*>/g)].map((m) => attributes(m[0]));
}

function assertStaticTarget(url, prefix) {
  let name = decodeURIComponent(url.pathname);
  assert.ok(name.startsWith(prefix), url.href + " escaped the site prefix");
  name = name.slice(prefix.length);
  if (!name || name.endsWith("/")) name += "index.html";
  const target = path.join(site, name);
  assert.ok(fs.statSync(target).isFile(), url.href);
  if (url.hash) {
    const html = fs.readFileSync(target, "utf8");
    assert.ok(html.includes('id="' + url.hash.slice(1) + '"'), url.href + " has no section target");
  }
}

function render404(hostname, pathname) {
  const links = anchors(notFound).filter((a) => "data-site-path" in a).map((attrs) => ({
    attrs,
    getAttribute(name) { return this.attrs[name] ?? null; },
    setAttribute(name, value) { this.attrs[name] = value; },
  }));
  const panels = { "not-found-panel": { hidden: false }, "admin-status-panel": { hidden: true } };
  const document = {
    title: "Page not found · Runkite",
    querySelectorAll(selector) { assert.equal(selector, "[data-site-path]"); return links; },
    getElementById(id) { return panels[id]; },
  };
  const rejectNetwork = () => { throw new Error("The static status page must not call a backend"); };
  const context = {
    document, location: Object.freeze({ hostname, pathname }),
    fetch: rejectNetwork, XMLHttpRequest: rejectNetwork, WebSocket: rejectNetwork,
    setInterval: rejectNetwork, setTimeout: rejectNetwork,
  };
  const scripts = [...notFound.matchAll(/<script>([\s\S]*?)<\/script>/g)];
  assert.equal(scripts.length, 1);
  vm.runInNewContext(scripts[0][1], context, { filename: "site/404.html" });
  return { document, panels, links };
}

test("Admin landing explains shared-demo availability without a login form or backend code", () => {
  assert.match(admin, /Demo offline/);
  assert.match(admin, /public Admin demo is currently unavailable/);
  assert.match(admin, /Your own control plane serves its Admin UI/);
  assert.doesNotMatch(admin, /<(?:form|input|script)\b/i);
  assert.doesNotMatch(admin, /(?:fetch\(|XMLHttpRequest|WebSocket|setInterval)/);
});

test("Admin landing links and styling work at the custom domain and project prefix", () => {
  for (const base of ["https://getrunkite.com/admin/", "https://getrunkite.github.io/runkite/admin/"]) {
    const prefix = new URL(base).hostname.endsWith("github.io") ? "/runkite/" : "/";
    for (const a of anchors(admin)) {
      const url = new URL(a.href, base);
      if (url.origin === new URL(base).origin) assertStaticTarget(url, prefix);
    }
    assert.doesNotMatch(admin, /<(?:link|img)\b[^>]*(?:href|src)="(?!data:)/i);
    assert.match(admin, /<style>[\s\S]+<\/style>/);
  }
});

test("the recorded preview CTA reaches the actual image/player and local demo anchor", () => {
  assert.match(admin, /href="\.\.\/#product-image">Watch the recorded preview/);
  assert.match(admin, /href="\.\.\/support\/try\.html#local-compose">Run the demo locally/);
  assert.doesNotMatch(admin + notFound, /(?:href|data-site-path)="[^"]*#demo"/);
});

test("Admin deep links show availability for the custom domain and Pages prefix", () => {
  for (const [hostname, pathname] of [
    ["getrunkite.com", "/admin/runs/saved-run"],
    ["getrunkite.com", "/admin/threads/saved-thread"],
    ["getrunkite.github.io", "/runkite/admin/runs/saved-run"],
  ]) {
    const rendered = render404(hostname, pathname);
    assert.equal(rendered.panels["admin-status-panel"].hidden, false);
    assert.equal(rendered.panels["not-found-panel"].hidden, true);
    assert.match(rendered.document.title, /Admin demo unavailable/);
    const prefix = hostname.endsWith("github.io") ? "/runkite/" : "/";
    for (const link of rendered.links) assertStaticTarget(new URL(link.attrs.href, "https://" + hostname), prefix);
  }
});

test("unknown pages and API paths keep not-found content rather than pretending to be a backend", () => {
  for (const hostname of ["getrunkite.com", "getrunkite.github.io"]) {
    const prefix = hostname.endsWith("github.io") ? "/runkite" : "";
    for (const pathname of ["/missing", "/administrator", "/admin-api/runs", "/threads/id/runs/1", "/governance/decide"]) {
      const rendered = render404(hostname, prefix + pathname);
      assert.equal(rendered.panels["not-found-panel"].hidden, false, pathname);
      assert.equal(rendered.panels["admin-status-panel"].hidden, true, pathname);
      assert.equal(rendered.document.title, "Page not found · Runkite");
    }
  }
  for (const pathname of ["admin-api", "threads", "governance"]) {
    assert.equal(fs.existsSync(path.join(site, pathname)), false, "Unexpected static API endpoint: " + pathname);
  }
});

test("404 remains readable without scripts or an asset base at arbitrary depths", () => {
  assert.match(notFound, /<style>[\s\S]+<\/style>/);
  assert.doesNotMatch(notFound, /<(?:link|img)\b[^>]*(?:href|src)="(?!data:)/i);
  assert.match(notFound, /shared public demo is currently unavailable/);
  assert.match(notFound, /href="https:\/\/getrunkite.com\/admin\/"/);
  assert.match(notFound, /id="not-found-panel">/);
  assert.match(notFound, /id="admin-status-panel" hidden>/);
});

test("support and Engineering keep the public Admin addresses with availability hints", () => {
  const files = ["support/try.html", "support/admin-login.html", ...fs.readdirSync(path.join(site, "design")).filter((n) => n.endsWith(".html")).map((n) => "design/" + n)];
  for (const file of files) {
    const html = fs.readFileSync(path.join(site, file), "utf8");
    let checked = 0;
    for (const tag of html.matchAll(/<a\b[^>]*>/g)) {
      const attrs = attributes(tag[0]);
      if (attrs.href !== "https://getrunkite.com/admin/") continue;
      checked++;
      assert.match(tag[0], /\bdata-demo-offline\b/, file);
      assert.equal(attrs.title, "Shared live demo currently unavailable; local self-hosting is available.");
      assert.match(attrs["aria-label"], /currently unavailable/);
    }
    assert.ok(checked > 0, file + " removed its public Admin address");
  }
});
