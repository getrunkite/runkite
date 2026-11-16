import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, realpath, rm, readFile } from 'node:fs/promises';
import path from 'node:path';
import os from 'node:os';
import { fileURLToPath } from 'node:url';

test('pinned native loader and scheduler retain class retry guard and intentional repeats', async (t) => {
  const root = await realpath(await mkdtemp(path.join(os.tmpdir(), 'runkite-pfoo-native-')));
  t.after(() => rm(root, { recursive: true, force: true }));
  process.env.PROMPTFOO_DISABLE_TELEMETRY = '1';
  process.env.PROMPTFOO_DISABLE_UPDATE = '1';
  process.env.PROMPTFOO_CONFIG_DIR = path.join(root, 'promptfoo');
  process.env.IS_TESTING = 'true';
  const here = path.dirname(fileURLToPath(import.meta.url));
  const pkg = JSON.parse(await readFile(path.join(here, 'node_modules/promptfoo/package.json'), 'utf8'));
  assert.equal(pkg.version, '0.125.1', 'requalify source and scheduler before changing the pin');
  const { loadApiProvider, evaluate } = await import('promptfoo');
  const previous = globalThis.fetch;
  const creates = [];
  const runs = new Map();
  globalThis.fetch = async (url, init) => {
    assert.equal(new URL(url).origin, 'https://runkite.invalid', 'offline fixture attempted an external request');
    let run;
    if (init.method === 'POST') {
      const body = JSON.parse(init.body);
      run = { ...body, thread_id: new URL(url).pathname.split('/')[2], status: 'success' };
      creates.push(run); runs.set(run.run_id, run);
    } else run = runs.get(new URL(url).pathname.split('/')[2]);
    return Response.json(String(url).endsWith('/wait') ? { run, values: { messages: [{ role: 'assistant', content: 'native answer' }] } } : run);
  };
  t.after(() => { globalThis.fetch = previous; });
  const provider = await loadApiProvider(path.join(here, 'runkite-provider.mjs'), { options: { config: { baseUrl: 'https://runkite.invalid', agentId: 'fixture-agent', receiptDirectory: path.join(root, 'receipts') }, env: { RUNKITE_API_KEY: 'synthetic-native-key' } } });
  assert.equal(provider.handlesOwnRetries, true);
  assert.deepEqual(provider.config, { maxRetries: 0 });
  const original = provider.callApi.bind(provider);
  let calls = 0;
  // Diagnostic response only: force the real scheduler's retry predicate after
  // an actual managed call. A scheduler replay would create extra run IDs.
  provider.callApi = async (...args) => {
    calls++;
    const result = await original(...args);
    assert.equal(result.output, 'native answer');
    return { error: 'qualification transient availability', metadata: { ...result.metadata, retryableErrorKind: 'transient_availability' } };
  };
  provider.config.maxRetries = 3;
  const evaluation = await evaluate({ prompts: ['{{input}}'], providers: [provider], tests: [{ vars: { input: 'native repeated prompt' } }] }, { repeat: 2, maxConcurrency: 1, cache: false, writeLatestResults: false });
  const results = await evaluation.getResults();
  assert.equal(results.length, 2);
  assert.equal(calls, 2, 'scheduler must not replay callApi for a retryable result');
  assert.equal(creates.length, 2);
  assert.equal(new Set(creates.map((r) => r.run_id)).size, 2);
  assert.equal(new Set(creates.map((r) => r.thread_id)).size, 2);
  for (const result of results) assert.match(result.response.error, /qualification transient availability/);
});
