const assert = require('node:assert/strict');
const { test: check } = require('node:test');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const source = fs.readFileSync(path.join(__dirname, '..', 'site', 'execution-flow.js'), 'utf8');
function element() {
  const events = new Map(), attributes = new Map(), classes = new Set();
  return { hidden: false, textContent: '', events, attributes,
    classList: { add: v => classes.add(v), contains: v => classes.has(v) },
    setAttribute: (key, value) => attributes.set(key, value),
    addEventListener: (name, fn) => events.set(name, fn) };
}
function fixture(options = {}) {
  const root = element(), controls = element(), toggle = element();
  controls.hidden = toggle.hidden = true;
  const choices = [element(), element()];
  const svgs = [0, 1].map(index => {
    const svg = { paused: true, begin: 0 };
    if (!(options.unsupported || []).includes(index)) {
      svg.pauseAnimations = () => { svg.paused = true; };
      svg.unpauseAnimations = () => { svg.paused = false; };
    }
    svg.querySelectorAll = () => [{ beginElement() {
      if ((options.throwBegin || []).includes(index)) throw Error('No SVG motion');
      svg.begin++;
    } }];
    return svg;
  });
  const panels = svgs.map(svg => ({ ...element(), querySelector: () => svg }));
  root.querySelectorAll = selector => selector === '.execution-panel' ? panels : choices;
  root.querySelector = selector => selector === '.execution-controls' ? controls : toggle;
  const document = { ...element(), hidden: false, querySelector: () => root };
  const reduced = { ...element(), matches: Boolean(options.reduced) };
  const desktop = { ...element(), matches: options.desktop !== false };
  let intersection;
  class Observer {
    constructor(fn) { intersection = fn; }
    observe() {}
  }
  const window = { matchMedia: query => query.includes('reduced-motion') ? reduced : desktop };
  if (options.observer) window.IntersectionObserver = Observer;
  vm.runInNewContext(source, { window, document, IntersectionObserver: Observer });
  return { root, controls, toggle, choices, panels, svgs, reduced, desktop, document,
    click: e => e.events.get('click')(),
    intersect: visible => intersection([{isIntersecting:visible}]),
    emit: (e, type) => e.events.get(type)() };
}
check('original protocol view selected by default', () => {
  const f = fixture();
  assert.equal(f.root.attributes.get('data-execution-view'), 'protocol');
  assert.equal(f.panels[0].hidden, false); assert.equal(f.panels[1].hidden, true);
  assert.equal(f.choices[0].attributes.get('aria-pressed'), 'true');
  assert.equal(f.controls.hidden, false); assert.equal(f.toggle.hidden, false);
  assert.equal(f.svgs[0].paused, false); assert.equal(f.svgs[1].paused, true);
});
check('native view switches content and only its animation runs', () => {
  const f = fixture(); f.click(f.choices[1]);
  assert.equal(f.root.attributes.get('data-execution-view'), 'native');
  assert.equal(f.panels[0].hidden, true); assert.equal(f.panels[1].hidden, false);
  assert.equal(f.choices[0].attributes.get('aria-pressed'), 'false');
  assert.equal(f.svgs[0].paused, true); assert.equal(f.svgs[1].paused, false);
});
check('explicit pause survives view changes and Play restarts current view', () => {
  const f = fixture(); f.click(f.toggle); f.click(f.choices[1]);
  assert.equal(f.svgs[0].paused, true); assert.equal(f.svgs[1].paused, true);
  assert.equal(f.toggle.attributes.get('aria-pressed'), 'false');
  f.click(f.toggle); assert.equal(f.svgs[1].paused, false);
});
check('reduced motion starts paused with both views usable', () => {
  const f = fixture({reduced:true}); f.click(f.choices[1]);
  assert.equal(f.svgs[1].paused, true); assert.equal(f.panels[1].hidden, false);
  f.click(f.toggle); assert.equal(f.svgs[1].paused, false);
  f.reduced.matches = true; f.emit(f.reduced, 'change');
  assert.equal(f.svgs[1].paused, true);
});
check('mobile is static but view controls remain available', () => {
  const f = fixture({desktop:false}); f.click(f.choices[1]);
  assert.equal(f.controls.hidden, false); assert.equal(f.toggle.hidden, true);
  assert.equal(f.panels[1].hidden, false); assert.equal(f.svgs[1].paused, true);
});
check('offscreen and hidden document pause active motion', () => {
  const f = fixture({observer:true}); assert.equal(f.svgs[0].paused, true);
  f.intersect(true); assert.equal(f.svgs[0].paused, false);
  f.intersect(false); assert.equal(f.svgs[0].paused, true);
  f.intersect(true); f.document.hidden = true; f.emit(f.document, 'visibilitychange');
  assert.equal(f.svgs[0].paused, true);
  f.document.hidden = false; f.emit(f.document, 'visibilitychange');
  assert.equal(f.svgs[0].paused, false);
});
check('unsupported SVG still allows switching to a readable second panel', () => {
  const f = fixture({unsupported:[0,1]}); f.click(f.choices[1]);
  assert.equal(f.panels[1].hidden, false); assert.equal(f.controls.hidden, false);
  assert.equal(f.toggle.hidden, true);
});
check('motion initialization failure leaves readable selectable static content', () => {
  const f = fixture({throwBegin:[0]}); assert.equal(f.panels[0].hidden, false);
  assert.equal(f.toggle.hidden, true); f.click(f.choices[1]);
  assert.equal(f.toggle.hidden, false); assert.equal(f.svgs[1].paused, false);
});
