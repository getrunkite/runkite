/**
 * HTTP proxy BaseCheckpointSaver (opaque blobs via control plane §6.2).
 *
 * Used when POSTGRES_DSN is unset but RUNKITE_HTTP_URL is set -- persistence
 * survives runner restarts against any CP backend (SQLite/MySQL/Mongo/Postgres)
 * without giving the runner DB credentials.
 *
 * Blob format (owned by this saver, opaque to the CP):
 *   {
 *     v: 1,
 *     checkpoint: <serde dumpsTyped>,
 *     metadata: <serde dumpsTyped>,
 *     parent_checkpoint_id: <string|null>,
 *     writes: [[taskId, channel, dumpsTyped], ...]
 *   }
 *
 * checkpoint_ns is folded into the CP path key as "{ns}\x1f{checkpoint_id}"
 * (empty ns → bare checkpoint_id) so subgraphs do not collide.
 *
 * HTTP paths use the bare assignment thread_id (not the tenant-prefixed
 * configurable.thread_id used by PostgresSaver). Prefixing is only for
 * direct Postgres tables that lack a tenant_id column; proxy rows already
 * carry tenant_id and run-binding checks against the assignment thread_id.
 *
 * put / putWrites serialize per blob key with an in-process mutex so
 * parallel LangGraph tasks in one runner do not lose writes via
 * GET-modify-PUT. Across processes/replicas, PUTs carry If-Match (ETag)
 * and retry on 412 so concurrent writers merge rather than silently clobber.
 * putWrites without configurable.checkpoint_id throws (no silent no-op that
 * would drop mid-superstep channels).
 *
 * LangGraph's Pregel order calls putWrites for a *new* checkpoint_id
 * *before* put creates that blob — so a 404 here is normal, not an error.
 * Both put and putWrites create with If-None-Match: * on 404; if a peer
 * already won, 412 triggers a GET+merge retry instead of clobbering.
 *
 * getTuple without checkpoint_id uses GET .../latest?ns= (one round-trip)
 * instead of list+N get.
 */
import { BaseCheckpointSaver, type ChannelVersions, type Checkpoint, type CheckpointListOptions, type CheckpointMetadata, type CheckpointTuple, type PendingWrite } from "@langchain/langgraph-checkpoint";
import type { RunnableConfig } from "@langchain/core/runnables";
export type ProxyCheckpointSaverOptions = {
    httpBaseUrl: string;
    runnerToken?: string;
};
export declare class ProxyCheckpointSaver extends BaseCheckpointSaver {
    private readonly base;
    private readonly staticHeaders;
    private readonly dispatcher;
    private readonly locks;
    constructor(opts: ProxyCheckpointSaverOptions);
    /** No persistent client to tear down (global fetch); kept for CheckpointerManager.closeFn parity. */
    close(): Promise<void>;
    private clientHeaders;
    private url;
    private latestUrl;
    private encode;
    private decode;
    private putCas;
    private tupleFromBlob;
    getTuple(config: RunnableConfig): Promise<CheckpointTuple | undefined>;
    private getLatestViaList;
    list(config: RunnableConfig, options?: CheckpointListOptions): AsyncGenerator<CheckpointTuple>;
    put(config: RunnableConfig, checkpoint: Checkpoint, metadata: CheckpointMetadata, newVersions: ChannelVersions): Promise<RunnableConfig>;
    putWrites(config: RunnableConfig, writes: PendingWrite[], taskId: string): Promise<void>;
    deleteThread(threadId: string): Promise<void>;
}
