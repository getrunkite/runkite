/** Optional real npm artifact, SQL plane and individual mTLS holder. No model API. */
import assert from "node:assert/strict";
import { execFile, spawn, type ChildProcess } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import { once } from "node:events";
import { copyFile, mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { request as httpsRequest } from "node:https";
import { createServer } from "node:net";
import os from "node:os";
import path from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { credentials, loadPackageDefinition, Metadata, status as grpcStatus } from "@grpc/grpc-js";
import { loadSync } from "@grpc/proto-loader";

const binary = process.env.RUNKITE_TEST_WORKLOAD_BINARY;
const runnerDirectory = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const execute = promisify(execFile);
const uri = "spiffe://fixture.test/runner/typescript";
type ProcessHandle = { child: ChildProcess; log: () => string; stop: () => Promise<void> };

function launch(command: string, args: string[], cwd: string, env: NodeJS.ProcessEnv): ProcessHandle {
  const child = spawn(command, args, { cwd, env, stdio: ["ignore", "pipe", "pipe"] });
  let output = "";
  const append = (chunk: Buffer) => {
    output = (output + chunk.toString()).slice(-64_000);
  };
  child.stdout?.on("data", append);
  child.stderr?.on("data", append);
  child.on("error", (error) => {
    output += `\nspawn failed: ${error.message}`;
  });
  return {
    child,
    log: () => output,
    async stop() {
      if (!child.pid || child.exitCode !== null || child.signalCode !== null) return;
      const exited = once(child, "exit").catch(() => {});
      child.kill("SIGTERM");
      await Promise.race([exited, delay(1500)]);
      if (child.exitCode === null && child.signalCode === null) {
        child.kill("SIGKILL");
        await exited;
      }
    },
  };
}

async function freePort(): Promise<number> {
  const server = createServer();
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  assert.ok(address && typeof address !== "string");
  await new Promise<void>((resolve, reject) => server.close((error) => (error ? reject(error) : resolve())));
  return address.port;
}

async function waitFor<T>(
  description: string,
  operation: () => Promise<T | false>,
  timeout = 25_000,
  alive?: () => boolean,
): Promise<T> {
  const end = Date.now() + timeout;
  let error: unknown;
  while (Date.now() < end) {
    if (alive && !alive()) throw new Error(`Process exited while waiting for ${description}`);
    try {
      const value = await operation();
      if (value !== false) return value;
    } catch (caught) {
      error = caught;
    }
    await delay(40);
  }
  throw new Error(`Timed out waiting for ${description}`, { cause: error });
}

test(
  "installed TypeScript npm artifact owns strict mTLS runs and rejects a same-URI peer key",
  {
    skip: binary ? false : "Set RUNKITE_TEST_WORKLOAD_BINARY for real packaged strict-mTLS qualification",
    timeout: 180_000,
  },
  async (t) => {
    const directory = await mkdtemp(path.join(os.tmpdir(), "runkite-ts-workload-"));
    let plane: ProcessHandle | undefined;
    let runner: ProcessHandle | undefined;
    let peer: { close(): void } | undefined;
    const report: Record<string, unknown> = {
      passed: false,
      profile: "packaged TypeScript AI SDK + SQLite/inprocess + direct mTLS",
      plane_mode: "dev",
      node: process.version,
      bundled_undici: process.versions.undici ?? "unknown",
      cases: [],
    };
    const cases = report.cases as string[];
    t.after(async () => {
      peer?.close();
      await runner?.stop();
      await plane?.stop();
      if (report.passed !== true) t.diagnostic(`Control plane:\n${plane?.log()}\nPackaged worker:\n${runner?.log()}`);
      const output = process.env.RUNKITE_TEST_WORKLOAD_OUTPUT;
      if (output) {
        await mkdir(path.dirname(path.resolve(output)), { recursive: true });
        await writeFile(output, JSON.stringify(report, null, 2) + "\n");
        await writeFile(output + ".plane.log", plane?.log() ?? "plane not started\n");
        await writeFile(output + ".runner.log", runner?.log() ?? "runner not started\n");
      }
      await rm(directory, { recursive: true, force: true });
    });
    const home = path.join(directory, "home");
    await mkdir(home);
    const environment: NodeJS.ProcessEnv = {
      PATH: process.env.PATH,
      HOME: home,
      TMPDIR: process.env.TMPDIR,
      LANG: "C.UTF-8",
    };
    const tool = (command: string, args: string[], cwd = directory, extra: NodeJS.ProcessEnv = {}) =>
      execute(command, args, { cwd, env: { ...environment, ...extra }, timeout: 90_000, maxBuffer: 2_000_000 });
    report.plane_version = (await tool(path.resolve(binary!), ["version"])).stdout.trim();

    // Pin the CA extension on the actual req -x509 invocation. Leaf extensions
    // are separately applied by x509 -req, never inferred from the CSR's CN.
    // RSA avoids explicit-curve EC encoding from older system LibreSSL tools.
    await writeFile(
      path.join(directory, "ca.conf"),
      "[req]\nprompt=no\ndistinguished_name=dn\nx509_extensions=ca\n[dn]\nCN=Runkite synthetic fixture CA\n[ca]\nbasicConstraints=critical,CA:TRUE\nkeyUsage=critical,keyCertSign,cRLSign\n",
    );
    await tool("openssl", [
      "req",
      "-x509",
      "-sha256",
      "-newkey",
      "rsa:2048",
      "-nodes",
      "-days",
      "1",
      "-config",
      "ca.conf",
      "-keyout",
      "ca.key",
      "-out",
      "ca.pem",
    ]);
    for (const name of ["server", "a", "b"]) {
      await tool("openssl", [
        "req",
        "-new",
        "-sha256",
        "-newkey",
        "rsa:2048",
        "-nodes",
        "-subj",
        `/CN=synthetic-${name}`,
        "-keyout",
        `${name}.key`,
        "-out",
        `${name}.csr`,
      ]);
      const ext =
        "basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\n" +
        (name === "server"
          ? "extendedKeyUsage=serverAuth\nsubjectAltName=IP:127.0.0.1,DNS:localhost\n"
          : `extendedKeyUsage=clientAuth\nsubjectAltName=URI:${uri}\n`);
      await writeFile(path.join(directory, `${name}.ext`), ext);
      await tool("openssl", [
        "x509",
        "-sha256",
        "-req",
        "-in",
        `${name}.csr`,
        "-CA",
        "ca.pem",
        "-CAkey",
        "ca.key",
        "-CAcreateserial",
        "-days",
        "1",
        "-extfile",
        `${name}.ext`,
        "-out",
        `${name}.pem`,
      ]);
    }
    const ca = await readFile(path.join(directory, "ca.pem"));
    const clients = Object.fromEntries(
      await Promise.all(
        ["a", "b"].map(async (name) => [
          name,
          {
            cert: await readFile(path.join(directory, `${name}.pem`)),
            key: await readFile(path.join(directory, `${name}.key`)),
          },
        ]),
      ),
    );
    const adminKey = randomBytes(32).toString("hex");
    const runnerToken = randomBytes(32).toString("hex");
    const release = path.join(directory, "release");
    const calls = path.join(directory, "calls.jsonl");
    const consumer = path.join(directory, "consumer");
    await mkdir(consumer);
    await writeFile(path.join(consumer, "package.json"), JSON.stringify({ private: true, type: "module" }));
    await writeFile(path.join(directory, "user.npmrc"), "");
    await writeFile(path.join(directory, "global.npmrc"), "");
    const npmEnvironment = {
      NPM_CONFIG_CACHE: process.env.NPM_CONFIG_CACHE ?? process.env.npm_config_cache ?? path.join(home, "npm-cache"),
      NPM_CONFIG_USERCONFIG: path.join(directory, "user.npmrc"),
      NPM_CONFIG_GLOBALCONFIG: path.join(directory, "global.npmrc"),
      NPM_CONFIG_REGISTRY: "https://registry.npmjs.org/",
    };
    const expectedPackage = JSON.parse(await readFile(path.join(runnerDirectory, "package.json"), "utf8"));
    const suppliedArchive = process.env.RUNKITE_TEST_WORKLOAD_NPM_ARTIFACT;
    let archive: string;
    let packedIntegrity: string | undefined;
    if (suppliedArchive) {
      archive = path.join(directory, "supplied-runkite-runner.tgz");
      await copyFile(path.resolve(suppliedArchive), archive);
    } else {
      const packed = await tool(
        "npm",
        ["pack", "--ignore-scripts", "--json", "--pack-destination", directory],
        runnerDirectory,
        npmEnvironment,
      );
      const metadata = JSON.parse(packed.stdout);
      assert.equal(metadata.length, 1);
      assert.equal(metadata[0].name, expectedPackage.name);
      assert.equal(metadata[0].version, expectedPackage.version);
      archive = path.join(directory, metadata[0].filename);
      packedIntegrity = metadata[0].integrity;
    }
    const integrity =
      "sha512-" +
      createHash("sha512")
        .update(await readFile(archive))
        .digest("base64");
    if (packedIntegrity) assert.equal(integrity, packedIntegrity, "npm pack integrity differs from archive bytes");
    report.npm_artifact = {
      name: expectedPackage.name,
      version: expectedPackage.version,
      integrity,
      source: suppliedArchive ? "supplied_archive_copy" : "self_packed",
    };
    await tool(
      "npm",
      [
        "install",
        process.env.RUNKITE_TEST_WORKLOAD_NPM_OFFLINE === "1" ? "--offline" : "--prefer-offline",
        "--ignore-scripts",
        "--no-package-lock",
        "--no-audit",
        "--no-fund",
        archive,
        "ai@7.0.155",
      ],
      consumer,
      npmEnvironment,
    );
    const packagedRunner = path.join(consumer, "node_modules/runkite-runner");
    const installedPackage = JSON.parse(await readFile(path.join(packagedRunner, "package.json"), "utf8"));
    assert.equal(installedPackage.name, expectedPackage.name, "installed archive package name must match source");
    assert.equal(installedPackage.version, expectedPackage.version, "installed archive version must match source");
    assert.ok(
      (await readFile(path.join(packagedRunner, "dist/tls.js"), "utf8")).includes("HTTPSOnlyAgent"),
      "build current distribution files before packing",
    );
    report.installed_dependencies = JSON.parse(
      (await tool("npm", ["ls", "--depth=0", "--json"], consumer, npmEnvironment)).stdout,
    ).dependencies;
    cases.push("actual npm archive installed outside the source tree with its declared dependencies");
    await writeFile(
      path.join(consumer, "agent.mjs"),
      `
import { appendFileSync, existsSync } from 'node:fs';
import { MockLanguageModelV4 } from 'ai/test';
import { runnerHeaders } from 'runkite-runner/dist/tenantCtx.js';
export function agent(context) {
 return { maxRetries: 0, model: new MockLanguageModelV4({ modelId: 'offline-strict-mtls', doStream: async ({ prompt }) => ({ stream: new ReadableStream({ async start(controller) {
  const users = prompt.filter(message => message.role === 'user').length;
  appendFileSync(process.env.E2E_CALLS_FILE, JSON.stringify({runId:context.runId,users,runIncarnationId:runnerHeaders()['X-Runkite-Run-Incarnation']}) + '\\n');
  controller.enqueue({type:'stream-start',warnings:[]});
  controller.enqueue({type:'text-start',id:'text'});
  controller.enqueue({type:'text-delta',id:'text',delta:'Strict mTLS history ' + users});
  while (!existsSync(process.env.E2E_RELEASE_FILE) && !context.signal.aborted) await new Promise(resolve => setTimeout(resolve,20));
  if (!context.signal.aborted) {
   controller.enqueue({type:'text-end',id:'text'});
   controller.enqueue({type:'finish',finishReason:{unified:'stop',raw:'stop'},usage:{inputTokens:{total:3,noCache:3,cacheRead:0,cacheWrite:0},outputTokens:{total:1,text:1,reasoning:0}}});
  }
  controller.close();
 }}) }) }) };
}
`,
    );
    const config = path.join(consumer, "runkite.json");
    await writeFile(
      config,
      JSON.stringify({
        runner_kind: "typescript-ai-sdk",
        graphs: { strict_echo: "./agent.mjs:agent" },
        auth: {
          type: "api_key",
          strict_permissions: true,
          keys: { [adminKey]: { name: "synthetic-admin", tenant_id: "default", permissions: ["admin"] } },
        },
      }),
    );
    const allowlist = path.join(directory, "identities.json");
    await writeFile(allowlist, JSON.stringify({ identities: { "typescript-ai-sdk": [uri] }, denied_key_sha256: [] }));
    const [httpPort, grpcPort] = await Promise.all([freePort(), freePort()]);
    const base = `https://127.0.0.1:${httpPort}`;
    const tlsEnvironment = {
      TLS_CERT_FILE: path.join(directory, "server.pem"),
      TLS_KEY_FILE: path.join(directory, "server.key"),
      TLS_CLIENT_CA_FILE: path.join(directory, "ca.pem"),
      GRPC_TLS_CERT_FILE: path.join(directory, "server.pem"),
      GRPC_TLS_KEY_FILE: path.join(directory, "server.key"),
      GRPC_TLS_CLIENT_CA_FILE: path.join(directory, "ca.pem"),
    };
    plane = launch(
      path.resolve(binary!),
      ["dev", "--config", config, "--port", String(httpPort), "--grpc-port", String(grpcPort)],
      consumer,
      {
        ...environment,
        ...tlsEnvironment,
        DATABASE_PATH: path.join(directory, "state.db"),
        LOG_FORMAT: "json",
        RUNNER_TOKEN_TYPESCRIPT_AI_SDK: runnerToken,
        RUNNER_TENANTS_TYPESCRIPT_AI_SDK: "default",
        RUNKITE_WORKLOAD_IDENTITY: "mtls",
        RUNKITE_WORKLOAD_IDENTITIES_FILE: allowlist,
        RUNKITE_WORKLOAD_MAX_CERT_TTL: "24h",
      },
    );
    const request = (
      identity: "a" | "b",
      route: string,
      body?: unknown,
      headers: Record<string, string> = { Authorization: `Bearer ${adminKey}` },
    ) =>
      new Promise<{ status: number; json: any; text: string }>((resolve, reject) => {
        const req = httpsRequest(
          base + route,
          {
            ca,
            ...clients[identity],
            method: body === undefined ? "GET" : "POST",
            headers: { "Content-Type": "application/json", ...headers },
            timeout: 5000,
          },
          (res) => {
            const parts: Buffer[] = [];
            res.on("data", (chunk: Buffer) => parts.push(chunk));
            res.on("end", () => {
              const text = Buffer.concat(parts).toString();
              let json: unknown;
              try {
                json = text ? JSON.parse(text) : null;
              } catch {
                json = null;
              }
              resolve({ status: res.statusCode ?? 0, json, text });
            });
          },
        );
        req.on("error", reject);
        req.on("timeout", () => req.destroy(new Error("synthetic fixture HTTP deadline")));
        req.end(body === undefined ? undefined : JSON.stringify(body));
      });
    const api = async (route: string, body?: unknown) => {
      const response = await request("a", route, body);
      assert.equal(response.status, 200, `${route}: ${response.status} ${response.text}`);
      return response.json;
    };
    await waitFor(
      "strict control-plane readiness",
      async () => (await request("a", "/readyz")).status === 200 || false,
      25_000,
      () => Boolean(plane!.child.pid) && plane!.child.exitCode === null && plane!.child.signalCode === null,
    );
    runner = launch(
      process.execPath,
      [
        path.join(packagedRunner, "dist/cli.js"),
        "--framework",
        "ai-sdk",
        "--config",
        config,
        "--grpc-address",
        `localhost:${grpcPort}`,
        "--http-address",
        base,
      ],
      consumer,
      {
        ...environment,
        RUNNER_TOKEN: runnerToken,
        RUNKITE_HTTP_URL: base,
        RUNKITE_TLS_CA_FILE: path.join(directory, "ca.pem"),
        RUNKITE_TLS_CLIENT_CERT_FILE: path.join(directory, "a.pem"),
        RUNKITE_TLS_CLIENT_KEY_FILE: path.join(directory, "a.key"),
        E2E_RELEASE_FILE: release,
        E2E_CALLS_FILE: calls,
      },
    );
    await waitFor("packaged worker startup", async () => {
      assert.equal(runner!.child.exitCode, null, runner!.log());
      return runner!.log().includes("Worker ready") || false;
    });
    const thread = (await api("/threads", {})).thread_id;
    const created = await api(`/threads/${thread}/runs`, {
      agent_id: "strict_echo",
      input: { messages: [{ role: "user", content: "hold until peer checks complete" }] },
      stream_mode: ["values", "messages"],
    });
    const run = created.run_id;
    await waitFor(
      "offline model after HTTPS preflight and checkpoint load",
      async () => (await readFile(calls, "utf8")).includes(run) || false,
    );
    const current = await api(`/threads/${thread}/runs/${run}`);
    const owner = current.metadata._runkite_workload_owner;
    assert.equal(owner.uri, uri);
    assert.match(owner.key_sha256, /^[0-9a-f]{64}$/);
    assert.equal(current.metadata._runkite_attempt_generation, 1);
    const runIncarnationId = (await readFile(calls, "utf8"))
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line))
      .find((call) => call.runId === run)?.runIncarnationId;
    assert.match(runIncarnationId, /^[\x21-\x7e]{1,256}$/);
    const runnerHeaders = {
      "X-Runkite-Run-Incarnation": runIncarnationId,
      "X-Runner-Kind": "typescript-ai-sdk",
      "X-Runner-Token": runnerToken,
      "X-Runkite-Run-Id": run,
      "X-Runkite-Generation": "1",
    };
    assert.equal((await request("a", `/internal/runs/${run}/status`, undefined, runnerHeaders)).status, 200);
    assert.equal((await request("b", `/internal/runs/${run}/status`, undefined, runnerHeaders)).status, 403);
    assert.equal((await request("b", `/internal/checkpoints/${thread}/latest`, undefined, runnerHeaders)).status, 403);
    const definitions = loadPackageDefinition(
      loadSync(path.join(packagedRunner, "proto/runner.proto"), { keepCase: false, longs: String, defaults: true }),
    ) as any;
    peer = new definitions.runkite.runner.v0.RunnerService(
      `localhost:${grpcPort}`,
      credentials.createSsl(ca, clients.b.key, clients.b.cert),
    );
    const metadataHeader = new Metadata();
    metadataHeader.set("runner-kind", "typescript-ai-sdk");
    metadataHeader.set("runner-token", runnerToken);
    for (const [method, body] of [
      ["heartbeat", { runId: run, runIncarnationId, generation: "1" }],
      ["reportStatus", { runId: run, runIncarnationId, generation: "1", status: "success", errorMessage: "" }],
    ] as const) {
      const error = await new Promise<any>((resolve) =>
        (peer as any)[method](body, metadataHeader, { deadline: Date.now() + 5000 }, (caught: unknown) =>
          resolve(caught),
        ),
      );
      assert.equal(error?.code, grpcStatus.PERMISSION_DENIED, "same-URI peer borrowed the owner's assignment");
    }
    assert.notEqual((await api(`/threads/${thread}/runs/${run}`)).status, "success");
    cases.push(
      "real verified URI and public-key owner survives HTTP/gRPC; same-URI different-key peer cannot read checkpoints, heartbeat or complete",
    );
    await writeFile(release, "release");
    const final = await waitFor("durable packaged worker completion", async () => {
      const value = await api(`/threads/${thread}/runs/${run}`);
      return ["success", "error", "interrupted"].includes(value.status) ? value : false;
    });
    assert.equal(final.status, "success", JSON.stringify(final));
    assert.equal(final.output.messages.at(-1).content[0].text, "Strict mTLS history 1");
    assert.equal(final.output.usage.prompt_tokens, 3);
    assert.equal(final.output.usage.completion_tokens, 1);
    assert.deepEqual((await api(`/threads/${thread}`)).values, final.output);
    cases.push(
      "packaged worker completes through direct HTTPS opaque persistence and gRPC, with durable output and native usage",
    );
    const second = await api(`/threads/${thread}/runs`, {
      agent_id: "strict_echo",
      input: { messages: [{ role: "user", content: "restore the committed conversation" }] },
    });
    const restored = await waitFor("HTTPS checkpoint continuation", async () => {
      const value = await api(`/threads/${thread}/runs/${second.run_id}`);
      return ["success", "error", "interrupted"].includes(value.status) ? value : false;
    });
    assert.equal(restored.status, "success", JSON.stringify(restored));
    assert.equal(restored.output.messages.at(-1).content[0].text, "Strict mTLS history 2");
    cases.push("second managed turn restores the durable opaque conversation through certificate-bound HTTPS");
    report.passed = true;
  },
);
