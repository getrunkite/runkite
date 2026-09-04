/**
 * Executes a single run and emits RunEvents via a callback. TypeScript
 * mirror of the Python runner's execute_run() in worker.py -- same event
 * shapes, same lifecycle/interrupt/cancel/error handling, so the control
 * plane's normalizer sees an identical event stream regardless of which
 * runner produced it (this equivalence is the whole point of proving the
 * Runner Protocol is language-agnostic).
 */
import type { LangGraphAdapter, RunnableGraph } from "./adapter.js";
import { RunnerUser } from "./runnerUser.js";
import type { FactoryGraphBuild, RunFactoryContext } from "./factoryGraph.js";
import { logger } from "./logger.js";
import { checkpointThreadId } from "./tenantCtx.js";
import { makeRunCallbacks } from "./tracing.js";
import { accumulateUsage, usagePayload, type UsageTotals } from "./usage.js";

export interface RunEvent {
  event_id: string;
  seq: number;
  method: string;
  namespace: string[];
  data: unknown;
  ts: number;
}

export interface RunAssignment {
  run_id: string;
  thread_id: string;
  graph_id: string;
  input?: unknown;
  config?: Record<string, unknown>;
  stream_modes?: string[];
  checkpoint_ref?: string | null;
  resume_command?: { response?: unknown } | null;
  // The identity that authenticated this run's originating HTTP
  // request, if any (internal/transport.UserContext's flat wire shape:
  // identity/is_authenticated/display_name?/permissions?, plus any
  // provider-specific Extra fields at the top level) -- undefined when
  // no auth provider is configured, or the caller has no identity
  // attached. See runnerUser.ts.
  user?: Record<string, unknown>;
  // Fencing token -- see heartbeat.ts's doc comment.
  // Absent (undefined) from a control plane that predates this field,
  // treated the same as 0 (unfenced) by worker.ts's own `?? 0`.
  generation?: number;
  // Tenant that authenticated the originating request -- scopes
  // direct-mode store SQL, proxy X-Runkite-Tenant-Id, and (for
  // non-default tenants) the LangGraph checkpointer thread_id key.
  // Absent on older control planes → runner falls back to "default".
  tenant_id?: string;
  // Optional in-graph tool allowlist. Absent/undefined = no runner-side
  // filter. Present (including []) = refuse tool names not listed before
  // ToolNode side effects. Distinct from connector policy grants.
  allowed_tools?: string[] | null;
}

/**
 * Builds the RunnableConfig passed to graph.stream(), including the keys
 * LangGraph's own OSS code reads to populate Runtime.server_info for
 * node code -- distinct from a Factory Graph's ServerRuntime.user (see
 * factoryGraph.ts), which only the graph *factory* sees at build time.
 * LangGraph documents this as "the server puts assistant_id/graph_id in
 * config.configurable and the authenticated user dict in
 * configurable.langgraph_auth_user" -- any hosting server (LangGraph
 * Platform, this runner, or any other LangGraph SDK-compatible server)
 * is expected to set these keys for node-level code to work at all.
 * Direct TypeScript mirror of the Python runner's build_run_config() in
 * worker.py -- a pure function (no I/O) so it's unit-testable without a
 * live control plane or graph.
 */
export function buildRunConfig(assignment: RunAssignment): Record<string, any> {
  const config: Record<string, any> = { ...(assignment.config ?? {}) };
  config.configurable = { ...(config.configurable ?? {}) };
  // Checkpointer key: bare thread_id for default/absent tenant; prefixed
  // otherwise so tenants cannot collide on a client-chosen thread id.
  // Node code that reads configurable.thread_id sees this same value.
  config.configurable.thread_id = checkpointThreadId(assignment.tenant_id, assignment.thread_id);
  config.configurable.run_id = assignment.run_id;
  // Fencing generation for run-bound /internal/* calls (connectors,
  // store, vectors). Same value Heartbeat/ReportStatus echo; helpers
  // like getConnectorSession read it from configurable.
  config.configurable.generation = assignment.generation ?? 0;
  config.configurable.assistant_id = assignment.graph_id;
  config.configurable.graph_id = assignment.graph_id;
  // Time-travel: non-empty checkpoint_ref → LangGraph's
  // configurable.checkpoint_id so stream() resumes from that past
  // checkpoint instead of the thread's latest. Absent/null keeps the
  // checkpointer's normal latest-checkpoint lookup.
  const checkpointRef = assignment.checkpoint_ref;
  if (typeof checkpointRef === "string" && checkpointRef.trim()) {
    config.configurable.checkpoint_id = checkpointRef.trim();
  }
  if (assignment.user) {
    const user = new RunnerUser(assignment.user);
    config.configurable.langgraph_auth_user = user;
    // LangGraph Platform hosting convenience shortcuts some agents read
    // instead of (or in addition to) server_info.user.
    config.configurable.user_id = user.identity;
    config.configurable.user_display_name = user.displayName;
  }
  // Nest LLM/tool spans under the active runkite.run when OTEL is on.
  // Merge so a graph that already set LangSmith/custom callbacks keeps them.
  const otelCbs = makeRunCallbacks(assignment.run_id);
  if (otelCbs.length > 0) {
    const existing = config.callbacks;
    if (existing == null) {
      config.callbacks = otelCbs;
    } else if (Array.isArray(existing)) {
      config.callbacks = [...existing, ...otelCbs];
    } else {
      config.callbacks = [existing, ...otelCbs];
    }
  }
  return config;
}

export type RunStatus = "success" | "error" | "interrupted";

/** Recursively scans a LangGraph.js stream chunk for AIMessage.tool_calls
 * the control plane's on_tool_call hook watches for -- TypeScript mirror
 * of the Python runner's find_new_tool_calls() in worker.py. Neither
 * runner emits that method natively today, so this scan is what surfaces
 * tool calls instead. `seenIds` is mutated in place (dedup state); pass a
 * fresh Set per run.
 *
 * Checked in every stream mode, same reasoning as the Python version:
 * "values"/"updates" give complete, already-materialized messages per
 * graph step; "messages" streams per-token deltas, so an early chunk's
 * tool_calls may have incomplete name/args from partially-parsed JSON.
 * An id is only marked seen once its name is non-empty — otherwise a
 * later chunk that fills in a disallowed name would be deduped and never
 * checked by allowed_tools. */
function findNewToolCalls(obj: unknown, seenIds: Set<string>): Array<{ id?: string; name?: string; args?: unknown }> {
  const found: Array<{ id?: string; name?: string; args?: unknown }> = [];
  const toolCalls = obj != null && typeof obj === "object" ? (obj as any).tool_calls : undefined;
  if (Array.isArray(toolCalls) && toolCalls.length > 0) {
    for (const tc of toolCalls) {
      const id = tc?.id;
      if (!id || seenIds.has(id)) continue;
      const name = tc?.name;
      // Incomplete stream delta: wait for a later chunk with the name.
      // Do not mark seen — that would permanently skip allowlist checks.
      if (!name) continue;
      seenIds.add(id);
      found.push({ id: tc.id, name: tc.name, args: tc.args });
    }
  } else if (Array.isArray(obj)) {
    for (const item of obj) found.push(...findNewToolCalls(item, seenIds));
  } else if (obj != null && typeof obj === "object") {
    for (const v of Object.values(obj)) found.push(...findNewToolCalls(v, seenIds));
  }
  return found;
}

/** Converts LangChain message objects (BaseMessage etc.) and arbitrary
 * class instances into plain JSON-serializable values, the same job
 * Python's _serialize() helper does for LangChain's model_dump()/dict(). */
function serialize(obj: unknown): unknown {
  if (obj === null || obj === undefined) return obj ?? null;
  if (typeof obj === "string" || typeof obj === "number" || typeof obj === "boolean") return obj;
  if (Array.isArray(obj)) return obj.map(serialize);
  if (obj instanceof Date) return obj.toISOString();
  if (typeof obj === "object") {
    const anyObj = obj as any;
    // LangChain messages implement toDict()/toJSON() via @langchain/core's
    // Serializable base class.
    if (typeof anyObj.toDict === "function") return serialize(anyObj.toDict());
    if (typeof anyObj.toJSON === "function") return serialize(anyObj.toJSON());
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(anyObj)) out[k] = serialize(v);
    return out;
  }
  return String(obj);
}

export async function executeRun(
  adapter: LangGraphAdapter,
  assignment: RunAssignment,
  emit: (event: RunEvent) => Promise<void>,
  isCancelled: () => boolean,
): Promise<RunStatus> {
  const runId = assignment.run_id;
  const graphId = assignment.graph_id;
  let inputData: unknown = assignment.input;
  const config = buildRunConfig(assignment);
  const streamModesReq = assignment.stream_modes ?? ["values"];
  const resumeCommand = assignment.resume_command;

  let seq = 0;
  const makeEvent = (method: string, data: unknown, namespace: string[] = []): RunEvent => {
    seq += 1;
    return {
      event_id: `${runId}_evt_${seq}`,
      seq,
      method,
      namespace,
      data: serialize(data),
      ts: Date.now(),
    };
  };

  // Declared outside try/catch (not just outside the stream loop) so the
  // outer catch's own "interrupted" path -- a cancel racing an exception --
  // can still report whatever usage was captured before the exception,
  // the same as every in-loop "interrupted" exit.
  let lastValues: unknown = null;
  const usageTotals: UsageTotals = {};
  // Declared here, not just at its assignment site below, so emitInterrupted
  // (which can fire before the getState snapshot runs, e.g. a cancel
  // during graph construction) always has a defined set to pass --- an
  // empty set is correct in that case (nothing to exclude yet), not a bug.
  let skipIds = new Set<string>();
  let skipPrefix = 0;

  // A HITL pause or a cancel mid-run still burns real, already-spent LLM
  // tokens on every turn that ran before the interrupt/cancel -- those
  // turns were already billed by the provider. Every "interrupted" exit
  // path shares this so a paused run's tokens don't just vanish from
  // FinOps until (if ever) it resumes to a terminal status. Mirrors the
  // Python runner's _emit_interrupted helper.
  const emitInterrupted = async (): Promise<"interrupted"> => {
    // Same once-at-end discipline as the success path: prefer the last
    // cumulative values snapshot; fall back to incremental totals when
    // stream_modes never included "values".
    let totals: UsageTotals = {};
    if (lastValues != null) {
      accumulateUsage(totals, lastValues, skipIds, skipPrefix);
    } else {
      totals = { ...usageTotals };
    }
    const usage = usagePayload(totals);
    if (usage) {
      const base = lastValues != null && typeof lastValues === "object" ? { ...(lastValues as object) } : {};
      await emit(makeEvent("values", { ...base, usage }));
    }
    await emit(makeEvent("end", { status: "interrupted" }));
    return "interrupted";
  };

  try {
    // Emitted BEFORE building the graph, not after: for a factory graph
    // (see factoryGraph.ts) this construction runs fresh, on the
    // request path, every single run -- it can genuinely take seconds
    // (LLM client setup, tool binding, MCP session negotiation), and a
    // static graph has zero equivalent cost since it's built once at
    // startup. A client waiting for ANY signal that its run was even
    // accepted, before the first real token, needs this ordering.
    await emit(makeEvent("lifecycle", { event: "running" }));

    if (resumeCommand != null) {
      const { Command } = await import("@langchain/langgraph");
      inputData = new Command({ resume: resumeCommand.response });
    }

    const streamMode = streamModesReq.filter((m) => m === "values" || m === "updates" || m === "messages");
    const lgStreamMode = streamMode.length > 0 ? streamMode : ["values"];

    let hasInterrupt = false;
    const seenToolCallIds = new Set<string>();

    // Factory graph (for LangGraph SDK/ServerRuntime compatibility) --
    // built fresh for this run alone, with
    // checkpointer/store attached to THIS instance, not the shared one
    // static graphs use. See factoryGraph.ts for the full rationale.
    // factoryBuild.close() MUST wrap open() as well as the stream loop:
    // open() can set a dispose()-able resource and then throw (e.g.
    // compile/attach failure), and closing only after a successful open
    // would leak that resource. Same "teardown regardless of exit
    // reason" job as Python's AsyncExitStack around `async with`.
    let graph: RunnableGraph;
    let factoryBuild: FactoryGraphBuild | null = null;
    try {
      if (adapter.isFactory(graphId)) {
        const runContext: RunFactoryContext = { runId, threadId: assignment.thread_id, user: assignment.user };
        factoryBuild = adapter.buildFactoryGraph(graphId, config, runContext);
        graph = await factoryBuild.open();
      } else {
        graph = adapter.getGraph(graphId);
      }

      // Snapshot which message ids already existed before this run -- a
      // stateful graph's checkpoint carries every prior turn's AIMessage
      // forward into every future "values" snapshot, and without this,
      // every subsequent turn on the same thread (and every HITL resume)
      // would re-sum tokens FinOps already recorded for those older
      // messages on top of its own new ones. See accumulateUsage's
      // docstring for the full failure mode. Best-effort: a fresh thread
      // has no checkpoint yet, which correctly yields an empty skip set.
      try {
        const priorState = await graph.getState?.(config);
        const priorValues = priorState?.values;
        if (priorValues && typeof priorValues === "object") {
          const msgs = (priorValues as Record<string, unknown>).messages;
          if (Array.isArray(msgs)) {
            skipPrefix = msgs.length;
            for (const msg of msgs) {
              const mid = msg && typeof msg === "object" ? (msg as Record<string, unknown>).id : undefined;
              if (mid) skipIds.add(String(mid));
            }
          }
        }
      } catch (err) {
        logger.debug("could not snapshot prior state for usage dedup", err);
      }

      const stream = await graph.stream(inputData, { ...config, streamMode: lgStreamMode });

      for await (const chunk of stream) {
        let mode: string;
        let data: any;
        if (Array.isArray(chunk) && chunk.length === 2 && typeof chunk[0] === "string") {
          [mode, data] = chunk;
        } else {
          mode = lgStreamMode.length === 1 ? lgStreamMode[0] : "values";
          data = chunk;
        }

        // allowed_tools: undefined = no filter; present (incl. empty) =
        // refuse names not listed before ToolNode side effects.
        const allowedTools = assignment.allowed_tools != null ? new Set(assignment.allowed_tools) : null;

        for (const tc of findNewToolCalls(data, seenToolCallIds)) {
          await emit(makeEvent("tool_call", { name: tc.name, args: tc.args, id: tc.id }));
          if (allowedTools != null) {
            const name = tc.name;
            // findNewToolCalls only yields non-empty names; keep the guard so
            // a future change cannot false-deny "".
            if (!name) {
              continue;
            }
            if (!allowedTools.has(name)) {
              await emit(
                makeEvent("tool_auth", {
                  stage: "tool.call",
                  effect: "deny",
                  tool: name,
                  reason_code: "tool_not_allowed",
                  reason: `tool ${JSON.stringify(name)} is not in allowed_tools`,
                }),
              );
              await emit(
                makeEvent("error", {
                  message: `tool ${JSON.stringify(name)} is not in allowed_tools`,
                  type: "ToolNotAllowed",
                  reason_code: "tool_not_allowed",
                }),
              );
              await emit(makeEvent("end", { status: "error" }));
              return "error";
            }
          }
        }

        if (data && typeof data === "object" && "__interrupt__" in data) {
          hasInterrupt = true;
          const interrupts = Array.isArray(data.__interrupt__) ? data.__interrupt__ : [data.__interrupt__];
          await emit(makeEvent("lifecycle", { event: "interrupted" }));
          for (const it of interrupts) {
            // LangGraph.js's actual interrupt shape carries "id" directly
            // (confirmed against a real interrupt()/Command(resume) round
            // trip, v1.4.8: {id, value} -- no "ns" field at all despite
            // some published docs/examples showing an {ns, resumable, when}
            // shape instead). Falls back to "ns" for forward/backward
            // compatibility with whichever shape a given version actually
            // produces, then to a synthesized id as a last resort.
            const interruptId =
              typeof it?.id === "string" && it.id
                ? it.id
                : Array.isArray(it?.ns) && it.ns.length > 0
                  ? it.ns.join(":")
                  : `interrupt-${seq}`;
            await emit(makeEvent("input.requested", { interrupt_id: interruptId, value: it?.value ?? it }));
          }
          const cleanData = Object.fromEntries(Object.entries(data).filter(([k]) => k !== "__interrupt__"));
          if (Object.keys(cleanData).length > 0) await emit(makeEvent(mode, cleanData));
          continue;
        }

        if (isCancelled()) {
          return await emitInterrupted();
        }

        // Mirror Python: keep last values snapshot; accumulate non-values
        // incrementally so stream_mode without "values" still meters.
        if (mode === "values" && data && typeof data === "object") {
          lastValues = data;
        } else if (mode !== "values") {
          accumulateUsage(usageTotals, data, skipIds);
        }
        await emit(makeEvent(mode, data));
      }

      if (hasInterrupt) {
        return await emitInterrupted();
      }
      // Enrich final values with top-level usage so FinOps can meter Output
      // (same once-at-end discipline as the Python LangGraph runner).
      let totals: UsageTotals = { ...usageTotals };
      if (lastValues != null) {
        // Prefer once-at-end from the last cumulative values snapshot.
        totals = {};
        accumulateUsage(totals, lastValues, skipIds, skipPrefix);
      }
      const usage = usagePayload(totals);
      if (usage && lastValues != null) {
        await emit(makeEvent("values", { ...(lastValues as object), usage }));
      } else if (usage) {
        // No values mode — still emit a values event carrying usage so the
        // control plane can meter Output the same way as Python.
        await emit(makeEvent("values", { usage }));
      }
      await emit(makeEvent("end", { status: "success" }));
      return "success";
    } finally {
      if (factoryBuild) await factoryBuild.close();
    }
  } catch (err: any) {
    if (isCancelled()) {
      return await emitInterrupted();
    }
    logger.error(`Run ${runId} failed:`, err);
    await emit(
      makeEvent("error", {
        message: err?.message ?? String(err),
        type: err?.constructor?.name ?? "Error",
      }),
    );
    return "error";
  }
}
