/** Native AI SDK history, stored through the existing run-bound opaque API. */
import { CompletionDeliveryError } from "./completion.js";
import { runnerHeaders } from "./tenantCtx.js";
import { httpDispatcher, type FetchInit } from "./tls.js";

const MAX_BYTES = 16 << 20;

function wellFormed(text: string): boolean {
  for (const character of text) {
    const code = character.codePointAt(0)!;
    if (code >= 0xd800 && code <= 0xdfff) return false;
  }
  return true;
}

/** Walk before stringify: custom toJSON/getters must never change native state. */
export function stateJson(value: unknown): string {
  const ancestors = new Set<object>();
  const copy = (item: unknown): unknown => {
    if (item === null || typeof item === "boolean") return item;
    if (typeof item === "string") {
      if (!wellFormed(item)) throw new Error("state contains an invalid Unicode string");
      return item;
    }
    if (
      typeof item === "number" &&
      Number.isFinite(item) &&
      !Object.is(item, -0) &&
      (!Number.isInteger(item) || Number.isSafeInteger(item))
    )
      return item;
    if (typeof item !== "object" || ancestors.has(item)) throw new Error("state contains a non-JSON value or cycle");
    const prototype = Object.getPrototypeOf(item);
    if (Array.isArray(item) ? prototype !== Array.prototype : prototype !== Object.prototype && prototype !== null)
      throw new Error("state contains a non-JSON object");
    if (Object.getOwnPropertySymbols(item).length) throw new Error("state contains symbol keys");
    ancestors.add(item);
    try {
      const properties = Object.getOwnPropertyDescriptors(item);
      if (Object.values(properties).some((property) => property.get || property.set))
        throw new Error("state contains an accessor");
      if (Array.isArray(item)) {
        const keys = Object.keys(item);
        if (keys.length !== item.length || keys.some((key, index) => key !== String(index)))
          throw new Error("state contains a sparse or decorated array");
        return Array.prototype.map.call(item, copy);
      }
      const out: Record<string, unknown> = Object.create(null);
      for (const [key, descriptor] of Object.entries(properties)) {
        if (!descriptor.enumerable) continue;
        if (!wellFormed(key)) throw new Error("state contains an invalid Unicode key");
        // Native optional model-message properties may be absent or undefined.
        if (descriptor.value !== undefined) out[key] = copy(descriptor.value);
      }
      return out;
    } finally {
      ancestors.delete(item);
    }
  };
  return JSON.stringify(copy(value));
}

export type StateSnapshot<T> = { value: T | null; etag: string | null };

export class AiSdkState {
  constructor(
    private readonly baseUrl: string,
    private readonly runnerKind: string,
    private readonly runnerToken?: string,
    private readonly request: typeof fetch = globalThis.fetch,
  ) {}

  private url(threadId: string): string {
    return `${this.baseUrl.replace(/\/+$/, "")}/internal/checkpoints/${encodeURIComponent(threadId)}/adapter-state`;
  }

  private headers(): Record<string, string> {
    return { ...runnerHeaders(this.runnerToken, this.runnerKind), "X-Runkite-Checkpoint-Framework": "ai-sdk" };
  }

  async load<T>(threadId: string, signal?: AbortSignal): Promise<StateSnapshot<T>> {
    try {
      const init: FetchInit = {
        redirect: "error",
        headers: this.headers(),
        dispatcher: httpDispatcher(),
        signal: signal ? AbortSignal.any([signal, AbortSignal.timeout(30_000)]) : AbortSignal.timeout(30_000),
      };
      const response = await this.request(this.url(threadId), init);
      if (response.status === 404) {
        await response.body?.cancel();
        return { value: null, etag: null };
      }
      if (!response.ok) {
        await response.body?.cancel();
        throw new Error(`state read failed: HTTP ${response.status}`);
      }
      if (response.headers.get("X-Runkite-Checkpoint-Framework") !== "ai-sdk") {
        throw new Error("thread contains another framework's state");
      }
      const etag = response.headers.get("ETag");
      if (!etag || !/^"[0-9]+"$/.test(etag)) throw new Error("state read returned no strong numeric ETag");
      const reader = response.body?.getReader();
      if (!reader) throw new Error("state read returned no body");
      const parts: Uint8Array[] = [];
      let size = 0;
      try {
        while (true) {
          const { value, done } = await reader.read();
          if (done) break;
          size += value.byteLength;
          if (size > MAX_BYTES) throw new Error("AI SDK state exceeds the opaque checkpoint limit");
          parts.push(value);
        }
      } finally {
        await reader.cancel().catch(() => {});
      }
      const value = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(Buffer.concat(parts))) as T;
      stateJson(value);
      return { value, etag };
    } catch (cause) {
      throw new CompletionDeliveryError("AI SDK state could not be loaded safely", { cause });
    }
  }

  async save(threadId: string, value: unknown, etag: string | null): Promise<void> {
    try {
      const body = stateJson(value);
      if (Buffer.byteLength(body) > MAX_BYTES) throw new Error("AI SDK state exceeds the opaque checkpoint limit");
      const init: FetchInit = {
        redirect: "error",
        method: "PUT",
        body,
        headers: {
          ...this.headers(),
          "Content-Type": "application/octet-stream",
          ...(etag ? { "If-Match": etag } : { "If-None-Match": "*" }),
        },
        dispatcher: httpDispatcher(),
        signal: AbortSignal.timeout(30_000),
      };
      const response = await this.request(this.url(threadId), init);
      await response.body?.cancel();
      if (!response.ok) throw new Error(`state commit failed: HTTP ${response.status}`);
    } catch (cause) {
      throw new CompletionDeliveryError("AI SDK state commit was not confirmed", { cause });
    }
  }
}
