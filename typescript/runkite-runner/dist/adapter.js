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
export class LangGraphAdapter {
    configPath;
    graphs = new Map();
    configDir;
    constructor(configPath) {
        this.configPath = configPath;
        this.configDir = path.dirname(path.resolve(configPath));
    }
    async load() {
        const config = JSON.parse(readFileSync(this.configPath, "utf-8"));
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
            this.graphs.set(graphId, graph);
            console.log(`Loaded graph: ${graphId} from ${absPath}`);
        }
    }
    getGraph(graphId) {
        const graph = this.graphs.get(graphId);
        if (!graph) {
            throw new Error(`Graph not found: ${graphId}. Available: ${[...this.graphs.keys()].join(", ")}`);
        }
        return graph;
    }
    attachCheckpointer(manager) {
        for (const [graphId, graph] of this.graphs) {
            manager.attach(graph);
            console.log(`Attached ${manager.mode} checkpointer to graph: ${graphId}`);
        }
    }
    attachStore(store) {
        for (const [graphId, graph] of this.graphs) {
            graph.store = store;
            console.log(`Attached ${store.mode} store to graph: ${graphId}`);
        }
    }
}
/** Splits "./graph.ts:graph" into ["./graph.ts", "graph"] -- same convention as the Python config's "path:symbol" format. */
export function splitGraphPath(graphPath) {
    const idx = graphPath.indexOf(":");
    if (idx === -1) {
        throw new Error(`Invalid graph path "${graphPath}" -- expected "path:exportName"`);
    }
    return [graphPath.slice(0, idx), graphPath.slice(idx + 1)];
}
export function loadCustomAppConfig(configPath) {
    const raw = JSON.parse(readFileSync(configPath, "utf-8"));
    return raw.custom_app ?? null;
}
