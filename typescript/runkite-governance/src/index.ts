/**
 * Calls POST /governance/decide, then polls GET /governance/actions/{id}
 * when the effect is pending. The app executes the tool after allow.
 * Runkite does not execute the tool and does not consume the approval
 * on poll. This is not the deferred client SDK in docs/client-sdk.md.
 */

export type GovernanceEffect = "allow" | "deny" | "pending";

export type GovernanceCall = {
  agentId: string;
  connector: string;
  tool: string;
  args?: unknown;
  runId?: string;
  tenantId?: string;
};

export type GovernanceDecision = {
  effect: GovernanceEffect;
  reason?: string;
  reasonCode?: string;
  actionId?: string;
};

export type GovernanceStatus = "pending" | "approved" | "rejected" | "consumed";

export type GovernanceAction = {
  actionId: string;
  status: GovernanceStatus;
  reason?: string;
  reasonCode?: string;
};

/** Terminal answer for the app. approved becomes allow. rejected and consumed become deny. */
export type GovernanceOutcome = {
  effect: "allow" | "deny";
  reason?: string;
  reasonCode?: string;
  actionId?: string;
  status?: GovernanceStatus;
};

export class GovernanceError extends Error {
  status: number;

  constructor(message: string, status: number) {
    super(message);
    this.name = "GovernanceError";
    this.status = status;
  }
}

export type GovernanceClientOptions = {
  baseUrl: string;
  /** Sent as Authorization: Bearer. Omit for a local plane with auth off. */
  apiKey?: string;
  fetch?: typeof fetch;
  /** Default 500. */
  intervalMs?: number;
  /**
   * Total HTTP/poll deadline for resolve(), decide() and action(). Default 60s.
   * The plane rejects an embedded row after 24h; raise this if the operator is slower than a minute.
   */
  timeoutMs?: number;
};

export type GovernanceWait = {
  intervalMs?: number;
  timeoutMs?: number;
  /** Stops the poll, including an in-flight request. A closed chat tab passes its AbortSignal here. */
  signal?: AbortSignal;
};

export type GovernanceClient = {
  decide(
    call: GovernanceCall,
    signal?: AbortSignal,
  ): Promise<GovernanceDecision>;
  action(actionId: string, signal?: AbortSignal): Promise<GovernanceAction>;
  resolve(
    call: GovernanceCall,
    wait?: GovernanceWait,
  ): Promise<GovernanceOutcome>;
};

/** Identity for one application-owned tool operation, not a managed Runkite run. */
export type GovernanceExecutionIdentity = {
  agentId: string;
  connector: string;
  tool: string;
  /** A stable application operation ID, reused for retries of the same operation. */
  runId: string;
  tenantId?: string;
};

export type GovernanceExecutionOptions = { abortSignal?: AbortSignal };

/** The guard executes a frozen JSON copy. Plain object copies have no inherited properties. */
export type DeepReadonly<T> = T extends readonly (infer Item)[]
  ? readonly DeepReadonly<Item>[]
  : T extends object
    ? { readonly [Key in keyof T]: DeepReadonly<T[Key]> }
    : T;

export type GovernanceDeniedDecision = Readonly<
  GovernanceDecision & { effect: "deny" }
>;
export type GovernancePendingDecision = Readonly<
  GovernanceDecision & { effect: "pending"; actionId: string }
>;

export type GovernedExecuteConfig<
  Input,
  Output,
  Options extends GovernanceExecutionOptions = GovernanceExecutionOptions,
> = {
  client: Pick<GovernanceClient, "decide">;
  /** Runs synchronously on validated, frozen JSON before the first HTTP await. */
  identify: (
    input: DeepReadonly<Input>,
    options: Options,
  ) => GovernanceExecutionIdentity;
  /** Single-result tools only. AsyncIterable/streaming results are unsupported. */
  execute: (
    input: DeepReadonly<Input>,
    options: Options,
  ) => Output | PromiseLike<Output>;
  onDenied?: (
    input: DeepReadonly<Input>,
    options: Options,
    decision: GovernanceDeniedDecision,
  ) => Output | PromiseLike<Output>;
  /** Persist the action ID and original input in the application before returning. */
  onPending?: (
    input: DeepReadonly<Input>,
    options: Options,
    decision: GovernancePendingDecision,
  ) => Output | PromiseLike<Output>;
};

/** A blocked operation. It contains decision metadata, never the submitted arguments. */
export class GovernanceBlockedError extends Error {
  readonly decision: GovernanceDeniedDecision | GovernancePendingDecision;

  constructor(decision: GovernanceDeniedDecision | GovernancePendingDecision) {
    super(
      decision.effect === "pending"
        ? "Tool execution is pending governance approval"
        : "Tool execution was denied by governance",
    );
    this.name = "GovernanceBlockedError";
    const checked = executionDecision(decision);
    if (checked.effect === "allow")
      throw new GovernanceError("blocked decision cannot allow execution", 502);
    this.decision = checked;
  }
}

/**
 * Guard a single-result tool executor without taking ownership of the model loop.
 * It decides once, never polls or retries, and executes only after allow.
 * Input must be a plain JSON object; nested JSON values and arrays are supported.
 * Options stay outside policy arguments; put every authorization-relevant value in input.
 * The application owns persistence, idempotency and later approval continuation.
 */
export function createGovernedExecute<
  Input,
  Output,
  Options extends GovernanceExecutionOptions = GovernanceExecutionOptions,
>(
  config: GovernedExecuteConfig<Input, Output, Options>,
): (input: Input, options: Options) => Promise<Output> {
  // Capture the operation and callbacks once. Mutating config during a decision
  // must not replace the executor that will run after authorization.
  const decide = config.client.decide.bind(config.client);
  const { identify, execute, onDenied, onPending } = config;

  return async (input, options) => {
    const signal = options.abortSignal;
    signal?.throwIfAborted();
    const snapshot = snapshotExecutionInput(input);
    const identity = executionIdentity(identify(snapshot, options));
    const call = Object.freeze({ ...identity, args: snapshot });
    signal?.throwIfAborted();
    const rawDecision = await decide(call, signal);
    signal?.throwIfAborted();
    const decision = executionDecision(rawDecision);
    // A custom client's response validation must not introduce an abort window.
    signal?.throwIfAborted();
    let result: Output;
    if (decision.effect === "allow") {
      result = await execute(snapshot, options);
    } else if (decision.effect === "deny") {
      if (!onDenied) throw new GovernanceBlockedError(decision);
      result = await onDenied(snapshot, options, decision);
    } else {
      if (!onPending) throw new GovernanceBlockedError(decision);
      result = await onPending(snapshot, options, decision);
    }
    if (
      result != null &&
      (typeof result === "object" || typeof result === "function") &&
      Symbol.asyncIterator in result
    ) {
      throw new GovernanceError(
        "streaming tool results are not supported by createGovernedExecute",
        400,
      );
    }
    return result;
  };
}

function executionIdentity(
  raw: GovernanceExecutionIdentity,
): GovernanceExecutionIdentity {
  if (!raw || typeof raw !== "object")
    throw new GovernanceError(
      "governance execution identity must be an object",
      400,
    );
  const read = (key: keyof GovernanceExecutionIdentity, required: boolean) => {
    const descriptor = Object.getOwnPropertyDescriptor(raw, key);
    if (!descriptor && !required) return undefined;
    if (
      !descriptor ||
      !("value" in descriptor) ||
      !validExecutionID(descriptor.value)
    ) {
      throw new GovernanceError(
        `governance execution ${key} must be a nonempty canonical string of at most 256 UTF-8 bytes`,
        400,
      );
    }
    return descriptor.value as string;
  };
  const agentId = read("agentId", true)!;
  const connector = read("connector", true)!;
  const tool = read("tool", true)!;
  const runId = read("runId", true)!;
  const tenantId = read("tenantId", false);
  return tenantId === undefined
    ? { agentId, connector, tool, runId }
    : { agentId, connector, tool, runId, tenantId };
}

type ExecutionDecision =
  | Readonly<GovernanceDecision & { effect: "allow" }>
  | GovernanceDeniedDecision
  | GovernancePendingDecision;

function executionDecision(raw: unknown): ExecutionDecision {
  if (!raw || typeof raw !== "object" || Array.isArray(raw))
    throw new GovernanceError(
      "governance decide response must be an object",
      502,
    );
  const read = (key: string): unknown => {
    const descriptor = Object.getOwnPropertyDescriptor(raw, key);
    if (!descriptor) return undefined;
    if (!("value" in descriptor))
      throw new GovernanceError(
        "governance decide response contains an accessor",
        502,
      );
    return descriptor.value;
  };
  const effect = read("effect");
  if (effect !== "allow" && effect !== "deny" && effect !== "pending")
    throw new GovernanceError("governance decide response missing effect", 502);
  const metadata: { reason?: string; reasonCode?: string; actionId?: string } =
    {};
  for (const key of ["reason", "reasonCode", "actionId"] as const) {
    const value = read(key);
    if (value !== undefined) {
      if (typeof value !== "string")
        throw new GovernanceError(
          "governance decide response metadata must be strings",
          502,
        );
      metadata[key] = value;
    }
  }
  if (effect === "pending") {
    if (!validExecutionID(metadata.actionId))
      throw new GovernanceError("pending decision missing action_id", 502);
    return Object.freeze({ ...metadata, effect, actionId: metadata.actionId });
  }
  return Object.freeze({ ...metadata, effect });
}

function snapshotExecutionInput<Input>(input: Input): DeepReadonly<Input> {
  const ancestors = new Set<object>();
  const invalid = (): never => {
    throw new GovernanceError(
      "tool input must be lossless JSON data without accessors or cycles",
      400,
    );
  };
  if (input === null || typeof input !== "object" || Array.isArray(input))
    return invalid();
  const copy = (value: unknown): unknown => {
    if (value === null || typeof value === "boolean") return value;
    if (typeof value === "string")
      return wellFormedExecutionString(value) ? value : invalid();
    if (typeof value === "number") {
      if (!Number.isFinite(value) || Object.is(value, -0)) return invalid();
      return value;
    }
    if (typeof value !== "object") return invalid();
    if (ancestors.has(value)) return invalid();
    const array = Array.isArray(value);
    const prototype = Object.getPrototypeOf(value);
    if (
      array
        ? prototype !== Array.prototype
        : prototype !== Object.prototype && prototype !== null
    )
      return invalid();
    ancestors.add(value);
    try {
      const keys = Reflect.ownKeys(value);
      if (keys.some((key) => typeof key !== "string")) return invalid();
      const result = array ? [] : Object.create(null);
      const length = array
        ? Object.getOwnPropertyDescriptor(value, "length")?.value
        : undefined;
      if (
        array &&
        (!Number.isSafeInteger(length) ||
          length < 0 ||
          keys.length !== length + 1)
      )
        return invalid();
      for (const key of keys) {
        if (typeof key !== "string" || !wellFormedExecutionString(key))
          return invalid();
        const descriptor = Object.getOwnPropertyDescriptor(value, key);
        if (!descriptor || !("value" in descriptor)) return invalid();
        if (array && key === "length") continue;
        if (!descriptor.enumerable) return invalid();
        if (array && (!/^(0|[1-9][0-9]*)$/.test(key) || Number(key) >= length))
          return invalid();
        Object.defineProperty(result, key, {
          value: copy(descriptor.value),
          enumerable: true,
          writable: true,
          configurable: true,
        });
      }
      return Object.freeze(result);
    } finally {
      ancestors.delete(value);
    }
  };
  // The validator preserves every JSON property and value before this type assertion.
  return copy(input) as DeepReadonly<Input>;
}

function wellFormedExecutionString(value: string): boolean {
  for (let index = 0; index < value.length; index++) {
    const code = value.charCodeAt(index);
    if (code >= 0xd800 && code <= 0xdbff) {
      const next = value.charCodeAt(++index);
      if (!(next >= 0xdc00 && next <= 0xdfff)) return false;
    } else if (code >= 0xdc00 && code <= 0xdfff) return false;
  }
  return true;
}

function validExecutionID(value: unknown): value is string {
  return (
    typeof value === "string" &&
    value.length > 0 &&
    !/^[\p{White_Space}\uFEFF]|[\p{White_Space}\uFEFF]$/u.test(value) &&
    wellFormedExecutionString(value) &&
    new TextEncoder().encode(value).length <= 256
  );
}

const DEFAULT_INTERVAL_MS = 500;
const DEFAULT_TIMEOUT_MS = 60_000;

export function createGovernanceClient(
  options: GovernanceClientOptions,
): GovernanceClient {
  const base = options.baseUrl.replace(/\/$/, "");
  const fetchImpl = options.fetch ?? fetch;
  const intervalMs = options.intervalMs ?? DEFAULT_INTERVAL_MS;
  const timeoutMs = options.timeoutMs ?? DEFAULT_TIMEOUT_MS;

  function deadlineSignal(signal?: AbortSignal, limit = timeoutMs) {
    if (!Number.isFinite(limit) || limit <= 0)
      throw new GovernanceError("governance poll timed out", 408);
    const controller = new AbortController();
    const abort = () =>
      controller.abort(
        signal?.reason ??
          new DOMException("The operation was aborted.", "AbortError"),
      );
    const timer = setTimeout(
      () =>
        controller.abort(new GovernanceError("governance poll timed out", 408)),
      limit,
    );
    signal?.addEventListener("abort", abort, { once: true });
    if (signal?.aborted) abort();
    return {
      signal: controller.signal,
      close: () => {
        clearTimeout(timer);
        signal?.removeEventListener("abort", abort);
      },
    };
  }

  async function request(
    method: string,
    path: string,
    body: unknown,
    signal?: AbortSignal,
  ): Promise<unknown> {
    const deadline = deadlineSignal(signal);
    try {
      deadline.signal.throwIfAborted();
      const headers: Record<string, string> = { Accept: "application/json" };
      if (body !== undefined) headers["Content-Type"] = "application/json";
      if (options.apiKey) headers.Authorization = `Bearer ${options.apiKey}`;
      const res = await fetchImpl(`${base}${path}`, {
        method,
        redirect: "error",
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: deadline.signal,
      });
      const text = await res.text();
      deadline.signal.throwIfAborted();
      let parsed: unknown = undefined;
      if (text) {
        try {
          parsed = JSON.parse(text);
        } catch {
          parsed = undefined;
        }
      }
      if (!res.ok) {
        const message =
          parsed &&
          typeof parsed === "object" &&
          "message" in parsed &&
          typeof parsed.message === "string"
            ? parsed.message
            : text || res.statusText;
        throw new GovernanceError(message, res.status);
      }
      deadline.signal.throwIfAborted();
      return parsed;
    } finally {
      deadline.close();
    }
  }

  return {
    async decide(call, signal) {
      const raw = await request(
        "POST",
        "/governance/decide",
        {
          agent_id: call.agentId,
          connector: call.connector,
          tool: call.tool,
          args: call.args ?? {},
          run_id: call.runId ?? "",
          ...(call.tenantId ? { tenant_id: call.tenantId } : {}),
        },
        signal,
      );
      return asDecision(raw);
    },

    async action(actionId, signal) {
      const raw = await request(
        "GET",
        `/governance/actions/${encodeURIComponent(actionId)}`,
        undefined,
        signal,
      );
      return asAction(raw);
    },

    async resolve(call, wait) {
      const deadline = deadlineSignal(
        wait?.signal,
        wait?.timeoutMs ?? timeoutMs,
      );
      const signal = deadline.signal;
      try {
        const decision = await this.decide(call, signal);
        signal.throwIfAborted();
        if (decision.effect !== "pending") {
          return compact({
            effect: decision.effect,
            reason: decision.reason,
            reasonCode: decision.reasonCode,
          });
        }
        if (!decision.actionId)
          throw new GovernanceError("pending decision missing action_id", 502);
        const every = wait?.intervalMs ?? intervalMs;
        if (!Number.isFinite(every) || every < 0)
          throw new GovernanceError("intervalMs must be non-negative", 400);
        for (;;) {
          const action = await this.action(decision.actionId, signal);
          signal.throwIfAborted();
          if (action.status !== "pending") return outcomeFromAction(action);
          await sleep(every, signal);
        }
      } finally {
        deadline.close();
      }
    },
  };
}

function asDecision(raw: unknown): GovernanceDecision {
  const row = object(raw);
  const effect = row.effect;
  if (effect !== "allow" && effect !== "deny" && effect !== "pending") {
    throw new GovernanceError("governance decide response missing effect", 502);
  }
  return compact({
    effect,
    reason: optionalString(row.reason),
    reasonCode: optionalString(row.reason_code),
    actionId: optionalString(row.action_id),
  });
}

function asAction(raw: unknown): GovernanceAction {
  const row = object(raw);
  const status = row.status;
  if (
    status !== "pending" &&
    status !== "approved" &&
    status !== "rejected" &&
    status !== "consumed"
  ) {
    throw new GovernanceError("governance action response missing status", 502);
  }
  const actionId = optionalString(row.action_id);
  if (!actionId)
    throw new GovernanceError(
      "governance action response missing action_id",
      502,
    );
  return compact({
    actionId,
    status,
    reason: optionalString(row.reason),
    reasonCode: optionalString(row.reason_code),
  });
}

function outcomeFromAction(action: GovernanceAction): GovernanceOutcome {
  return {
    effect: action.status === "approved" ? "allow" : "deny",
    reason: action.reason,
    reasonCode: action.reasonCode,
    actionId: action.actionId,
    status: action.status,
  };
}

function object(raw: unknown): Record<string, unknown> {
  if (!raw || typeof raw !== "object")
    throw new GovernanceError("governance response was not JSON", 502);
  return raw as Record<string, unknown>;
}

function optionalString(value: unknown): string | undefined {
  return typeof value === "string" && value !== "" ? value : undefined;
}

function compact<T extends Record<string, unknown>>(row: T): T {
  return Object.fromEntries(
    Object.entries(row).filter(([, value]) => value !== undefined),
  ) as T;
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  signal?.throwIfAborted();
  return new Promise((resolve, reject) => {
    let settled = false;
    const finish = (abort: boolean) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      signal?.removeEventListener("abort", onAbort);
      if (abort)
        reject(
          signal?.reason ??
            new DOMException("The operation was aborted.", "AbortError"),
        );
      else resolve();
    };
    const onAbort = () => finish(true);
    const timer = setTimeout(() => finish(false), ms);
    signal?.addEventListener("abort", onAbort, { once: true });
    if (signal?.aborted) onAbort();
  });
}
