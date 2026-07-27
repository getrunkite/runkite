/**
 * Executes a single run and emits RunEvents via a callback. TypeScript
 * mirror of the Python runner's execute_run() in worker.py -- same event
 * shapes, same lifecycle/interrupt/cancel/error handling, so the control
 * plane's normalizer sees an identical event stream regardless of which
 * runner produced it (this equivalence is the whole point of proving the
 * Runner Protocol is language-agnostic).
 */
import type { LangGraphAdapter } from "./adapter.js";

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
}

export type RunStatus = "success" | "error" | "interrupted";

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
  const config: Record<string, any> = assignment.config ?? {};
  const streamModesReq = assignment.stream_modes ?? ["values"];
  const resumeCommand = assignment.resume_command;

  config.configurable ??= {};
  config.configurable.thread_id = assignment.thread_id;
  config.configurable.run_id = runId;

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
    const graph = adapter.getGraph(graphId);
    await emit(makeEvent("lifecycle", { event: "running" }));

    if (resumeCommand != null) {
      const { Command } = await import("@langchain/langgraph");
      inputData = new Command({ resume: resumeCommand.response });
    }

    const streamMode = streamModesReq.filter((m) => m === "values" || m === "updates" || m === "messages");
    const lgStreamMode = streamMode.length > 0 ? streamMode : ["values"];

    let hasInterrupt = false;
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
  } catch (err: any) {
    if (isCancelled()) {
      await emit(makeEvent("end", { status: "interrupted" }));
      return "interrupted";
    }
    console.error(`Run ${runId} failed:`, err);
    await emit(
      makeEvent("error", {
        message: err?.message ?? String(err),
        type: err?.constructor?.name ?? "Error",
      }),
    );
    return "error";
  }
}
