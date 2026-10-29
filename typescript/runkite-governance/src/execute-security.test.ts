import assert from "node:assert/strict";
import { test } from "node:test";
import {
  createGovernedExecute,
  GovernanceBlockedError,
  type GovernanceCall,
  type GovernanceDecision,
  type GovernanceExecutionOptions,
} from "./index.ts";

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((settle) => {
    resolve = settle;
  });
  return { promise, resolve };
}

const identity = {
  agentId: "app-agent",
  connector: "workspace",
  tool: "transfer",
  runId: "app-turn-1-tool-1",
};

test("guard snapshots arguments and operation identity before its first await", async () => {
  const reply = deferred<GovernanceDecision>();
  const input = { Amount: 100, nested: { account: "original" } };
  const operation = { ...identity };
  let submitted!: GovernanceCall;
  let executions = 0;
  const guarded = createGovernedExecute<
    unknown,
    unknown,
    GovernanceExecutionOptions
  >({
    client: {
      decide(call) {
        submitted = call;
        assert.ok(Object.isFrozen(call));
        assert.ok(Object.isFrozen(call.args));
        assert.ok(Object.isFrozen((call.args as typeof input).nested));
        assert.equal(Reflect.set(call, "tool", "otherTool"), false);
        assert.equal(Reflect.set(call.args as object, "Amount", 5000), false);
        return reply.promise;
      },
    },
    identify: () => operation,
    execute(snapshot) {
      executions += 1;
      assert.ok(Object.isFrozen(snapshot));
      return snapshot;
    },
  });
  const result = guarded(input, {});
  input.Amount = 9000;
  input.nested.account = "replacement";
  operation.tool = "otherTool";
  operation.runId = "otherRun";
  reply.resolve({ effect: "allow" });
  assert.deepEqual(JSON.parse(JSON.stringify(await result)), {
    Amount: 100,
    nested: { account: "original" },
  });
  assert.equal(submitted.tool, "transfer");
  assert.equal(submitted.runId, "app-turn-1-tool-1");
  assert.equal(executions, 1);
});

test("guard rejects lossy JSON inputs without invoking accessors or toJSON", async () => {
  let inspected = 0;
  const accessor = Object.defineProperty({}, "amount", {
    enumerable: true,
    get() {
      inspected += 1;
      return 1;
    },
  });
  const hidden = Object.defineProperty({ amount: 1 }, "hidden", {
    value: "omitted",
  });
  const symbol = { amount: 1, [Symbol("hidden")]: "omitted" };
  const toJSON = {
    amount: 1,
    toJSON() {
      inspected += 1;
      return { amount: 0 };
    },
  };
  const cycle: Record<string, unknown> = { amount: 1 };
  cycle.self = cycle;
  const extraArray = Object.assign([1, 2], { hidden: "omitted" });
  const sparse = [1, 2, 3];
  delete sparse[1];
  const invalid: unknown[] = [
    accessor,
    hidden,
    symbol,
    toJSON,
    cycle,
    extraArray,
    sparse,
    { value: undefined },
    { value() {} },
    { value: BigInt(1) },
    { value: NaN },
    { value: Infinity },
    { value: -0 },
    { value: "\ud800" },
    { "\udfff": "invalid key" },
    { value: new Date("2026-01-01T00:00:00Z") },
    { value: new Map([["amount", 1]]) },
    Object.create({ inherited: "omitted" }),
  ];
  let decisions = 0;
  let executions = 0;
  const guarded = createGovernedExecute<
    unknown,
    unknown,
    GovernanceExecutionOptions
  >({
    client: {
      async decide() {
        decisions += 1;
        return { effect: "allow" };
      },
    },
    identify: () => identity,
    execute() {
      executions += 1;
    },
  });
  for (const value of invalid) {
    await assert.rejects(guarded(value, {}));
  }
  assert.equal(inspected, 0);
  assert.equal(decisions, 0);
  assert.equal(executions, 0);
});

test("guard rejects primitive roots that the plane could normalize differently", async () => {
  let decisions = 0;
  const guarded = createGovernedExecute<
    unknown,
    unknown,
    GovernanceExecutionOptions
  >({
    client: {
      async decide() {
        decisions += 1;
        return { effect: "allow" };
      },
    },
    identify: () => identity,
    execute() {
      assert.fail("non-object tool arguments");
    },
  });
  for (const input of [null, undefined, "", '{"amount":100}', 0, false, []]) {
    await assert.rejects(guarded(input, {}));
  }
  assert.equal(decisions, 0);
});

test("guard preserves case and prototype-like argument names as ordinary data", async () => {
  const input = JSON.parse(
    '{"__proto__":{"guardProof":"data"},"constructor":{"prototype":{"Amount":7}},"amount":1,"Amount":2,"unicode":"\ud83d\ude80","nested":[null,"text",true,1.5]}',
  );
  let submitted: unknown;
  const guarded = createGovernedExecute<
    unknown,
    unknown,
    GovernanceExecutionOptions
  >({
    client: {
      async decide(call) {
        submitted = call.args;
        return { effect: "allow" };
      },
    },
    identify: () => identity,
    execute: (snapshot) => snapshot,
  });
  const result = await guarded(input, {});
  assert.equal(JSON.stringify(submitted), JSON.stringify(input));
  assert.equal(JSON.stringify(result), JSON.stringify(input));
  assert.equal(
    (Object.prototype as Record<string, unknown>).guardProof,
    undefined,
  );
});

test("guard captures its executor when mutable configuration changes during authorization", async () => {
  const reply = deferred<GovernanceDecision>();
  let executions = 0;
  const configuration = {
    client: {
      decide() {
        return reply.promise;
      },
    },
    identify: () => identity,
    execute() {
      executions += 1;
      return "original executor";
    },
  };
  const guarded = createGovernedExecute<
    unknown,
    string,
    GovernanceExecutionOptions
  >(configuration);
  const result = guarded({ amount: 1 }, {});
  configuration.execute = () => {
    assert.fail("replacement executor");
  };
  reply.resolve({ effect: "allow" });
  assert.equal(await result, "original executor");
  assert.equal(executions, 1);
});

test("guard copies and freezes decision metadata before handing it to a blocked handler", async () => {
  const response: GovernanceDecision = {
    effect: "deny",
    reason: "grant denied",
  };
  const guarded = createGovernedExecute<
    unknown,
    string,
    GovernanceExecutionOptions
  >({
    client: {
      async decide() {
        return response;
      },
    },
    identify: () => identity,
    execute() {
      assert.fail("denied execution");
    },
    onDenied(_snapshot, _options, decision) {
      assert.notEqual(decision, response);
      assert.ok(Object.isFrozen(decision));
      assert.equal(Reflect.set(decision, "effect", "allow"), false);
      response.effect = "allow";
      response.reason = "replacement reason";
      assert.equal(decision.effect, "deny");
      assert.equal(decision.reason, "grant denied");
      return "blocked";
    },
  });
  assert.equal(await guarded({ amount: 1 }, {}), "blocked");
});

test("guard fails closed for malformed custom decision responses", async () => {
  const invalid: unknown[] = [
    undefined,
    null,
    {},
    { effect: "approved" },
    { effect: "ALLOW" },
    { effect: "pending" },
    { effect: "pending", actionId: "" },
  ];
  for (const decision of invalid) {
    let executions = 0;
    let handlers = 0;
    const guarded = createGovernedExecute<
      unknown,
      unknown,
      GovernanceExecutionOptions
    >({
      client: {
        async decide() {
          return decision as GovernanceDecision;
        },
      },
      identify: () => identity,
      execute() {
        executions += 1;
      },
      onPending() {
        handlers += 1;
      },
      onDenied() {
        handlers += 1;
      },
    });
    await assert.rejects(guarded({ amount: 1 }, {}));
    assert.equal(executions, 0);
    assert.equal(handlers, 0);
  }
});

test("guard does not expose arguments in a default blocked error", async () => {
  const secret = "private-argument-security-regression";
  for (const effect of ["deny", "pending"] as const) {
    const guarded = createGovernedExecute<
      unknown,
      unknown,
      GovernanceExecutionOptions
    >({
      client: {
        async decide() {
          return {
            effect,
            actionId: effect === "pending" ? "pending-1" : undefined,
          };
        },
      },
      identify: () => identity,
      execute() {
        assert.fail("blocked execution");
      },
    });
    await assert.rejects(guarded({ secret }, {}), (error: unknown) => {
      assert.ok(error instanceof GovernanceBlockedError);
      assert.equal(JSON.stringify(error).includes(secret), false);
      assert.equal(String(error).includes(secret), false);
      assert.equal(Object.hasOwn(error, "args"), false);
      return true;
    });
  }
});

test("guard observes cancellation before any callback after a late decision", async () => {
  for (const effect of ["allow", "deny", "pending"] as const) {
    const controller = new AbortController();
    let callbacks = 0;
    const guarded = createGovernedExecute<
      unknown,
      unknown,
      GovernanceExecutionOptions
    >({
      client: {
        async decide() {
          controller.abort(new Error("cancelled before tool invocation"));
          return {
            effect,
            actionId: effect === "pending" ? "pending-1" : undefined,
          };
        },
      },
      identify: () => identity,
      execute() {
        callbacks += 1;
      },
      onDenied() {
        callbacks += 1;
      },
      onPending() {
        callbacks += 1;
      },
    });
    await assert.rejects(
      guarded({ amount: 1 }, { abortSignal: controller.signal }),
      /cancelled before tool invocation/,
    );
    assert.equal(callbacks, 0);
  }
});

test("guard keeps the original cancellation signal when options mutate during a decision", async () => {
  const reply = deferred<GovernanceDecision>();
  const original = new AbortController();
  const replacement = new AbortController();
  const options = { abortSignal: original.signal };
  let executions = 0;
  const guarded = createGovernedExecute<
    unknown,
    unknown,
    GovernanceExecutionOptions
  >({
    client: {
      decide() {
        return reply.promise;
      },
    },
    identify: () => identity,
    execute() {
      executions += 1;
    },
  });
  const result = guarded({ amount: 1 }, options);
  options.abortSignal = replacement.signal;
  original.abort(new Error("original operation cancelled"));
  reply.resolve({ effect: "allow" });
  await assert.rejects(result, /original operation cancelled/);
  assert.equal(executions, 0);
});

test("guard never retries a callback that rejects after it starts", async () => {
  let executions = 0;
  let decisions = 0;
  const failure = new Error("side effect status unknown");
  const guarded = createGovernedExecute<
    unknown,
    unknown,
    GovernanceExecutionOptions
  >({
    client: {
      async decide() {
        decisions += 1;
        return { effect: "allow" };
      },
    },
    identify: () => identity,
    async execute() {
      executions += 1;
      throw failure;
    },
  });
  await assert.rejects(
    guarded({ amount: 1 }, {}),
    (error: unknown) => error === failure,
  );
  assert.equal(executions, 1);
  assert.equal(decisions, 1);
});
