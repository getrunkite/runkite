import type { BaseCheckpointSaver } from "@langchain/langgraph-checkpoint";
import type { BaseStore } from "@langchain/langgraph-checkpoint";
import type { CheckpointerManager } from "./checkpoint.js";
import type { RunkiteStore } from "./store.js";
import { type FactoryGraphBuild, type RunFactoryContext } from "./factoryGraph.js";
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
export declare class LangGraphAdapter {
    private configPath;
    private graphs;
    private factories;
    private configDir;
    private checkpointerManager;
    private store;
    constructor(configPath: string);
    load(): Promise<void>;
    isFactory(graphId: string): boolean;
    /** Returns the compiled graph for a STATIC graph_id. Throws for a
     * factory graph_id -- those are only resolvable per-run, with a run's
     * own config/runtime context, via buildFactoryGraph. */
    getGraph(graphId: string): RunnableGraph;
    allGraphIds(): string[];
    /** Resolves a factory graph_id for one run. Returns a builder whose
     * `.open()`/`.close()` bracket exactly one run -- callers do
     * `const graph = await adapter.buildFactoryGraph(...).open()` then
     * `await build.close()` in a finally block (see executeRun.ts). */
    buildFactoryGraph(graphId: string, config: Record<string, unknown>, runContext: RunFactoryContext): FactoryGraphBuild;
    /** Overrides every loaded STATIC graph's checkpointer with the
     * runner's shared one, and remembers it so factory graphs get the
     * same treatment per-instance in buildFactoryGraph. Checkpoint mode
     * is a runner concern (see checkpoint.ts), not something agent
     * authors configure in their own graph.ts. */
    attachCheckpointer(manager: CheckpointerManager): void;
    /** Attaches the runner's Store Dual Mode client (see store.ts) to
     * every loaded STATIC graph, the same way attachCheckpointer works,
     * and remembers it for factory graphs. */
    attachStore(store: RunkiteStore): void;
}
/** Splits "./graph.ts:graph" into ["./graph.ts", "graph"] -- same convention as the Python config's "path:symbol" format. */
export declare function splitGraphPath(graphPath: string): [string, string];
export declare function loadCustomAppConfig(configPath: string): {
    module: string;
    host?: string;
    port?: number;
} | null;
