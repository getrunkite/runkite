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
export declare class LangGraphAdapter {
    private configPath;
    private graphs;
    private configDir;
    constructor(configPath: string);
    load(): Promise<void>;
    getGraph(graphId: string): RunnableGraph;
    attachCheckpointer(manager: CheckpointerManager): void;
    attachStore(store: RunkiteStore): void;
}
/** Splits "./graph.ts:graph" into ["./graph.ts", "graph"] -- same convention as the Python config's "path:symbol" format. */
export declare function splitGraphPath(graphPath: string): [string, string];
export declare function loadCustomAppConfig(configPath: string): {
    module: string;
    host?: string;
    port?: number;
} | null;
