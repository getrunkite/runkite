/** Web Request/Response handlers for an application's existing Next or Nuxt routes. */
import {
  attachManagedChatResponse,
  cancelManagedChatRun,
  createManagedChatResponse,
  type ManagedChatAttachOptions,
  type ManagedChatOptions,
} from "./bridge.ts";

export type ChatOperation = "create" | "attach" | "stop";

export type OwnedChatTurn = Pick<
  ManagedChatOptions,
  "threadId" | "agentId" | "profile" | "expectedMessageId"
> & {
  runId: string;
  onMessageStart: NonNullable<ManagedChatAttachOptions["onMessageStart"]>;
};

export interface ChatRouteOptions {
  baseUrl: string;
  apiKey: string;
  fetch?: typeof globalThis.fetch;
  timeoutMs?: number;
  onCancelError?: () => void;
  /** Authenticate and check chat/turn ownership on EVERY operation. For create,
   * persist the turn/run mapping before returning it. Read the body only through
   * a bounded clone; the original stream belongs to the bridge.
   * Return 204 for attach when no owned operation exists, or an auth error.
   */
  resolveTurn: (
    request: Request,
    operation: ChatOperation,
  ) => Promise<OwnedChatTurn | Response>;
}

/** The app supplies authentication, durable mappings and assistant-ID binding.
 * These handlers never select credentials, profiles or run IDs from browser JSON.
 */
export function createManagedChatRoutes(options: ChatRouteOptions) {
  const connection = {
    baseUrl: options.baseUrl,
    apiKey: options.apiKey,
    fetch: options.fetch,
    timeoutMs: options.timeoutMs,
    onCancelError: options.onCancelError,
  };
  const resolveTurn = options.resolveTurn;

  async function handle(
    request: Request,
    operation: ChatOperation,
  ): Promise<Response> {
    const method = operation === "attach" ? "GET" : "POST";
    if (request.method !== method)
      return new Response(null, { status: 405, headers: { Allow: method } });
    if (request.signal.aborted) throw request.signal.reason;
    const turn = await resolveTurn(request, operation);
    if (turn instanceof Response) return turn;
    if (request.signal.aborted) throw request.signal.reason;
    if (typeof turn.onMessageStart !== "function")
      throw new Error("The application must bind assistant identity durably");
    const owned: ManagedChatAttachOptions = {
      ...connection,
      threadId: turn.threadId,
      runId: turn.runId,
      profile: turn.profile,
      expectedMessageId: turn.expectedMessageId,
      onMessageStart: turn.onMessageStart,
      disconnectPolicy: "detach",
    };
    if (operation === "create")
      return createManagedChatResponse(request, {
        ...owned,
        agentId: turn.agentId,
      });
    if (operation === "attach")
      return attachManagedChatResponse(request, owned);
    await cancelManagedChatRun(owned);
    return new Response(null, { status: 204 });
  }

  return {
    create: (request: Request) => handle(request, "create"),
    attach: (request: Request) => handle(request, "attach"),
    stop: (request: Request) => handle(request, "stop"),
  };
}
