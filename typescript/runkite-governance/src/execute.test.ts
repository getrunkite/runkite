import assert from "node:assert/strict";
import { test } from "node:test";
import {
  createGovernedExecute,
  GovernanceBlockedError,
  GovernanceError,
  type GovernanceCall,
  type GovernanceDecision,
  type GovernedExecuteConfig,
} from "./index.ts";

type Input = { title: string; nested?: { values: readonly unknown[] } };
type Options = {
  toolCallId: string;
  abortSignal?: AbortSignal;
  context?: object;
};
const identity = {
  agentId: "notes-app",
  connector: "workspace",
  tool: "createNote",
  runId: "turn-1:call-1",
};

test("allow decides once and preserves the executor result and framework options", async () => {
  const controller = new AbortController();
  const options: Options = {
    toolCallId: "call-1",
    abortSignal: controller.signal,
    context: {},
  };
  const input = {
    title: "Release plan",
    nested: { values: [null, "text", false, 1, { x: "y" }] },
  };
  const output = { noteId: "note-1" };
  let calls = 0;
  const guarded = createGovernedExecute<Input, typeof output, Options>({
    client: {
      async decide(call, signal) {
        calls += 1;
        assert.equal(signal, options.abortSignal);
        assert.equal(call.runId, "turn-1:call-1");
        assert.deepEqual(JSON.parse(JSON.stringify(call.args)), input);
        assert.equal(Object.hasOwn(call, "options"), false);
        return { effect: "allow" };
      },
    },
    identify(snapshot, receivedOptions) {
      assert.equal(snapshot.title, input.title);
      assert.equal(receivedOptions, options);
      return { ...identity, runId: "turn-1:" + receivedOptions.toolCallId };
    },
    execute(snapshot, receivedOptions) {
      assert.notEqual(snapshot, input);
      assert.equal(Object.getPrototypeOf(snapshot), null);
      assert.equal(receivedOptions, options);
      return output;
    },
    onDenied: () => assert.fail("allow called deny handler"),
    onPending: () => assert.fail("allow called pending handler"),
  });
  assert.equal(await guarded(input, options), output);
  assert.equal(calls, 1);
});

test("deny and pending invoke only their matching application callback", async () => {
  for (const effect of ["deny", "pending"] as const) {
    const calls: string[] = [];
    const guarded = createGovernedExecute<Input, string, Options>({
      client: {
        async decide() {
          calls.push("decide");
          return { effect, actionId: "action-1" };
        },
      },
      identify: () => identity,
      execute: () => assert.fail("blocked executor ran"),
      onDenied(snapshot, options, decision) {
        calls.push("deny");
        assert.equal(snapshot.title, "Release plan");
        assert.equal(options.toolCallId, "call-1");
        assert.equal(decision.effect, "deny");
        return "denied";
      },
      async onPending(snapshot, options, decision) {
        calls.push("pending");
        assert.equal(snapshot.title, "Release plan");
        assert.equal(options.toolCallId, "call-1");
        assert.equal(decision.actionId, "action-1");
        return "stored pending action";
      },
    });
    assert.equal(
      await guarded({ title: "Release plan" }, { toolCallId: "call-1" }),
      effect === "deny" ? "denied" : "stored pending action",
    );
    assert.deepEqual(calls, ["decide", effect]);
  }
});

test("blocked defaults expose a frozen copy of decision metadata only", async () => {
  for (const effect of ["deny", "pending"] as const) {
    const response = {
      effect,
      actionId: "action-1",
      reasonCode: "rule",
      args: { private: "not error metadata" },
    };
    const guarded = createGovernedExecute<Input, void, Options>({
      client: {
        async decide() {
          return response;
        },
      },
      identify: () => identity,
      execute: () => assert.fail("blocked executor ran"),
    });
    await assert.rejects(
      guarded({ title: "private argument" }, { toolCallId: "call-1" }),
      (error: unknown) => {
        assert.ok(error instanceof GovernanceBlockedError);
        assert.equal(error.decision.effect, effect);
        assert.equal(error.decision.reasonCode, "rule");
        assert.ok(Object.isFrozen(error.decision));
        assert.equal(Object.hasOwn(error.decision, "args"), false);
        assert.equal(JSON.stringify(error).includes("private"), false);
        return true;
      },
    );
  }
});

test("identity requires explicit nonempty operation fields before deciding", async () => {
  const invalid: unknown[] = [
    null,
    {},
    ...["agentId", "connector", "tool", "runId", "tenantId"].flatMap((key) => [
      { ...identity, [key]: "" },
      { ...identity, [key]: "  " },
      { ...identity, [key]: 7 },
      { ...identity, [key]: "a".repeat(257) },
      { ...identity, [key]: "é".repeat(129) },
      { ...identity, [key]: " leading" },
      { ...identity, [key]: "trailing " },
      { ...identity, [key]: "\u0085trimmed-by-server" },
      { ...identity, [key]: "invalid\ud800" },
    ]),
  ];
  let decisions = 0;
  for (const value of invalid) {
    const guarded = createGovernedExecute<Input, void, Options>({
      client: {
        async decide() {
          decisions += 1;
          return { effect: "allow" };
        },
      },
      identify: () => value as typeof identity,
      execute: () => assert.fail("invalid identity executed"),
    });
    await assert.rejects(
      guarded({ title: "Release plan" }, { toolCallId: "call-1" }),
      GovernanceError,
    );
  }
  assert.equal(decisions, 0);
});

test("identity cannot supply replacement arguments", async () => {
  let submitted: GovernanceCall | undefined;
  const guarded = createGovernedExecute<Input, string, Options>({
    client: {
      async decide(call) {
        submitted = call;
        return { effect: "allow" };
      },
    },
    identify: () => ({ ...identity, args: { title: "replacement" } }),
    execute: (snapshot) => snapshot.title,
  });
  assert.equal(
    await guarded({ title: "authorized" }, { toolCallId: "call-1" }),
    "authorized",
  );
  assert.ok(submitted);
  assert.equal((submitted.args as Input).title, "authorized");
});

test("an already aborted invocation does not identify or decide", async () => {
  const controller = new AbortController();
  const failure = new Error("caller closed request");
  controller.abort(failure);
  const guarded = createGovernedExecute<Input, void, Options>({
    client: { decide: () => assert.fail("aborted invocation decided") },
    identify: () => assert.fail("aborted invocation identified"),
    execute: () => assert.fail("aborted invocation executed"),
  });
  await assert.rejects(
    guarded(
      { title: "Release plan" },
      { toolCallId: "call-1", abortSignal: controller.signal },
    ),
    (error: unknown) => error === failure,
  );
});

test("cancellation during synchronous identity construction does not decide", async () => {
  const controller = new AbortController();
  const guarded = createGovernedExecute<Input, void, Options>({
    client: { decide: () => assert.fail("aborted invocation decided") },
    identify() {
      controller.abort();
      return identity;
    },
    execute: () => assert.fail("aborted invocation executed"),
  });
  await assert.rejects(
    guarded(
      { title: "Release plan" },
      { toolCallId: "call-1", abortSignal: controller.signal },
    ),
    { name: "AbortError" },
  );
});

test("decision and application callback failures propagate without retry", async () => {
  const failure = new Error("unavailable or execution result unknown");
  for (const branch of ["decide", "allow", "deny", "pending"] as const) {
    const calls: string[] = [];
    const callback = () => {
      calls.push(branch);
      throw failure;
    };
    const guarded = createGovernedExecute<Input, void, Options>({
      client: {
        async decide() {
          calls.push("decide");
          if (branch === "decide") throw failure;
          return { effect: branch, actionId: "action-1" };
        },
      },
      identify: () => identity,
      execute: callback,
      onDenied: callback,
      onPending: callback,
    });
    await assert.rejects(
      guarded({ title: "Release plan" }, { toolCallId: "call-1" }),
      (error: unknown) => error === failure,
    );
    assert.deepEqual(
      calls,
      branch === "decide" ? ["decide"] : ["decide", branch],
    );
  }
});

test("the factory captures callbacks and the client's original method", async () => {
  let release!: (decision: GovernanceDecision) => void;
  const response = new Promise<GovernanceDecision>((resolve) => {
    release = resolve;
  });
  const calls: string[] = [];
  const config: GovernedExecuteConfig<Input, string, Options> = {
    client: {
      decide() {
        calls.push("original decide");
        return response;
      },
    },
    identify: () => identity,
    execute: () => "original executor",
  };
  const guarded = createGovernedExecute(config);
  const result = guarded({ title: "Release plan" }, { toolCallId: "call-1" });
  config.client.decide = () => assert.fail("replacement client called");
  config.execute = () => assert.fail("replacement executor called");
  config.identify = () => assert.fail("replacement identity called");
  release({ effect: "allow" });
  assert.equal(await result, "original executor");
  assert.deepEqual(calls, ["original decide"]);
});

test("repeated calls leave execution idempotency to the application", async () => {
  let decisions = 0;
  let executions = 0;
  const guarded = createGovernedExecute<Input, number, Options>({
    client: {
      async decide() {
        decisions += 1;
        return { effect: "allow" };
      },
    },
    identify: () => identity,
    execute: () => ++executions,
  });
  assert.equal(
    await guarded({ title: "Release plan" }, { toolCallId: "call-1" }),
    1,
  );
  assert.equal(
    await guarded({ title: "Release plan" }, { toolCallId: "call-1" }),
    2,
  );
  assert.equal(decisions, 2);
});

test("streaming executor and callback results are rejected without consuming the stream", async () => {
  for (const effect of ["allow", "deny", "pending"] as const) {
    let consumed = 0;
    async function* stream() {
      consumed += 1;
      yield "chunk";
    }
    const guarded = createGovernedExecute<
      Input,
      AsyncIterable<string>,
      Options
    >({
      client: {
        async decide() {
          return { effect, actionId: "action-1" };
        },
      },
      identify: () => identity,
      execute: stream,
      onDenied: stream,
      onPending: stream,
    });
    await assert.rejects(
      guarded({ title: "Release plan" }, { toolCallId: "call-1" }),
      /streaming tool results/,
    );
    assert.equal(consumed, 0);
  }
});

test("JSON strings and keys must survive the server's Unicode decoding unchanged", async () => {
  let decisions = 0;
  const guarded = createGovernedExecute<unknown, unknown, Options>({
    client: {
      async decide() {
        decisions += 1;
        return { effect: "allow" };
      },
    },
    identify: () => identity,
    execute: (snapshot) => snapshot,
  });
  for (const value of [
    { text: "\ud800" },
    { text: "\udc00" },
    { ["\ud800"]: "key" },
    { n: -0 },
  ]) {
    await assert.rejects(
      guarded(value, { toolCallId: "call-1" }),
      /lossless JSON/,
    );
  }
  assert.equal(decisions, 0);
  const valid = {
    title: "emoji \ud83d\ude80",
    nested: ["\u0000", "", "\ufffd"],
  };
  assert.equal(
    JSON.stringify(await guarded(valid, { toolCallId: "call-1" })),
    JSON.stringify(valid),
  );
  assert.equal(decisions, 1);
});

test("custom decision metadata is validated without invoking response accessors", async () => {
  let getterCalls = 0;
  const getter = Object.defineProperty({ effect: "allow" }, "reason", {
    enumerable: true,
    get() {
      getterCalls += 1;
      return "grant";
    },
  });
  for (const response of [
    getter,
    { effect: "allow", reasonCode: 1 },
    { effect: "pending", actionId: "   " },
  ]) {
    const guarded = createGovernedExecute<Input, void, Options>({
      client: {
        async decide() {
          return response as GovernanceDecision;
        },
      },
      identify: () => identity,
      execute: () => assert.fail("malformed response executed"),
      onPending: () => assert.fail("malformed response persisted"),
    });
    await assert.rejects(
      guarded({ title: "Release plan" }, { toolCallId: "call-1" }),
      GovernanceError,
    );
  }
  assert.equal(getterCalls, 0);
});
