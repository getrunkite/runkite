import assert from 'node:assert/strict';
import { mkdtemp, readFile, realpath, rm, writeFile } from 'node:fs/promises';
import path from 'node:path';
import os from 'node:os';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

// Invoked by internal/api/promptfoo_qualification_test.go with a real local plane.
// The worker is scripted. No model provider or remote grader is configured.
const here = path.dirname(fileURLToPath(import.meta.url));
const endpoint = new URL(process.env.RUNKITE_URL);
assert.equal(endpoint.protocol, 'http:');
assert.equal(endpoint.hostname, '127.0.0.1');
const root = await realpath(await mkdtemp(path.join(os.tmpdir(), 'runkite-pfoo-cli-')));
try {
  const pkg = JSON.parse(await readFile(path.join(here, 'node_modules/promptfoo/package.json'), 'utf8'));
  assert.equal(pkg.version, '0.125.1');
  const configFile = path.join(root, 'promptfooconfig.json');
  const outputFile = path.join(root, 'results.json');
  await writeFile(configFile, JSON.stringify({
    description: 'Offline managed-run qualification with a scripted worker',
    prompts: ['{{input}}'],
    providers: [{ id: path.join(here, 'runkite-provider.mjs'), config: { baseUrl: endpoint.href, agentId: 'pfoo-fixture', allowInsecureLoopback: true, receiptDirectory: path.join(root, 'receipts'), timeoutMs: 5000, requestTimeoutMs: 1000 } }],
    tests: [{ vars: { input: 'first fixture prompt' }, assert: [{ type: 'equals', value: 'scripted:first fixture prompt' }] }, { vars: { input: 'second fixture prompt' }, assert: [{ type: 'equals', value: 'scripted:second fixture prompt' }] }],
  }));
  const child = spawn(process.execPath, [path.join(here, 'node_modules/promptfoo/dist/src/entrypoint.js'), 'eval', '--config', configFile, '--output', outputFile, '--repeat', '2', '--max-concurrency', '1', '--no-cache', '--no-progress-bar', '--no-table'], {
    cwd: root,
    // Deliberately exclude ambient model credentials, proxy settings and hooks.
    env: { PATH: process.env.PATH, HOME: root, CI: '1', RUNKITE_API_KEY: process.env.RUNKITE_API_KEY, PROMPTFOO_CONFIG_DIR: path.join(root, 'promptfoo'), PROMPTFOO_DISABLE_TELEMETRY: '1', PROMPTFOO_DISABLE_UPDATE: '1', PROMPTFOO_DISABLE_REMOTE_GENERATION: '1', PROMPTFOO_DISABLE_REDTEAM_REMOTE_GENERATION: '1' },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let logs = '';
  const collect = (chunk) => { if (logs.length < 65536) logs += chunk.toString(); };
  child.stdout.on('data', collect); child.stderr.on('data', collect);
  const timer = setTimeout(() => child.kill('SIGKILL'), 20000);
  const code = await new Promise((resolve, reject) => { child.once('error', reject); child.once('exit', resolve); }).finally(() => clearTimeout(timer));
  assert.ok(!logs.includes(process.env.RUNKITE_API_KEY), 'credential appeared in native CLI output');
  assert.equal(code, 0, 'native promptfoo CLI failed: ' + logs);
  const output = JSON.parse(await readFile(outputFile, 'utf8'));
  const results = output.results.results;
  assert.equal(results.length, 4);
  const handles = results.map((r) => {
    assert.equal(r.success, true);
    assert.ok(!r.response.error);
    assert.equal(r.response.metadata.runkite.state, 'success');
    return r.response.metadata.runkite;
  });
  assert.equal(new Set(handles.map((h) => h.run_id)).size, 4);
  assert.equal(new Set(handles.map((h) => h.thread_id)).size, 4);
  assert.ok(!JSON.stringify(output).includes(process.env.RUNKITE_API_KEY));
  console.log(JSON.stringify({ promptfoo: pkg.version, node: process.version, successful_evaluations: results.length, fresh_managed_runs: handles.length, model_calls: 0, grader: 'local equals', worker: 'scripted', run_reference_acquired: handles.every((h) => h.run_reference_acquired === true), pre_acquisition_continuity_proven: false }));
} finally {
  await rm(root, { recursive: true, force: true });
}
