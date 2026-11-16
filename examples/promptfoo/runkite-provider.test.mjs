import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, realpath, rm, readFile, readdir, chmod, symlink, stat, writeFile, open } from 'node:fs/promises';
import path from 'node:path';
import os from 'node:os';
import RunkiteProvider from './runkite-provider.mjs';
import { loadIntent, loadReference, saveIntent, saveReference } from './receipts.mjs';

const KEY = 'fixture-credential-never-in-artifacts';
const REF = 'run-v1.fixture-reference';
function json(data, reference) {
  return Response.json(data, { headers: reference ? { 'Runkite-Run-Reference': reference } : {} });
}
async function fixture(t, options = {}) {
  const root = await realpath(await mkdtemp(path.join(os.tmpdir(), 'runkite-pfoo-')));
  t.after(() => rm(root, { recursive: true, force: true }));
  const directory = path.join(root, 'receipts');
  const calls = [];
  let run;
  let values = { messages: [{ role: 'assistant', content: 'fixture answer' }] };
  const fetcher = async (url, init) => {
    const call = { method: init.method, pathname: new URL(url).pathname, headers: init.headers, signal: init.signal, redirect: init.redirect, body: init.body && JSON.parse(init.body) };
    calls.push(call);
    if (call.method === 'POST') run = { ...call.body, thread_id: call.pathname.split('/')[2], status: 'running' };
    if (options.respond) return options.respond({ call, calls, run, values, json });
    return call.pathname.endsWith('/wait') ? json({ run: { ...run, status: 'success' }, values }, options.reference) : json(run, options.reference);
  };
  const provider = new RunkiteProvider({ config: { baseUrl: 'https://runkite.invalid', agentId: 'fixture-agent', receiptDirectory: directory, timeoutMs: 500, requestTimeoutMs: 100, pollIntervalMs: 1, ...options.config }, env: { RUNKITE_API_KEY: KEY } }, { fetch: fetcher });
  return { root, directory, provider, calls, setValues(value) { values = value; } };
}

test('public managed create, get and wait preserve prompt and durable private binding', async (t) => {
  const f = await fixture(t, { reference: REF });
  const prompt = 'raw prompt\nwith whitespace  ';
  const result = await f.provider.callApi(prompt);
  assert.equal(result.output, 'fixture answer');
  assert.equal(result.cached, false);
  assert.equal(f.provider.handlesOwnRetries, true);
  assert.deepEqual(f.provider.config, { maxRetries: 0 });
  assert.deepEqual(f.calls.map((c) => c.method), ['POST', 'GET', 'GET']);
  assert.deepEqual(f.calls[0].body.input, { messages: [{ role: 'user', content: prompt }] });
  assert.equal(f.calls[0].body.if_not_exists, 'create');
  assert.equal(f.calls[0].body.multitask_strategy, 'reject');
  for (const c of f.calls) {
    assert.equal(c.headers.Authorization, `Bearer ${KEY}`);
    assert.equal(c.redirect, 'error');
    assert.equal(c.headers['X-Runkite-Simulation'], undefined);
    assert.ok(!c.pathname.includes('admin'));
  }
  assert.equal(f.calls[2].headers['Runkite-Expected-Run-Reference'], REF);
  assert.equal(result.metadata.runkite.run_reference_acquired, true);
  const receipt = result.metadata.runkite.receipt;
  assert.equal((await stat(f.directory)).mode & 0o777, 0o700);
  assert.equal((await stat(receipt)).mode & 0o777, 0o600);
  assert.equal((await loadIntent(receipt)).run_id, f.calls[0].body.run_id);
  assert.equal(await loadReference(receipt), REF);
  assert.ok(!(await readFile(receipt, 'utf8')).includes(prompt));
  assert.ok(!JSON.stringify(f.provider).includes(KEY));
  assert.ok(!JSON.stringify(result).includes(KEY));
});

test('intentional repeated calls create fresh run and thread IDs', async (t) => {
  const f = await fixture(t);
  await f.provider.callApi('same');
  await f.provider.callApi('same');
  const creates = f.calls.filter((c) => c.method === 'POST');
  assert.equal(creates.length, 2);
  assert.notEqual(creates[0].body.run_id, creates[1].body.run_id);
  assert.notEqual(creates[0].pathname, creates[1].pathname);
  assert.equal((await readdir(f.directory)).filter((n) => n.endsWith('.json')).length, 2);
});

test('lost create response recovers only by reads, retaining the original identity', async (t) => {
  const f = await fixture(t, { respond: ({ call, run, values, json }) => {
    if (call.method === 'POST') throw new Error(`transport ${KEY}`);
    return call.pathname.endsWith('/wait') ? json({ run: { ...run, status: 'success' }, values }) : json(run);
  } });
  const result = await f.provider.callApi('once');
  assert.equal(result.output, 'fixture answer');
  assert.equal(f.calls.filter((c) => c.method === 'POST').length, 1);
});

test('lost create followed by not-yet-visible row stays unknown and recovery never creates', async (t) => {
  const f = await fixture(t, { respond: ({ call }) => {
    if (call.method === 'POST') throw new Error('delivery unknown');
    return new Response(null, { status: 404 });
  } });
  const result = await f.provider.callApi('once');
  assert.match(result.error, /unknown/);
  assert.equal(result.metadata.runkite.state, 'unknown');
  assert.equal(result.metadata.runkite.run_reference_acquired, undefined);
  const recovered = await f.provider.observeReceipt(result.metadata.runkite.receipt);
  assert.match(recovered.error, /unknown/);
  assert.deepEqual(f.calls.map((c) => c.method), ['POST', 'GET', 'GET']);
});

test('transient read retries use the same run without replaying create', async (t) => {
  let firstRead = true;
  const f = await fixture(t, { respond: ({ call, run, values, json }) => {
    if (call.method === 'GET' && firstRead) { firstRead = false; throw new Error('socket reset'); }
    return call.pathname.endsWith('/wait') ? json({ run: { ...run, status: 'success' }, values }) : json(run);
  } });
  assert.equal((await f.provider.callApi('once')).output, 'fixture answer');
  assert.equal(f.calls.filter((c) => c.method === 'POST').length, 1);
  assert.equal(new Set(f.calls.filter((c) => c.method === 'GET').map((c) => c.pathname.split('/')[2])).size, 1);
});

test('recovery validates agent, thread, input and run identity', async (t) => {
  for (const change of [{ run_id: 'other' }, { thread_id: 'other' }, { agent_id: 'other' }, { input: { messages: [{ role: 'user', content: 'changed' }] } }]) {
    await t.test(Object.keys(change)[0], async (t) => {
      const f = await fixture(t, { respond: ({ call, run, json }) => json(call.method === 'POST' ? run : { ...run, ...change }) });
      const result = await f.provider.callApi('bound');
      assert.match(result.error, /unknown/);
      assert.equal(f.calls.length, 2);
    });
  }
});

test('acquired reference is immutable and required on subsequent responses and recovery', async (t) => {
  for (const changed of [undefined, 'run-v1.different', `${REF}, ${REF}`]) {
    await t.test(String(changed), async (t) => {
      const f = await fixture(t, { respond: ({ call, run, values, json }) => call.pathname.endsWith('/wait') ? json({ run: { ...run, status: 'success' }, values }, changed) : json(run, REF) });
      const result = await f.provider.callApi('bound');
      assert.match(result.error, /unknown/);
      assert.equal(await loadReference(result.metadata.runkite.receipt), REF);
      const before = f.calls.length;
      await f.provider.observeReceipt(result.metadata.runkite.receipt);
      assert.equal(f.calls[before].headers['Runkite-Expected-Run-Reference'], REF);
      assert.equal(f.calls.filter((c) => c.method === 'POST').length, 1);
    });
  }
});

test('a verified create response reference is pinned before any GET', async (t) => {
  const f = await fixture(t, { respond: ({ call, run, json }) => json(run, call.method === 'POST' ? REF : 'run-v1.replacement') });
  const result = await f.provider.callApi('bound');
  assert.match(result.error, /unknown/);
  assert.equal(f.calls.length, 2);
  assert.equal(f.calls[1].headers['Runkite-Expected-Run-Reference'], REF);
  assert.equal(await loadReference(result.metadata.runkite.receipt), REF);
});

test('an invalid create response reference fails before GET observation', async (t) => {
  for (const reference of ['joined,duplicate', '']) {
    const f = await fixture(t, { respond: ({ run }) => Response.json(run, { headers: { 'Runkite-Run-Reference': reference } }) });
    const result = await f.provider.callApi('bound');
    assert.match(result.error, /unknown/);
    assert.deepEqual(f.calls.map((c) => c.method), ['POST']);
  }
});

test('terminal managed failures are provider errors without invented output', async (t) => {
  for (const status of ['error', 'interrupted', 'timeout']) {
    await t.test(status, async (t) => {
      const f = await fixture(t, { respond: ({ call, run, json }) => call.pathname.endsWith('/wait') ? json({ run: { ...run, status }, values: { result: 'not success' } }) : json(run) });
      const result = await f.provider.callApi('input');
      assert.equal(result.output, undefined);
      assert.match(result.error, new RegExp(status));
    });
  }
});

test('text mode accepts only unambiguous final assistant text', async (t) => {
  const cases = [
    { messages: [{ role: 'user', content: 'echo' }] },
    { messages: [{ role: 'assistant', type: 'human', content: 'contradiction' }] },
    { messages: [{ role: 'assistant', content: [{ type: 'text', text: 'block' }] }] },
    { messages: [{ role: 'assistant', content: 'reasoning', channel: 'analysis' }] },
    { messages: [{ role: 'assistant', content: 'tool', tool_calls: [{ id: 'x' }] }] },
    { messages: [{ role: 'assistant', content: 'truncated', response_metadata: { finish_reason: 'length' } }] },
    { messages: [{ role: 'assistant', content: 'one' }], result: 'two' },
    { result: 'no messages' },
  ];
  for (const values of cases) {
    const f = await fixture(t); f.setValues(values);
    assert.match((await f.provider.callApi('input')).error, /unknown/);
  }
  const f = await fixture(t);
  f.setValues({ messages: [{ type: 'ai', data: { content: 'serialized final', tool_calls: [], response_metadata: { finish_reason: 'stop' } } }] });
  assert.equal((await f.provider.callApi('input')).output, 'serialized final');
});

test('explicit JSON mode returns full values and only consistent measured usage', async (t) => {
  const f = await fixture(t, { config: { outputMode: 'json' } });
  const values = { result: { structured: true }, usage: { prompt_tokens: 2, completion_tokens: 3, total_tokens: 5, cost_usd: 0.01 } };
  f.setValues(values);
  const result = await f.provider.callApi('input');
  assert.deepEqual(result.output, values);
  assert.deepEqual(result.tokenUsage, { prompt: 2, completion: 3, total: 5 });
  assert.equal(result.cost, 0.01);
  f.setValues({ ...values, usage: { ...values.usage, unmetered: true } });
  const unmetered = await f.provider.callApi('input');
  assert.equal(unmetered.tokenUsage, undefined);
  assert.equal(unmetered.cost, undefined);
  f.setValues({ ...values, usage: { prompt_tokens: 2, completion_tokens: 3, total_tokens: 999 } });
  assert.equal((await f.provider.callApi('input')).tokenUsage, undefined);
});

test('total deadline bounds a fetch that ignores AbortSignal without a second create', async (t) => {
  const f = await fixture(t, { config: { timeoutMs: 60, requestTimeoutMs: 20 }, respond: () => new Promise(() => {}) });
  const started = performance.now();
  // Keep the event loop alive while testing native AbortSignal.timeout timers.
  const keepAlive = setInterval(() => {}, 100);
  t.after(() => clearInterval(keepAlive));
  const result = await f.provider.callApi('input');
  assert.match(result.error, /unknown/);
  assert.ok(performance.now() - started < 500);
  assert.equal(f.calls.filter((c) => c.method === 'POST').length, 1);
});

test('caller abort detaches observation, creates no cancellation request and allows read-only recovery', async (t) => {
  const controller = new AbortController();
  const f = await fixture(t, { respond: ({ call, run, json }) => {
    if (call.pathname.endsWith('/wait')) { controller.abort(); return new Promise(() => {}); }
    return json(run);
  } });
  const result = await f.provider.callApi('input', undefined, { abortSignal: controller.signal });
  assert.match(result.error, /Observation cancellation does not prove worker cancellation/);
  assert.ok(f.calls.every((c) => !c.pathname.includes('cancel')));
  const before = f.calls.length;
  await f.provider.callApi('input', undefined, { abortSignal: controller.signal });
  assert.equal(f.calls.length, before);
});

test('oversized or malformed response fails closed without replay', async (t) => {
  for (const response of [() => new Response('x', { headers: { 'content-length': String((1 << 20) + 1) } }), () => new Response('x'.repeat((1 << 20) + 1)), () => new Response('{broken')]) {
    const f = await fixture(t, { respond: ({ call, run, json }) => call.method === 'POST' ? json(run) : response() });
    assert.match((await f.provider.callApi('input')).error, /unknown/);
    assert.equal(f.calls.length, 2);
  }
});

test('response body stalls are bounded by the total observation deadline', async (t) => {
  const f = await fixture(t, { config: { timeoutMs: 60, requestTimeoutMs: 20 }, respond: ({ call, run, json }) => call.method === 'POST' ? json(run) : new Response(new ReadableStream({ start() {} })) });
  const keepAlive = setInterval(() => {}, 100);
  t.after(() => clearInterval(keepAlive));
  const started = performance.now();
  const result = await f.provider.callApi('input');
  assert.match(result.error, /unknown/);
  assert.ok(performance.now() - started < 500);
  assert.equal(f.calls.filter((c) => c.method === 'POST').length, 1);
});

test('unsafe or symlinked receipts fail before dispatch and never overwrite intent', async (t) => {
  const f = await fixture(t);
  await writeFile(path.join(f.root, 'target'), 'untouched', { mode: 0o600 });
  await symlink(path.join(f.root, 'target'), f.directory);
  assert.match((await f.provider.callApi('input')).error, /no create was sent/);
  assert.equal(f.calls.length, 0);
  await rm(f.directory);
  const result = await f.provider.callApi('input');
  const receipt = result.metadata.runkite.receipt;
  for (const invalid of ['one\ntwo', 'one,two', undefined, '', 'x'.repeat(2049)]) await assert.rejects(saveReference(receipt, invalid));
  await saveReference(receipt, REF);
  await assert.rejects(saveReference(receipt, 'changed'));
  await chmod(receipt, 0o644);
  assert.match((await f.provider.observeReceipt(receipt)).error, /no create was sent/);
  assert.equal(f.calls.filter((c) => c.method === 'POST').length, 1);
});

test('destination binding, input limits and secure endpoint configuration fail closed', async (t) => {
  const f = await fixture(t);
  assert.ok((await f.provider.callApi({ text: 'not a string' })).error);
  assert.ok((await f.provider.callApi('x'.repeat((64 << 10) + 1))).error);
  assert.equal(f.calls.length, 0);
  const result = await f.provider.callApi('input');
  f.provider.agentId = 'changed';
  assert.match((await f.provider.observeReceipt(result.metadata.runkite.receipt)).error, /no create was sent/);
  for (const baseUrl of ['http://example.com', 'https://u:secret@example.com', 'https://example.com?secret', 'not-a-url']) {
    assert.throws(() => new RunkiteProvider({ config: { baseUrl }, env: { RUNKITE_API_KEY: KEY } }), /provider_url_invalid/);
  }
});

test('overlapping directory publishers each persist the parent before dispatch eligibility', async (t) => {
  const f = await fixture(t);
  const parentInfo = await stat(f.root);
  const probe = await open(path.join(f.root, 'probe'), 'wx', 0o600);
  const prototype = Object.getPrototypeOf(probe);
  const original = prototype.sync;
  await probe.close();
  let firstReached, releaseFirst;
  const reached = new Promise((resolve) => { firstReached = resolve; });
  const gate = new Promise((resolve) => { releaseFirst = resolve; });
  let parentAttempts = 0;
  let secondParentPersisted = false;
  t.mock.method(prototype, 'sync', async function () {
    const info = await this.stat();
    if (info.dev === parentInfo.dev && info.ino === parentInfo.ino) {
      if (++parentAttempts === 1) { firstReached(); await gate; throw new Error('injected first parent fsync failure'); }
      await original.call(this); secondParentPersisted = true; return;
    }
    return original.call(this);
  });
  const intent = (digit) => ({ version: 1, run_id: `pfoo-${digit.repeat(8)}-${digit.repeat(4)}-${digit.repeat(4)}-${digit.repeat(4)}-${digit.repeat(12)}` });
  const first = saveIntent(f.directory, intent('a')).catch((error) => error);
  await reached;
  try {
    await saveIntent(f.directory, intent('b'));
    assert.equal(secondParentPersisted, true, 'EEXIST publisher must establish its own parent durability barrier');
  } finally { releaseFirst(); }
  assert.match((await first).message, /injected first parent/);
});

test('overlapping equal reference pins fsync the directory before accepting a linked pin', async (t) => {
  const f = await fixture(t);
  const result = await f.provider.callApi('initial intent');
  const receipt = result.metadata.runkite.receipt;
  const directoryInfo = await stat(f.directory);
  const probe = await open(path.join(f.root, 'probe'), 'wx', 0o600);
  const prototype = Object.getPrototypeOf(probe);
  const original = prototype.sync;
  await probe.close();
  let firstReached, releaseFirst;
  const reached = new Promise((resolve) => { firstReached = resolve; });
  const gate = new Promise((resolve) => { releaseFirst = resolve; });
  let directoryAttempts = 0;
  let secondPinPersisted = false;
  t.mock.method(prototype, 'sync', async function () {
    const info = await this.stat();
    if (info.dev === directoryInfo.dev && info.ino === directoryInfo.ino) {
      if (++directoryAttempts === 1) { firstReached(); await gate; throw new Error('injected first pin fsync failure'); }
      await original.call(this); secondPinPersisted = true; return;
    }
    return original.call(this);
  });
  const first = saveReference(receipt, REF).catch((error) => error);
  await reached;
  try {
    await saveReference(receipt, REF);
    assert.equal(secondPinPersisted, true, 'equal EEXIST pin must persist the directory rather than trust the first writer');
  } finally { releaseFirst(); }
  assert.match((await first).message, /run_reference_invalid/);
  assert.equal(await loadReference(receipt), REF);
});

test('an observer fsyncs a concurrently linked reference before using the pin', async (t) => {
  const f = await fixture(t);
  const initial = await f.provider.callApi('initial intent');
  const receipt = initial.metadata.runkite.receipt;
  const previousFetch = f.provider.fetcher;
  f.provider.fetcher = async (...args) => {
    const response = await previousFetch(...args);
    response.headers.set('Runkite-Run-Reference', REF);
    return response;
  };
  const directoryInfo = await stat(f.directory);
  const probe = await open(path.join(f.root, 'probe'), 'wx', 0o600);
  const prototype = Object.getPrototypeOf(probe);
  const original = prototype.sync;
  await probe.close();
  let firstReached, releaseFirst;
  const reached = new Promise((resolve) => { firstReached = resolve; });
  const gate = new Promise((resolve) => { releaseFirst = resolve; });
  let directoryAttempts = 0;
  let observerPersisted = false;
  t.mock.method(prototype, 'sync', async function () {
    const info = await this.stat();
    if (info.dev === directoryInfo.dev && info.ino === directoryInfo.ino) {
      if (++directoryAttempts === 1) { firstReached(); await gate; throw new Error('injected publisher fsync failure'); }
      await original.call(this); observerPersisted = true; return;
    }
    return original.call(this);
  });
  const publisher = saveReference(receipt, REF).catch((error) => error);
  await reached;
  try {
    const before = f.calls.length;
    const result = await f.provider.observeReceipt(receipt);
    assert.equal(result.output, 'fixture answer');
    assert.equal(observerPersisted, true, 'observer must establish its own durability barrier before using a linked pin');
    assert.equal(f.calls[before].headers['Runkite-Expected-Run-Reference'], REF);
    assert.equal(f.calls.filter((c) => c.method === 'POST').length, 1);
  } finally { releaseFirst(); }
  assert.match((await publisher).message, /run_reference_invalid/);
});
