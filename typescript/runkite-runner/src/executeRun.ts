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
  config.configurable.thread_id = assignment.thread_id;
  config.configurable.run_id = assignment.run_id;
  config.configurable.assistant_id = assignment.graph_id;
  config.configurable.graph_id = assignment.graph_id;
  if (assignment.user) {
    const user = new RunnerUser(assignment.user);
    config.configurable.langgraph_auth_user = user;
    // LangGraph Platform hosting convenience shortcuts some agents read
    // instead of (or in addition to) server_info.user.
    config.configurable.user_id = user.identity;
    config.configurable.user_display_name = user.displayName;
  }
  return config;
}

export type RunStatus = "success" | "error" | "interrupted";

/** Recursively scans a LangGraph.js stream chunk for AIMessage.tool_calls
 * the control plane's on_tool_call hook watches for -- TypeScript mirror
 * of the Python runner's find_new_tool_calls() in worker.py (master plan
 * gap: "neither runner emits that method today"). `seenIds` is mutated
 * in place (dedup state); pass a fresh Set per run.
 *
 * Checked in every stream mode, same reasoning as the Python version:
 * "values"/"updates" give complete, already-materialized messages per
 * graph step; "messages" streams per-token deltas, so an early chunk's
 * tool_calls may have incomplete args from partially-parsed JSON --
 * deduping by id means a real but minor trade-off (emitting on an
 * early, args-incomplete chunk) against missing tool calls entirely for
 * messages-only stream requests. */
function findNewToolCalls(obj: unknown, seenIds: Set<string>): Array<{ id?: string; name?: string; args?: unknown }> {
  const found: Array<{ id?: string; name?: string; args?: unknown }> = [];
  const toolCalls = obj != null && typeof obj === "object" ? (obj as any).tool_calls : undefined;
  if (Array.isArray(toolCalls) && toolCalls.length > 0) {
    for (const tc of toolCalls) {
      const id = tc?.id;
      if (!id || seenIds.has(id)) continue;
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

    // Factory graph (master plan: LangGraph SDK/ServerRuntime
    // compatibility) -- built fresh for this run alone, with
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

        for (const tc of findNewToolCalls(data, seenToolCallIds)) {
          await emit(makeEvent("tool_call", { name: tc.name, args: tc.args, id: tc.id }));
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
          await emit(makeEvent("end", { status: "interrupted" }));
          return "interrupted";
        }

        await emit(makeEvent(mode, data));
      }

      if (hasInterrupt) {
        await emit(makeEvent("end", { status: "interrupted" }));
        return "interrupted";
      }
      await emit(makeEvent("end", { status: "success" }));
      return "success";
    } finally {
      if (factoryBuild) await factoryBuild.close();
    }
  } catch (err: any) {
    if (isCancelled()) {
      await emit(makeEvent("end", { status: "interrupted" }));
      return "interrupted";
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
