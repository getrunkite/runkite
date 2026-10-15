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
   * How long resolve() waits on pending. Default 60s.
   * The plane rejects an embedded row after 24h; raise this if the operator is slower than a minute.
   */
  timeoutMs?: number;
};

export type GovernanceClient = {
  decide(call: GovernanceCall): Promise<GovernanceDecision>;
  action(actionId: string): Promise<GovernanceAction>;
  resolve(call: GovernanceCall, wait?: { intervalMs?: number; timeoutMs?: number }): Promise<GovernanceOutcome>;
};

const DEFAULT_INTERVAL_MS = 500;
const DEFAULT_TIMEOUT_MS = 60_000;

export function createGovernanceClient(options: GovernanceClientOptions): GovernanceClient {
  const base = options.baseUrl.replace(/\/$/, "");
  const fetchImpl = options.fetch ?? fetch;
  const intervalMs = options.intervalMs ?? DEFAULT_INTERVAL_MS;
  const timeoutMs = options.timeoutMs ?? DEFAULT_TIMEOUT_MS;

  async function request(method: string, path: string, body?: unknown): Promise<unknown> {
    const headers: Record<string, string> = { Accept: "application/json" };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    if (options.apiKey) headers.Authorization = `Bearer ${options.apiKey}`;
    const res = await fetchImpl(`${base}${path}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const text = await res.text();
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
        parsed && typeof parsed === "object" && "message" in parsed && typeof parsed.message === "string"
          ? parsed.message
          : text || res.statusText;
      throw new GovernanceError(message, res.status);
    }
    return parsed;
  }

  return {
    async decide(call) {
      const raw = await request("POST", "/governance/decide", {
        agent_id: call.agentId,
        connector: call.connector,
        tool: call.tool,
        args: call.args ?? {},
        run_id: call.runId ?? "",
        ...(call.tenantId ? { tenant_id: call.tenantId } : {}),
      });
      return asDecision(raw);
    },

    async action(actionId) {
      const raw = await request("GET", `/governance/actions/${encodeURIComponent(actionId)}`);
      return asAction(raw);
    },

    async resolve(call, wait) {
      const decision = await this.decide(call);
      if (decision.effect !== "pending") {
        return compact({ effect: decision.effect, reason: decision.reason, reasonCode: decision.reasonCode });
      }
      if (!decision.actionId) {
        throw new GovernanceError("pending decision missing action_id", 502);
      }
      const actionId = decision.actionId;
      const every = wait?.intervalMs ?? intervalMs;
      const deadline = Date.now() + (wait?.timeoutMs ?? timeoutMs);
      for (;;) {
        const action = await this.action(actionId);
        if (action.status !== "pending") return outcomeFromAction(action);
        if (Date.now() >= deadline) {
          throw new GovernanceError("governance poll timed out", 408);
        }
        await sleep(every);
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
  if (status !== "pending" && status !== "approved" && status !== "rejected" && status !== "consumed") {
    throw new GovernanceError("governance action response missing status", 502);
  }
  const actionId = optionalString(row.action_id);
  if (!actionId) throw new GovernanceError("governance action response missing action_id", 502);
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
  if (!raw || typeof raw !== "object") throw new GovernanceError("governance response was not JSON", 502);
  return raw as Record<string, unknown>;
}

function optionalString(value: unknown): string | undefined {
  return typeof value === "string" && value !== "" ? value : undefined;
}

function compact<T extends Record<string, unknown>>(row: T): T {
  return Object.fromEntries(Object.entries(row).filter(([, value]) => value !== undefined)) as T;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
