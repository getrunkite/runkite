#!/usr/bin/env python3
"""Exercise the built TypeScript runtime image offline, without a control plane."""

import argparse
import json
import os
import subprocess
import uuid
from pathlib import Path

PROBE = r"""
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { existsSync, lstatSync, mkdtempSync, readFileSync, readdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { createRequire } from 'node:module';
import { graph } from '/app/examples/echo_agent_ts/graph.ts';
import { createAgent } from '/app/examples/managed_ai_sdk/agent.ts';
import { ToolLoopAgent, stepCountIs } from 'ai';

const require = createRequire(import.meta.url);
const absent = (name) => {
  assert.throws(() => lstatSync(name), { code: 'ENOENT' }, `${name} remains in runtime image`);
};
assert.notEqual(process.getuid(), 0, 'image ran as root');
assert.equal(process.getuid(), 65532, 'unexpected runtime identity');
for (const name of [
  '/usr/local/bin/npm', '/usr/local/bin/npx', '/usr/local/lib/node_modules/npm',
  '/app/typescript/runkite-runner/node_modules/typescript',
  '/app/examples/managed_ai_sdk/node_modules/typescript',
]) absent(name);
for (const namespace of [
  '/app/typescript/runkite-runner/node_modules/@typescript',
  '/app/examples/managed_ai_sdk/node_modules/@typescript',
]) {
  // npm can preserve an empty scope directory after omitting all its packages.
  if (existsSync(namespace)) assert.deepEqual(readdirSync(namespace), [], 'native compiler package remains');
}
assert.equal(JSON.parse(readFileSync(require.resolve('ai/package.json'), 'utf8')).version, '7.0.155');

const directory = mkdtempSync(path.join(tmpdir(), 'runner-image-help-'));
try {
  for (const option of ['--help', '-h']) {
    const result = spawnSync(process.execPath, [
      '--import', import.meta.resolve('tsx'),
      '/app/typescript/runkite-runner/src/cli.ts', option,
    ], { cwd: directory, encoding: 'utf8', timeout: 15000,
      env: { ...process.env, RUNKITE_CONCURRENCY: 'invalid' } });
    assert.ifError(result.error);
    assert.equal(result.status, 0, result.stderr);
    assert.match(result.stdout, /^Usage: runkite-runner/m);
    assert.equal(result.stderr, '');
  }
} finally {
  rmSync(directory, { recursive: true, force: true });
}

const text = 'echo offline runtime qualification';
const echoed = await graph.invoke({ messages: [{ role: 'user', content: text }] });
assert.deepEqual(echoed.messages.at(-1), { role: 'ai', content: text });
const definition = createAgent({ runId: 'image-fixture', threadId: 'image-thread',
  tenantId: 'default', generation: 1, signal: new AbortController().signal, runkiteConfig: {} });
const agent = new ToolLoopAgent({ ...definition, stopWhen: stepCountIs(4) });
const result = await agent.stream({ messages: [{ role: 'user', content: text }] });
let textChunks = 0;
for await (const part of result.fullStream) {
  if (part.type === 'error') throw part.error;
  if (part.type === 'text-delta') textChunks++;
}
const steps = await result.steps;
assert.equal(steps.length, 2, 'native tool loop did not complete both model steps');
assert.equal(steps[0].toolCalls.length, 1);
assert.equal(steps[0].toolCalls[0].toolName, 'echo');
assert.equal(steps[0].toolResults.length, 1);
assert.deepEqual(steps[0].toolResults[0].output, { text });
assert.equal(await result.text, `Remembered user turns: 1. ${text}`);
assert.ok(textChunks > 0, 'native AI SDK text stream was empty');
const usage = await result.totalUsage;
assert.equal(usage.inputTokens, 20);
assert.equal(usage.outputTokens, 4);
console.log(JSON.stringify({
  passed: true, uid: process.getuid(), ai_version: '7.0.155',
  cases: ['non-root', 'npm/npx absent', 'development compilers absent',
    'local tsx loads native TypeScript examples', 'help without config',
    'native LangGraph echo', 'native AI SDK model/tool loop', 'native text stream',
    'two-step native usage'],
}));
"""


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True, help="An already built local TypeScript runner image")
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    if os.environ.get("RUNKITE_TEST_RUNNER_IMAGE") != "1":
        parser.error("set RUNKITE_TEST_RUNNER_IMAGE=1 to run this disposable image qualification")
    name = "runkite-image-" + uuid.uuid4().hex[:16]
    evidence = {
        "passed": False,
        "requested_image": args.image,
        "scope": "offline native runtime dependencies in the actual built TypeScript image",
        "not_qualified": ["managed control-plane dispatch", "HA", "live model providers", "external sandbox"],
    }
    created = False
    try:
        inspected = subprocess.run(
            ["docker", "image", "inspect", args.image, "--format", "{{.Id}}"],
            check=True,
            capture_output=True,
            text=True,
            timeout=30,
        ).stdout.strip()
        if not inspected.startswith("sha256:"):
            raise RuntimeError("local image has no immutable image ID")
        evidence["image_id"] = inspected
        # Mark before run so a timeout or interrupted client still removes only
        # the uniquely named container which this invocation attempted to own.
        created = True
        result = subprocess.run(
            [
                "docker",
                "run",
                "--pull=never",
                "--name",
                name,
                "--network",
                "none",
                "--read-only",
                "--tmpfs",
                "/tmp:rw,nosuid,nodev,size=64m",
                "--entrypoint",
                "node",
                inspected,
                "--import",
                "tsx",
                "--input-type=module",
                "--eval",
                PROBE,
            ],
            check=True,
            capture_output=True,
            text=True,
            timeout=120,
        )
        evidence.update(json.loads(result.stdout))
        if evidence.get("passed") is not True:
            raise AssertionError("runtime image probe did not confirm success")
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        evidence["error"] = type(error).__name__
        evidence["stdout"] = str(error.stdout or "")[-16000:]
        evidence["stderr"] = str(error.stderr or "")[-16000:]
        raise
    except Exception as error:
        evidence["error"] = str(error)
        raise
    finally:
        try:
            if created:
                removed = subprocess.run(["docker", "rm", "--force", name], capture_output=True, text=True, timeout=30)
                if removed.returncode and "No such container" not in removed.stderr:
                    evidence["passed"] = False
                    evidence["cleanup_error"] = removed.stderr[-16000:]
                    raise RuntimeError("owned runtime qualification container cleanup failed")
        except Exception as error:
            evidence["passed"] = False
            evidence.setdefault("cleanup_error", str(error))
            raise
        finally:
            Path(args.output).write_text(json.dumps(evidence, indent=2) + "\n")


if __name__ == "__main__":
    main()
