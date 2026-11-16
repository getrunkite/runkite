#!/usr/bin/env node
// Exercise the actual installed public tarball outside the source tree.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import {
  mkdtempSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  realpathSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const source = join(root, "typescript/runkite-governance");
const tarball = process.argv[2] && resolve(process.argv[2]);
assert.ok(
  tarball?.endsWith(".tgz"),
  "Pass the already built governance tarball",
);
const sourcePackage = JSON.parse(
  readFileSync(join(source, "package.json"), "utf8"),
);
const allowed = [
  "LICENSE",
  "NOTICE",
  "README.md",
  "package.json",
  "dist/index.js",
  "dist/index.d.ts",
].sort();
const files = execFileSync("tar", ["-tzf", tarball], { encoding: "utf8" })
  .trim()
  .split("\n");
assert.deepEqual(
  files.map((file) => file.replace(/^package\//, "")).sort(),
  allowed,
);
for (const file of allowed) {
  const data = execFileSync("tar", ["-xOf", tarball, `package/${file}`]);
  assert.deepEqual(
    data,
    readFileSync(join(source, file)),
    `Packaged ${file} differs from built source`,
  );
}
assert.equal(sourcePackage.license, "Apache-2.0");
const temporary = realpathSync(
  mkdtempSync(join(tmpdir(), "runkite-governance-npm-")),
);
const env = { ...process.env, NODE_PATH: "", npm_config_offline: "true" };
function run(command, args) {
  execFileSync(command, args, {
    cwd: temporary,
    env,
    stdio: "inherit",
    timeout: 120_000,
  });
}
try {
  writeFileSync(
    join(temporary, "package.json"),
    JSON.stringify({ private: true, type: "module" }),
  );
  run("npm", [
    "install",
    "--offline",
    "--ignore-scripts",
    "--no-audit",
    "--no-fund",
    "--no-package-lock",
    tarball,
  ]);
  const installed = join(temporary, "node_modules/runkite-governance");
  assert.equal(
    JSON.parse(readFileSync(join(installed, "package.json"))).version,
    sourcePackage.version,
  );
  assert.deepEqual(readdirSync(installed).sort(), [
    "LICENSE",
    "NOTICE",
    "README.md",
    "dist",
    "package.json",
  ]);
  const tests = readdirSync(join(source, "src")).filter((name) =>
    name.endsWith(".test.ts"),
  );
  for (const name of tests) {
    const original = readFileSync(join(source, "src", name), "utf8");
    assert.ok(
      original.includes('"./index.ts"'),
      `Expected helper import in ${name}`,
    );
    writeFileSync(
      join(temporary, name),
      original.replaceAll('"./index.ts"', '"runkite-governance"'),
    );
  }
  // The import origin is checked before running the existing security and HTTP
  // tests against installed JavaScript, never the checkout's TypeScript module.
  writeFileSync(
    join(temporary, "origin.mjs"),
    `
    import assert from "node:assert/strict";
    import { fileURLToPath } from "node:url";
    const actual = fileURLToPath(import.meta.resolve("runkite-governance"));
    assert.equal(actual, ${JSON.stringify(join(installed, "dist/index.js"))});
  `,
  );
  run(process.execPath, ["origin.mjs"]);
  run(process.execPath, ["--experimental-strip-types", "--test", ...tests]);
  writeFileSync(
    join(temporary, "consumer.mts"),
    `
    import { createGovernanceClient, createGovernedExecute } from "runkite-governance";
    const client = createGovernanceClient({ baseUrl: "https://plane.example" });
    const guarded = createGovernedExecute({
      client, identify: (_input: Readonly<{ value: string }>) => ({
        agentId: "app", connector: "notes", tool: "write", runId: "operation-1"
      }), execute: (input: Readonly<{ value: string }>) => input.value
    });
    const result: Promise<string> = guarded({ value: "text" }, {});
    void result;
  `,
  );
  run(join(source, "node_modules/.bin/tsc"), [
    "--noEmit",
    "--module",
    "nodenext",
    "--target",
    "ES2022",
    "--lib",
    "ES2022,DOM",
    "--skipLibCheck",
    "consumer.mts",
  ]);
  // Reuse the pinned real AI SDK already installed for the offline example.
  // Only dependencies are linked. Example source is copied to the consumer and
  // imports the freshly installed governance tarball through its public export.
  const example = join(root, "examples/governance_ai_sdk");
  assert.ok(
    readdirSync(join(example, "node_modules")).includes("ai"),
    "Install the pinned example dependencies first",
  );
  for (const name of readdirSync(join(example, "node_modules"))) {
    if (
      name !== "runkite-governance" &&
      name !== ".bin" &&
      name !== ".package-lock.json"
    ) {
      symlinkSync(
        join(example, "node_modules", name),
        join(temporary, "node_modules", name),
        "dir",
      );
    }
  }
  mkdirSync(join(temporary, "example"));
  for (const name of [
    "tools.ts",
    "state.ts",
    "tools.test.ts",
    "stream.test.ts",
    "state.test.ts",
    "model.ts",
    "model.test.ts",
  ]) {
    writeFileSync(
      join(temporary, "example", name),
      readFileSync(join(example, name)),
    );
  }
  run(process.execPath, [
    "--experimental-strip-types",
    "--test",
    "example/tools.test.ts",
    "example/stream.test.ts",
    "example/state.test.ts",
    "example/model.test.ts",
  ]);
  console.log(
    JSON.stringify({
      package: sourcePackage.name,
      version: sourcePackage.version,
      installedArtifact: true,
      fileCount: allowed.length,
      helperTests: tests.length,
      aiSdkIntegration: true,
    }),
  );
} finally {
  rmSync(temporary, { recursive: true, force: true });
}
