import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const packageDirectory = fileURLToPath(new URL("..", import.meta.url));
const metadata = JSON.parse(readFileSync(path.join(packageDirectory, "package.json"), "utf8"));
// Exercise the compiled entrypoint declared in the published package, without
// tsx or the test process's module hooks supplying runtime behavior.
const cli = path.join(packageDirectory, metadata.bin["runkite-runner"]);

function invoke(args: string[], environment: NodeJS.ProcessEnv = {}) {
  const cwd = mkdtempSync(path.join(tmpdir(), "runkite-cli-"));
  try {
    const result = spawnSync(process.execPath, [cli, ...args], {
      cwd,
      encoding: "utf8",
      timeout: 10_000,
      maxBuffer: 1024 * 1024,
      env: { ...process.env, RUNKITE_CONCURRENCY: "1", LOG_FORMAT: "text", ...environment },
    });
    assert.ifError(result.error);
    assert.equal(result.signal, null, "CLI did not finish normally");
    assert.deepEqual(readdirSync(cwd), [], "CLI changed its fresh working directory");
    return result;
  } finally {
    rmSync(cwd, { recursive: true, force: true });
  }
}

test("compiled CLI help succeeds before configuration, environment validation or framework setup", () => {
  for (const args of [["--help"], ["-h"], ["--framework", "ai-sdk", "--help"]]) {
    const result = invoke(args, {
      RUNKITE_CONCURRENCY: "malformed",
      RUNKITE_EXECUTION_POOL: "INVALID",
      RUNKITE_TLS_CA_FILE: "/nonexistent/runkite-cli-fixture-ca.pem",
    });
    assert.equal(result.status, 0, result.stderr);
    assert.match(result.stdout, /^Usage: runkite-runner/m);
    assert.match(result.stdout, /--execution-pool NAME/);
    assert.match(result.stdout, /RUNKITE_HTTP_URL/);
    assert.equal(result.stderr, "");
    assert.doesNotMatch(result.stdout, /ENOENT|fatal error|Loaded graph/);
  }
});

test("compiled CLI rejects malformed, nonpositive and unsafe concurrency before configuration", () => {
  const invalid = ["2junk", "1.5", "0", "-1", "1e3", "Infinity", "NaN", "9007199254740992", " 2", "2 ", ""];
  for (const value of invalid) {
    for (const source of ["argument", "environment"]) {
      const result =
        source === "argument" ? invoke(["--concurrency", value]) : invoke([], { RUNKITE_CONCURRENCY: value });
      assert.equal(result.status, 1, `${source}=${JSON.stringify(value)}: ${result.stderr}`);
      assert.match(result.stderr, /--concurrency must be a positive integer/);
      assert.doesNotMatch(result.stderr, /ENOENT|langgraph\.json|connect ECONN|execution pool/);
      assert.equal(result.stdout, "");
    }
  }
  const missing = invoke(["--concurrency"]);
  assert.equal(missing.status, 1);
  assert.match(missing.stderr, /--concurrency must be a positive integer/);
});

test("compiled CLI accepts safe decimal concurrency and gives the flag precedence over environment", () => {
  for (const value of ["1", "2", "01", "9007199254740991"]) {
    const result = invoke(["--concurrency", value, "--framework", "unsupported"], {
      RUNKITE_CONCURRENCY: "malformed",
    });
    assert.equal(result.status, 1);
    assert.match(result.stderr, /unsupported framework unsupported/);
    assert.doesNotMatch(result.stderr, /--concurrency must|ENOENT|langgraph\.json/);
  }
});
