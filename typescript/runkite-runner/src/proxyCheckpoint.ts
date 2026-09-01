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
import {
  BaseCheckpointSaver,
  getCheckpointId,
  type ChannelVersions,
  type Checkpoint,
  type CheckpointListOptions,
  type CheckpointMetadata,
  type CheckpointTuple,
  type PendingWrite,
} from "@langchain/langgraph-checkpoint";
import type { RunnableConfig } from "@langchain/core/runnables";
import { storageThreadId, tenantHeaders } from "./tenantCtx.js";
import { httpDispatcher, type FetchInit } from "./tls.js";

const NS_SEP = "\x1f";
const HEADER_FRAMEWORK = "X-Runkite-Checkpoint-Framework";
const HEADER_CHECKPOINT_ID = "X-Runkite-Checkpoint-Id";
const LIST_FETCH_LIMIT = 1000;
const CAS_MAX_ATTEMPTS = 8;

class CASConflict extends Error {
  constructor() {
    super("checkpoint CAS conflict (412)");
    this.name = "CASConflict";
  }
}

/**
 * Control plane rejects checkpoint I/O once the run is cancelled /
 * reclaimed / terminal. Late Pregel putWrites after cancel must not
 * crash the Node process (unhandled throw kills the whole runner and
 * starves the next matrix scenario in the same cell).
 */
class RunNotInflight extends Error {
  constructor() {
    super("run_not_inflight");
    this.name = "RunNotInflight";
  }
}

function isRunNotInflight(status: number, body: string): boolean {
  return status === 403 && body.includes("run_not_inflight");
}

function blobKey(checkpointNs: string, checkpointId: string): string {
  if (!checkpointNs) return checkpointId;
  return `${checkpointNs}${NS_SEP}${checkpointId}`;
}

function parseBlobKey(key: string): [string, string] {
  const i = key.indexOf(NS_SEP);
  if (i >= 0) return [key.slice(0, i), key.slice(i + 1)];
  return ["", key];
}

function asTyped(value: unknown): [string, Uint8Array] {
  if (Array.isArray(value) && value.length === 2 && typeof value[0] === "string") {
    const data = value[1];
    if (data instanceof Uint8Array) return [value[0], data];
    if (typeof data === "string") return [value[0], new TextEncoder().encode(data)];
  }
  throw new TypeError(`expected typed [type, bytes], got ${typeof value}`);
}

/** In-process mutex via promise chain (one waiter queue per key). */
class KeyMutex {
  private tails = new Map<string, Promise<void>>();

  async run<T>(key: string, fn: () => Promise<T>): Promise<T> {
    const prev = this.tails.get(key) ?? Promise.resolve();
    let release!: () => void;
    const gate = new Promise<void>((r) => {
      release = r;
    });
    const next = prev.then(() => gate);
    this.tails.set(key, next);
    await prev.catch(() => undefined);
    try {
      return await fn();
    } finally {
      release();
      if (this.tails.get(key) === next) this.tails.delete(key);
    }
  }
}

export type ProxyCheckpointSaverOptions = {
  httpBaseUrl: string;
  runnerToken?: string;
};

export class ProxyCheckpointSaver extends BaseCheckpointSaver {
  private readonly base: string;
  private readonly staticHeaders: Record<string, string>;
  private readonly dispatcher = httpDispatcher();
  private readonly locks = new KeyMutex();

  constructor(opts: ProxyCheckpointSaverOptions) {
    super();
    this.base = opts.httpBaseUrl.replace(/\/+$/, "");
    this.staticHeaders = {
      [HEADER_FRAMEWORK]: "langgraph",
      "X-Runner-Kind": "typescript-langgraphjs",
    };
    if (opts.runnerToken) {
      this.staticHeaders["Authorization"] = `Bearer ${opts.runnerToken}`;
      this.staticHeaders["X-Runner-Token"] = opts.runnerToken;
    }
  }

  /** No persistent client to tear down (global fetch); kept for CheckpointerManager.closeFn parity. */
  async close(): Promise<void> {
    // fetch has no session; locks are GC'd with the saver instance.
  }

  private clientHeaders(): Record<string, string> {
    return { ...this.staticHeaders, ...tenantHeaders() };
  }

  private url(threadId: string, checkpointId?: string): string {
    const base = `${this.base}/internal/checkpoints/${encodeURIComponent(threadId)}`;
    if (checkpointId === undefined) return base;
    return `${base}/${encodeURIComponent(checkpointId)}`;
  }

  private latestUrl(threadId: string): string {
    return `${this.base}/internal/checkpoints/${encodeURIComponent(threadId)}/latest`;
  }

  private async encode(payload: Record<string, unknown>): Promise<Uint8Array> {
    const [typ, data] = await this.serde.dumpsTyped(payload);
    const tb = new TextEncoder().encode(typ);
    if (tb.length > 255) throw new Error("serde type name too long for proxy envelope");
    const out = new Uint8Array(1 + tb.length + data.length);
    out[0] = tb.length;
    out.set(tb, 1);
    out.set(data, 1 + tb.length);
    return out;
  }

  private async decode(raw: Uint8Array): Promise<Record<string, unknown>> {
    if (!raw.length) throw new Error("empty checkpoint blob");
    const n = raw[0]!;
    const typ = new TextDecoder().decode(raw.subarray(1, 1 + n));
    return (await this.serde.loadsTyped(typ, raw.subarray(1 + n))) as Record<string, unknown>;
  }

  private async putCas(
    threadId: string,
    key: string,
    body: Uint8Array,
    headers: Record<string, string>,
    opts: { etag: string | null; createOnly?: boolean },
  ): Promise<void> {
    const putHeaders: Record<string, string> = {
      ...headers,
      "Content-Type": "application/octet-stream",
    };
    if (opts.createOnly) putHeaders["If-None-Match"] = "*";
    else if (opts.etag) putHeaders["If-Match"] = opts.etag;
    else {
      // GET 200 without ETag would otherwise become an unconditional PUT
      // and clobber peers (gateway stripping ETag disables CAS).
      throw new Error(`checkpoint update refused: GET returned no ETag for ${threadId}/${key}`);
    }

    const init: FetchInit = {
      method: "PUT",
      headers: putHeaders,
      body,
      dispatcher: this.dispatcher,
    };
    const put = await fetch(this.url(threadId, key), init);
    if (put.status === 412) throw new CASConflict();
    if (!put.ok) {
      const body = await put.text();
      if (isRunNotInflight(put.status, body)) throw new RunNotInflight();
      throw new Error(`PUT checkpoint failed: ${put.status} ${body}`);
    }
  }

  private async tupleFromBlob(
    config: RunnableConfig,
    key: string,
    blob: Record<string, unknown>,
  ): Promise<CheckpointTuple> {
    const [ns, cid] = parseBlobKey(key);
    const cfgThread = config.configurable!.thread_id as string;
    const parentId = blob.parent_checkpoint_id as string | null | undefined;
    const parentConfig = parentId
      ? {
          configurable: {
            thread_id: cfgThread,
            checkpoint_ns: ns,
            checkpoint_id: parentId,
          },
        }
      : undefined;

    const pendingWrites: NonNullable<CheckpointTuple["pendingWrites"]> = [];
    for (const item of (blob.writes as unknown[]) || []) {
      const row = item as unknown[];
      const taskId = row[0] as string;
      const channel = row[1] as string;
      const [vt, vd] = asTyped(row[2]);
      pendingWrites.push([taskId, channel, await this.serde.loadsTyped(vt, vd)]);
    }

    const [ct, cd] = asTyped(blob.checkpoint);
    const [mt, md] = asTyped(blob.metadata);
    return {
      config: {
        configurable: {
          thread_id: cfgThread,
          checkpoint_ns: ns,
          checkpoint_id: cid,
        },
      },
      checkpoint: (await this.serde.loadsTyped(ct, cd)) as Checkpoint,
      metadata: (await this.serde.loadsTyped(mt, md)) as CheckpointMetadata,
      parentConfig,
      pendingWrites,
    };
  }

  async getTuple(config: RunnableConfig): Promise<CheckpointTuple | undefined> {
    const cfgThread = config.configurable!.thread_id as string;
    const threadId = storageThreadId(cfgThread);
    const ns = (config.configurable?.checkpoint_ns as string) ?? "";
    const checkpointId = getCheckpointId(config);
    const headers = this.clientHeaders();

    if (checkpointId) {
      const key = blobKey(ns, checkpointId);
      const init: FetchInit = { headers, dispatcher: this.dispatcher };
      const resp = await fetch(this.url(threadId, key), init);
      if (resp.status === 404) return undefined;
      if (!resp.ok) throw new Error(`GET checkpoint failed: ${resp.status} ${await resp.text()}`);
      const raw = new Uint8Array(await resp.arrayBuffer());
      return this.tupleFromBlob(config, key, await this.decode(raw));
    }

    const latestInit: FetchInit = { headers, dispatcher: this.dispatcher };
    const resp = await fetch(`${this.latestUrl(threadId)}?ns=${encodeURIComponent(ns)}`, latestInit);
    if (resp.status === 404) return undefined;
    if (!resp.ok) throw new Error(`GET latest checkpoint failed: ${resp.status} ${await resp.text()}`);
    const keyHeader = resp.headers.get(HEADER_CHECKPOINT_ID) || "";
    const key = keyHeader ? decodeURIComponent(keyHeader) : "";
    if (!key) {
      return this.getLatestViaList(config, threadId, ns, headers);
    }
    const raw = new Uint8Array(await resp.arrayBuffer());
    return this.tupleFromBlob(config, key, await this.decode(raw));
  }

  private async getLatestViaList(
    config: RunnableConfig,
    threadId: string,
    ns: string,
    headers: Record<string, string>,
  ): Promise<CheckpointTuple | undefined> {
    const init: FetchInit = { headers, dispatcher: this.dispatcher };
    const resp = await fetch(`${this.url(threadId)}?limit=${LIST_FETCH_LIMIT}`, init);
    if (!resp.ok) throw new Error(`LIST checkpoints failed: ${resp.status} ${await resp.text()}`);
    const items = (await resp.json()) as { checkpoint_id: string }[] | null;
    for (const item of items || []) {
      const key = item.checkpoint_id;
      const [itemNs] = parseBlobKey(key);
      if (itemNs !== ns) continue;
      const getInit: FetchInit = { headers, dispatcher: this.dispatcher };
      const get = await fetch(this.url(threadId, key), getInit);
      if (get.status === 404) continue;
      if (!get.ok) throw new Error(`GET checkpoint failed: ${get.status} ${await get.text()}`);
      const raw = new Uint8Array(await get.arrayBuffer());
      return this.tupleFromBlob(config, key, await this.decode(raw));
    }
    return undefined;
  }

  async *list(config: RunnableConfig, options?: CheckpointListOptions): AsyncGenerator<CheckpointTuple> {
    if (options?.filter) {
      throw new Error(
        "ProxyCheckpointSaver.list(filter=...) is not supported; omit filter or use POSTGRES_DSN direct mode",
      );
    }
    const cfgThread = config.configurable!.thread_id as string;
    const threadId = storageThreadId(cfgThread);
    const ns = (config.configurable?.checkpoint_ns as string) ?? "";
    const headers = this.clientHeaders();
    const lim = options?.limit ?? 10;
    const beforeId = options?.before ? getCheckpointId(options.before) : "";
    const fetchLimit = Math.max(lim * 4, LIST_FETCH_LIMIT);

    const init: FetchInit = { headers, dispatcher: this.dispatcher };
    const resp = await fetch(`${this.url(threadId)}?limit=${fetchLimit}`, init);
    if (!resp.ok) throw new Error(`LIST checkpoints failed: ${resp.status} ${await resp.text()}`);
    const items = (await resp.json()) as { checkpoint_id: string }[] | null;

    let n = 0;
    let skipping = Boolean(beforeId);
    for (const item of items || []) {
      const key = item.checkpoint_id;
      const [itemNs, itemId] = parseBlobKey(key);
      if (itemNs !== ns) continue;
      if (skipping) {
        if (itemId === beforeId) skipping = false;
        continue;
      }
      const getInit: FetchInit = { headers, dispatcher: this.dispatcher };
      const get = await fetch(this.url(threadId, key), getInit);
      if (get.status === 404) continue;
      if (!get.ok) throw new Error(`GET checkpoint failed: ${get.status} ${await get.text()}`);
      const raw = new Uint8Array(await get.arrayBuffer());
      yield await this.tupleFromBlob(config, key, await this.decode(raw));
      n += 1;
      if (n >= lim) return;
    }
  }

  async put(
    config: RunnableConfig,
    checkpoint: Checkpoint,
    metadata: CheckpointMetadata,
    newVersions: ChannelVersions,
  ): Promise<RunnableConfig> {
    void newVersions;
    const cfgThread = config.configurable!.thread_id as string;
    const threadId = storageThreadId(cfgThread);
    const ns = (config.configurable?.checkpoint_ns as string) ?? "";
    const checkpointId = checkpoint.id;
    const key = blobKey(ns, checkpointId);
    const parentId = config.configurable?.checkpoint_id as string | undefined;
    const headers = this.clientHeaders();

    await this.locks
      .run(`${threadId}\0${key}`, async () => {
        for (let attempt = 0; attempt < CAS_MAX_ATTEMPTS; attempt++) {
          let existingWrites: unknown[] = [];
          let etag: string | null = null;
          let createOnly = false;
          const getInit: FetchInit = { headers, dispatcher: this.dispatcher };
          const prev = await fetch(this.url(threadId, key), getInit);
          if (prev.status === 200) {
            etag = prev.headers.get("ETag");
            try {
              const decoded = await this.decode(new Uint8Array(await prev.arrayBuffer()));
              existingWrites = (decoded.writes as unknown[]) || [];
            } catch {
              existingWrites = [];
            }
          } else if (prev.status === 404) {
            // Same Pregel race as putWrites: a peer may create a writes
            // shell first. Create-only so we never wipe it with an
            // unconditional PUT of writes=[].
            createOnly = true;
          } else {
            const body = await prev.text();
            if (isRunNotInflight(prev.status, body)) throw new RunNotInflight();
            throw new Error(`GET checkpoint failed: ${prev.status} ${body}`);
          }

          const blob = {
            v: 1,
            checkpoint: await this.serde.dumpsTyped(checkpoint),
            metadata: await this.serde.dumpsTyped(metadata),
            parent_checkpoint_id: parentId ?? null,
            writes: existingWrites,
          };
          try {
            await this.putCas(threadId, key, await this.encode(blob), headers, {
              etag,
              createOnly,
            });
            return;
          } catch (err) {
            if (err instanceof RunNotInflight) throw err;
            if (err instanceof CASConflict) {
              if (attempt === CAS_MAX_ATTEMPTS - 1) {
                throw new Error(`checkpoint CAS failed after ${CAS_MAX_ATTEMPTS} attempts for ${threadId}/${key}`);
              }
              continue;
            }
            throw err;
          }
        }
      })
      .catch((err) => {
        if (err instanceof RunNotInflight) return;
        throw err;
      });

    return {
      configurable: {
        thread_id: cfgThread,
        checkpoint_ns: ns,
        checkpoint_id: checkpointId,
      },
    };
  }

  async putWrites(config: RunnableConfig, writes: PendingWrite[], taskId: string): Promise<void> {
    const cfgThread = config.configurable!.thread_id as string;
    const threadId = storageThreadId(cfgThread);
    const ns = (config.configurable?.checkpoint_ns as string) ?? "";
    const checkpointId = getCheckpointId(config);
    if (!checkpointId) {
      // Fail loud: a silent no-op would drop mid-superstep channel
      // writes and make HITL / crash resume look "fine" while state
      // is incomplete. LangGraph always supplies checkpoint_id here.
      throw new Error("ProxyCheckpointSaver.putWrites requires configurable.checkpoint_id");
    }
    const key = blobKey(ns, checkpointId);
    const headers = this.clientHeaders();

    await this.locks
      .run(`${threadId}\0${key}`, async () => {
        for (let attempt = 0; attempt < CAS_MAX_ATTEMPTS; attempt++) {
          let etag: string | null = null;
          let createOnly = false;
          let blob: Record<string, unknown>;
          const getInit: FetchInit = { headers, dispatcher: this.dispatcher };
          const resp = await fetch(this.url(threadId, key), getInit);
          if (resp.status === 404) {
            // Normal Pregel order: putWrites for the *next* checkpoint_id
            // lands before put creates that blob. Create a shell with
            // If-None-Match:* so a concurrent put that already wrote
            // cannot be overwritten.
            blob = {
              v: 1,
              checkpoint: await this.serde.dumpsTyped({}),
              metadata: await this.serde.dumpsTyped({}),
              parent_checkpoint_id: null,
              writes: [],
            };
            createOnly = true;
          } else if (resp.ok) {
            etag = resp.headers.get("ETag");
            blob = await this.decode(new Uint8Array(await resp.arrayBuffer()));
          } else {
            const body = await resp.text();
            if (isRunNotInflight(resp.status, body)) throw new RunNotInflight();
            throw new Error(`GET checkpoint failed: ${resp.status} ${body}`);
          }

          const encoded = [...((blob.writes as unknown[]) || [])];
          for (const [channel, value] of writes) {
            encoded.push([taskId, channel, await this.serde.dumpsTyped(value)]);
          }
          blob.writes = encoded;
          try {
            await this.putCas(threadId, key, await this.encode(blob), headers, {
              etag,
              createOnly,
            });
            return;
          } catch (err) {
            if (err instanceof RunNotInflight) throw err;
            if (err instanceof CASConflict) {
              if (attempt === CAS_MAX_ATTEMPTS - 1) {
                throw new Error(
                  `checkpoint writes CAS failed after ${CAS_MAX_ATTEMPTS} attempts for ${threadId}/${key}`,
                );
              }
              continue;
            }
            throw err;
          }
        }
      })
      .catch((err) => {
        if (err instanceof RunNotInflight) return;
        throw err;
      });
  }

  async deleteThread(threadId: string): Promise<void> {
    // CP has per-blob DELETE only — page through list then delete each.
    // A single LIST_FETCH_LIMIT page would silently leave overflow blobs.
    const storageId = storageThreadId(threadId);
    const headers = this.clientHeaders();
    for (;;) {
      const init: FetchInit = { headers, dispatcher: this.dispatcher };
      const resp = await fetch(`${this.url(storageId)}?limit=${LIST_FETCH_LIMIT}`, init);
      if (!resp.ok) throw new Error(`LIST checkpoints failed: ${resp.status} ${await resp.text()}`);
      const items = (await resp.json()) as { checkpoint_id: string }[] | null;
      if (!items || items.length === 0) return;
      for (const item of items) {
        const delInit: FetchInit = { method: "DELETE", headers, dispatcher: this.dispatcher };
        const del = await fetch(this.url(storageId, item.checkpoint_id), delInit);
        if (!del.ok && del.status !== 404) {
          throw new Error(`DELETE checkpoint failed: ${del.status} ${await del.text()}`);
        }
      }
      if (items.length < LIST_FETCH_LIMIT) return;
    }
  }
}
