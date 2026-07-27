/**
 * Loads and executes LangGraph.js graphs from a langgraph.json config.
 * TypeScript equivalent of the Python runner's LangGraphAdapter -- same
 * config format, same "zero changes to graph code" principle: checkpoint
 * and store mode are runner concerns, attached after loading, never
 * something an agent author configures in their own graph.ts.
 */
import { readFileSync } from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";
import type { BaseCheckpointSaver } from "@langchain/langgraph-checkpoint";
import type { BaseStore } from "@langchain/langgraph-checkpoint";
import type { CheckpointerManager } from "./checkpoint.js";
import type { RunkiteStore } from "./store.js";

/** Minimal surface of a compiled LangGraph.js graph this runner needs.
 * Matches Pregel's actual instance shape (see node_modules/@langchain/
 * langgraph/dist/pregel/index.d.ts) without importing its full generic
 * type, which is awkward to name for a dynamically-loaded, arbitrary
 * user graph. */
export interface RunnableGraph {
  checkpointer?: BaseCheckpointSaver | boolean;
  store?: BaseStore;
  stream(input: unknown, config?: unknown): Promise<AsyncIterable<unknown>>;
}

interface LangGraphJsonConfig {
  graphs?: Record<string, string>;
  dependencies?: string[];
  custom_app?: { module: string; host?: string; port?: number };
  cron?: unknown;
}

export class LangGraphAdapter {
  private graphs = new Map<string, RunnableGraph>();
  private configDir: string;

  constructor(private configPath: string) {
    this.configDir = path.dirname(path.resolve(configPath));
  }

  async load(): Promise<void> {
    const config: LangGraphJsonConfig = JSON.parse(readFileSync(this.configPath, "utf-8"));

    const graphsConfig = config.graphs ?? {};
    for (const [graphId, graphPath] of Object.entries(graphsConfig)) {
      const [filePath, exportName] = splitGraphPath(graphPath);
      const absPath = path.resolve(this.configDir, filePath);

      // tsx registers a loader that transpiles .ts on the fly, so a
      // dynamic import() of an arbitrary user .ts file works with zero
      // build step on their end -- the same "drop a file in your
      // project" DX as the Python runner's importlib-based loading.
      const mod = await import(pathToFileURL(absPath).href);
      let graph = mod[exportName];
      if (graph === undefined) {
        throw new Error(`Cannot load graph module: ${absPath} has no export "${exportName}"`);
      }
      // If the export is an uncompiled StateGraph builder, compile it --
      // mirrors the Python adapter's isinstance(graph, StateGraph) check.
      if (typeof graph.compile === "function" && typeof graph.stream !== "function") {
        graph = graph.compile();
      }

      this.graphs.set(graphId, graph as RunnableGraph);
      console.log(`Loaded graph: ${graphId} from ${absPath}`);
    }
  }

  getGraph(graphId: string): RunnableGraph {
    const graph = this.graphs.get(graphId);
    if (!graph) {
      throw new Error(`Graph not found: ${graphId}. Available: ${[...this.graphs.keys()].join(", ")}`);
    }
    return graph;
  }

  attachCheckpointer(manager: CheckpointerManager): void {
    for (const [graphId, graph] of this.graphs) {
      manager.attach(graph);
      console.log(`Attached ${manager.mode} checkpointer to graph: ${graphId}`);
    }
  }

  attachStore(store: RunkiteStore): void {
    for (const [graphId, graph] of this.graphs) {
      graph.store = store;
      console.log(`Attached ${store.mode} store to graph: ${graphId}`);
    }
  }
}

/** Splits "./graph.ts:graph" into ["./graph.ts", "graph"] -- same convention as the Python config's "path:symbol" format. */
export function splitGraphPath(graphPath: string): [string, string] {
  const idx = graphPath.indexOf(":");
  if (idx === -1) {
    throw new Error(`Invalid graph path "${graphPath}" -- expected "path:exportName"`);
  }
  return [graphPath.slice(0, idx), graphPath.slice(idx + 1)];
}

export function loadCustomAppConfig(configPath: string): { module: string; host?: string; port?: number } | null {
  const raw: LangGraphJsonConfig = JSON.parse(readFileSync(configPath, "utf-8"));
  return raw.custom_app ?? null;
}
