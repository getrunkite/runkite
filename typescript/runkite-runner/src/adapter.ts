/**
 * Loads and executes LangGraph.js graphs from a langgraph.json config.
 * TypeScript equivalent of the Python runner's LangGraphAdapter -- same
 * config format, same "zero changes to graph code" principle: checkpoint
 * and store mode are runner concerns, attached after loading, never
 * something an agent author configures in their own graph.ts.
 *
 * A loaded graph_id is one of two kinds (see factoryGraph.ts for the
 * full rationale -- this implements LangGraph's own documented factory-
 * graph/ServerRuntime convention):
 *
 * - static (the original, only supported form): a compiled graph,
 *   shared across every concurrent run. Stored in `graphs`.
 * - factory (`factories`): a callable built fresh PER RUN instead, for
 *   agents that need request-isolated state -- resolved in
 *   executeRun.ts via buildFactoryGraph, not here.
 */
import { readFileSync } from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";
import type { BaseCheckpointSaver } from "@langchain/langgraph-checkpoint";
import type { BaseStore } from "@langchain/langgraph-checkpoint";
import type { CheckpointerManager } from "./checkpoint.js";
import type { RunkiteStore } from "./store.js";
import { classifyGraphExport, FactoryGraph, type FactoryGraphBuild, type RunFactoryContext } from "./factoryGraph.js";

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
  private factories = new Map<string, FactoryGraph>();
  private configDir: string;
  private checkpointerManager: CheckpointerManager | null = null;
  private store: RunkiteStore | null = null;

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
      const exportValue = mod[exportName];
      if (exportValue === undefined) {
        throw new Error(`Cannot load graph module: ${absPath} has no export "${exportName}"`);
      }

      const classified = await classifyGraphExport(exportValue);
      if (classified.kind === "factory") {
        this.factories.set(graphId, classified.factory);
        console.log(`Loaded factory graph: ${graphId} from ${absPath} (params: ${classified.factory.paramNames.join(", ")})`);
      } else {
        this.graphs.set(graphId, classified.graph);
        console.log(`Loaded graph: ${graphId} from ${absPath}`);
      }
    }
  }

  isFactory(graphId: string): boolean {
    return this.factories.has(graphId);
  }

  /** Returns the compiled graph for a STATIC graph_id. Throws for a
   * factory graph_id -- those are only resolvable per-run, with a run's
   * own config/runtime context, via buildFactoryGraph. */
  getGraph(graphId: string): RunnableGraph {
    const graph = this.graphs.get(graphId);
    if (graph) return graph;
    if (this.factories.has(graphId)) {
      throw new Error(`'${graphId}' is a factory graph -- build it per-run via buildFactoryGraph, not getGraph.`);
    }
    throw new Error(`Graph not found: ${graphId}. Available: ${this.allGraphIds().join(", ")}`);
  }

  allGraphIds(): string[] {
    return [...this.graphs.keys(), ...this.factories.keys()];
  }

  /** Resolves a factory graph_id for one run. Returns a builder whose
   * `.open()`/`.close()` bracket exactly one run -- callers do
   * `const graph = await adapter.buildFactoryGraph(...).open()` then
   * `await build.close()` in a finally block (see executeRun.ts). */
  buildFactoryGraph(graphId: string, config: Record<string, unknown>, runContext: RunFactoryContext): FactoryGraphBuild {
    const factory = this.factories.get(graphId);
    if (!factory) {
      throw new Error(`'${graphId}' is not a factory graph. Available factories: ${[...this.factories.keys()].join(", ")}`);
    }
    return factory.build(config, runContext, { checkpointerManager: this.checkpointerManager, store: this.store });
  }

  /** Overrides every loaded STATIC graph's checkpointer with the
   * runner's shared one, and remembers it so factory graphs get the
   * same treatment per-instance in buildFactoryGraph. Checkpoint mode
   * is a runner concern (see checkpoint.ts), not something agent
   * authors configure in their own graph.ts. */
  attachCheckpointer(manager: CheckpointerManager): void {
    this.checkpointerManager = manager;
    for (const [graphId, graph] of this.graphs) {
      manager.attach(graph);
      console.log(`Attached ${manager.mode} checkpointer to graph: ${graphId}`);
    }
  }

  /** Attaches the runner's Store Dual Mode client (see store.ts) to
   * every loaded STATIC graph, the same way attachCheckpointer works,
   * and remembers it for factory graphs. */
  attachStore(store: RunkiteStore): void {
    this.store = store;
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
