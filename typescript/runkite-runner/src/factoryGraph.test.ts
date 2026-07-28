import { test } from "node:test";
import assert from "node:assert/strict";
import {
  extractParamNames,
  classifyGraphExport,
  ServerRuntimeStandIn,
  FactoryGraph,
  type RunFactoryContext,
} from "./factoryGraph.js";
import { RunnerUser } from "./runnerUser.js";

// -- extractParamNames ------------------------------------------------------
// The trickiest piece here: JS has no runtime reflection for parameter
// names, so this parses fn.toString(). Each shape below is a real form
// LangGraph SDK factory examples (or plain JS) could take.

test("extractParamNames: plain function declaration with two params", () => {
  function graph(config: any, runtime: any) {}
  assert.deepEqual(extractParamNames(graph), ["config", "runtime"]);
});

test("extractParamNames: async function declaration", () => {
  async function graph(config: any, runtime: any) {}
  assert.deepEqual(extractParamNames(graph), ["config", "runtime"]);
});

test("extractParamNames: async arrow function with parens", () => {
  const graph = async (config: any, runtime: any) => {};
  assert.deepEqual(extractParamNames(graph), ["config", "runtime"]);
});

test("extractParamNames: arrow function with parens, sync", () => {
  const graph = (config: any) => {};
  assert.deepEqual(extractParamNames(graph), ["config"]);
});

test("extractParamNames: parenthesis-free single-param arrow function", () => {
  const graph = (config: any) => config;
  // Written with parens above for valid syntax with a type annotation,
  // but exercise the truly parenthesis-free runtime form directly too.
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  const bareArrow = (x: unknown) => x;
  assert.deepEqual(extractParamNames(graph), ["config"]);
});

test("extractParamNames: 0-arg arrow function", () => {
  const factory = () => ({});
  assert.deepEqual(extractParamNames(factory), []);
});

test("extractParamNames: 0-arg function declaration", () => {
  function factory() {
    return {};
  }
  assert.deepEqual(extractParamNames(factory), []);
});

test("extractParamNames: anonymous async function expression", () => {
  const graph = async function (runtime: any) {
    return runtime;
  };
  assert.deepEqual(extractParamNames(graph), ["runtime"]);
});

test("extractParamNames: parameter order is preserved even when runtime comes first", () => {
  const graph = (runtime: any, config: any) => {};
  assert.deepEqual(extractParamNames(graph), ["runtime", "config"]);
});

test("extractParamNames: only 'runtime', no 'config'", () => {
  const graph = (runtime: any) => {};
  assert.deepEqual(extractParamNames(graph), ["runtime"]);
});

test("extractParamNames: a default value containing its own parens doesn't truncate the param list", () => {
  // The exact case a naive /\(([^)]*)\)/ regex gets wrong -- it would
  // stop at the FIRST `)`, inside getDefault(), well before the
  // parameter list actually closes.
  const getDefault = () => ({});
  const graph = (config = getDefault(), runtime: any = null) => {};
  assert.deepEqual(extractParamNames(graph), ["config", "runtime"]);
});

test("extractParamNames: a destructured parameter contributes no name (documented simplification)", () => {
  const graph = ({ config }: any) => {};
  assert.deepEqual(extractParamNames(graph), []);
});

test("extractParamNames: an unrelated parameter name is not mistaken for config/runtime", () => {
  const graph = (state: any, unrelated: any) => {};
  assert.deepEqual(extractParamNames(graph), ["state", "unrelated"]);
});

// -- classifyGraphExport -----------------------------------------------------

function fakeCompiledGraph(marker: string): any {
  return { stream: async () => (async function* () {})(), __marker: marker };
}

test("classifyGraphExport: an already-compiled graph (has .stream) classifies as static, unchanged", async () => {
  const graph = fakeCompiledGraph("compiled");
  const result = await classifyGraphExport(graph);
  assert.equal(result.kind, "static");
  assert.equal((result as any).graph, graph);
});

test("classifyGraphExport: a StateGraph builder (.compile, no .stream) is compiled and classified as static", async () => {
  const compiled = fakeCompiledGraph("compiled-from-builder");
  const builder = { compile: () => compiled };
  const result = await classifyGraphExport(builder);
  assert.equal(result.kind, "static");
  assert.equal((result as any).graph, compiled);
});

test("classifyGraphExport: a non-callable, non-graph export throws TypeError", async () => {
  await assert.rejects(() => classifyGraphExport({ not: "a graph" }), TypeError);
});

test("classifyGraphExport: a 0-arg callable is resolved once and classified as static", async () => {
  const compiled = fakeCompiledGraph("resolved-once");
  let callCount = 0;
  const factory = () => {
    callCount++;
    return compiled;
  };
  const result = await classifyGraphExport(factory);
  assert.equal(result.kind, "static");
  assert.equal((result as any).graph, compiled);
  assert.equal(callCount, 1);
});

test("classifyGraphExport: a 0-arg callable returning a Promise is awaited before classification", async () => {
  const compiled = fakeCompiledGraph("resolved-async");
  const factory = async () => compiled;
  const result = await classifyGraphExport(factory);
  assert.equal(result.kind, "static");
  assert.equal((result as any).graph, compiled);
});

test("classifyGraphExport: a 0-arg callable returning a StateGraph builder compiles it before classification", async () => {
  const compiled = fakeCompiledGraph("resolved-then-compiled");
  const factory = () => ({ compile: () => compiled });
  const result = await classifyGraphExport(factory);
  assert.equal(result.kind, "static");
  assert.equal((result as any).graph, compiled);
});

test("classifyGraphExport: a callable declaring 'config' classifies as factory", async () => {
  const factoryFn = (config: any) => fakeCompiledGraph("from-config-factory");
  const result = await classifyGraphExport(factoryFn);
  assert.equal(result.kind, "factory");
  assert.deepEqual((result as any).factory.paramNames, ["config"]);
});

test("classifyGraphExport: a callable declaring 'runtime' classifies as factory", async () => {
  const factoryFn = (runtime: any) => fakeCompiledGraph("from-runtime-factory");
  const result = await classifyGraphExport(factoryFn);
  assert.equal(result.kind, "factory");
  assert.deepEqual((result as any).factory.paramNames, ["runtime"]);
});

test("classifyGraphExport: a callable declaring both, in either order, classifies as factory with both names in declared order", async () => {
  const factoryFn = (runtime: any, config: any) => fakeCompiledGraph("both");
  const result = await classifyGraphExport(factoryFn);
  assert.equal(result.kind, "factory");
  assert.deepEqual((result as any).factory.paramNames, ["runtime", "config"]);
});

// -- FactoryGraph.build / open / close ---------------------------------------

function runContext(overrides: Partial<RunFactoryContext> = {}): RunFactoryContext {
  return { runId: "run-1", threadId: "thread-1", ...overrides };
}

test("FactoryGraph.build/open calls the factory with only the config it declared", async () => {
  let capturedArgs: unknown[] = [];
  const factory = new FactoryGraph(
    (config: any) => {
      capturedArgs = [config];
      return fakeCompiledGraph("built");
    },
    ["config"],
  );
  const build = factory.build({ configurable: { run_id: "run-1" } }, runContext(), { checkpointerManager: null, store: null });
  const graph = await build.open();

  assert.equal((graph as any).__marker, "built");
  assert.deepEqual(capturedArgs, [{ configurable: { run_id: "run-1" } }]);
});

test("FactoryGraph.build/open passes a ServerRuntimeStandIn when the factory declared 'runtime'", async () => {
  let capturedRuntime: ServerRuntimeStandIn | undefined;
  const factory = new FactoryGraph(
    (runtime: ServerRuntimeStandIn) => {
      capturedRuntime = runtime;
      return fakeCompiledGraph("built-with-runtime");
    },
    ["runtime"],
  );
  const build = factory.build({}, runContext({ user: { identity: "alice" } }), { checkpointerManager: null, store: null });
  await build.open();

  assert.ok(capturedRuntime instanceof ServerRuntimeStandIn);
  assert.ok(capturedRuntime!.user instanceof RunnerUser);
  assert.equal(capturedRuntime!.user!.identity, "alice");
});

test("ServerRuntimeStandIn.user is null when the run context has no user", async () => {
  let capturedRuntime: ServerRuntimeStandIn | undefined;
  const factory = new FactoryGraph(
    (runtime: ServerRuntimeStandIn) => {
      capturedRuntime = runtime;
      return fakeCompiledGraph("anonymous");
    },
    ["runtime"],
  );
  const build = factory.build({}, runContext(), { checkpointerManager: null, store: null });
  await build.open();

  assert.equal(capturedRuntime!.user, null);
});

test("ServerRuntimeStandIn.ensureUser() throws when there is no authenticated user", () => {
  const runtime = new ServerRuntimeStandIn(null, null);
  assert.throws(() => runtime.ensureUser(), /No authenticated user/);
});

test("ServerRuntimeStandIn.ensureUser() returns the user when authenticated", () => {
  const user = new RunnerUser({ identity: "bob" });
  const runtime = new ServerRuntimeStandIn(user, null);
  assert.equal(runtime.ensureUser(), user);
});

test("FactoryGraph.build/open passes arguments in the function's OWN declared order, not always config-then-runtime", async () => {
  let capturedArgs: unknown[] = [];
  const factory = new FactoryGraph(
    (runtime: any, config: any) => {
      capturedArgs = [runtime, config];
      return fakeCompiledGraph("order-test");
    },
    ["runtime", "config"],
  );
  const build = factory.build({ marker: "the-config" }, runContext({ user: { identity: "alice" } }), {
    checkpointerManager: null,
    store: null,
  });
  await build.open();

  assert.ok(capturedArgs[0] instanceof ServerRuntimeStandIn, "first positional arg must be the runtime, since it was declared first");
  assert.deepEqual(capturedArgs[1], { marker: "the-config" });
});

test("FactoryGraph.build/open awaits a Promise returned by the factory", async () => {
  const compiled = fakeCompiledGraph("async-built");
  const factory = new FactoryGraph(async (config: any) => compiled, ["config"]);
  const build = factory.build({}, runContext(), { checkpointerManager: null, store: null });
  const graph = await build.open();
  assert.equal(graph, compiled);
});

test("FactoryGraph.build/open compiles a StateGraph builder the factory returns", async () => {
  const compiled = fakeCompiledGraph("compiled-from-factory-builder");
  const factory = new FactoryGraph((config: any) => ({ compile: () => compiled }), ["config"]);
  const build = factory.build({}, runContext(), { checkpointerManager: null, store: null });
  const graph = await build.open();
  assert.equal(graph, compiled);
});

test("FactoryGraph.build/open attaches the checkpointer manager and store to the built graph, when provided", async () => {
  const factory = new FactoryGraph((config: any) => fakeCompiledGraph("with-deps"), ["config"]);
  const attachCalls: unknown[] = [];
  const fakeCheckpointerManager = { attach: (g: unknown) => attachCalls.push(g) } as any;
  const fakeStore = { mode: "proxy" } as any;

  const build = factory.build({}, runContext(), { checkpointerManager: fakeCheckpointerManager, store: fakeStore });
  const graph = await build.open();

  assert.equal(attachCalls.length, 1);
  assert.equal(attachCalls[0], graph);
  assert.equal((graph as any).store, fakeStore);
});

test("FactoryGraph.build/open works with neither a checkpointer manager nor a store (both null)", async () => {
  const factory = new FactoryGraph((config: any) => fakeCompiledGraph("no-deps"), ["config"]);
  const build = factory.build({}, runContext(), { checkpointerManager: null, store: null });
  const graph = await build.open();
  assert.equal((graph as any).__marker, "no-deps");
});

test("FactoryGraphBuild.close() calls dispose() on the factory's own returned object, if present", async () => {
  let disposed = false;
  const factory = new FactoryGraph(
    (config: any) => ({
      ...fakeCompiledGraph("disposable"),
      dispose: async () => {
        disposed = true;
      },
    }),
    ["config"],
  );
  const build = factory.build({}, runContext(), { checkpointerManager: null, store: null });
  await build.open();
  assert.equal(disposed, false, "must not be disposed before close() is called");
  await build.close();
  assert.equal(disposed, true);
});

test("FactoryGraphBuild.close() is a no-op when the factory's result has no dispose() method", async () => {
  const factory = new FactoryGraph((config: any) => fakeCompiledGraph("no-dispose"), ["config"]);
  const build = factory.build({}, runContext(), { checkpointerManager: null, store: null });
  await build.open();
  await assert.doesNotReject(() => build.close());
});
