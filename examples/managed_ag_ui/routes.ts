/** Framework-neutral app routes. The application owns authentication/storage. */
import { cancelManagedChatRun } from "../managed_ai_sdk/bridge.ts";
import {
  AGUIInputError,
  attachManagedAGUIResponse,
  createManagedAGUIResponse,
  type ManagedAGUIOptions,
} from "./bridge.ts";

export type AGUIOperation = "create" | "attach" | "stop";
export type OwnedAGUITurn = Omit<
  ManagedAGUIOptions,
  "baseUrl" | "apiKey" | "fetch" | "timeoutMs" | "onCancelError"
> & {
  onMessageStart: NonNullable<ManagedAGUIOptions["onMessageStart"]>;
};
export interface AGUIRouteOptions {
  baseUrl: string;
  apiKey: string;
  fetch?: typeof globalThis.fetch;
  timeoutMs?: number;
  onCancelError?: () => void;
  /** Authenticate and check ownership on every operation. Persist new run/turn
   * identity BEFORE returning a create mapping. Never trust browser IDs as
   * Runkite IDs. Read any body only through a bounded clone. Return an app auth
   * error, or 204 on attach when no owned operation exists.
   */
  resolveTurn(
    request: Request,
    operation: AGUIOperation,
  ): Promise<OwnedAGUITurn | Response>;
}

export function createManagedAGUIRoutes(options: AGUIRouteOptions) {
  const { baseUrl, apiKey, fetch, timeoutMs, onCancelError, resolveTurn } =
    options;
  async function handle(
    request: Request,
    operation: AGUIOperation,
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
    const owned = { ...turn, baseUrl, apiKey, fetch, timeoutMs, onCancelError };
    try {
      if (operation === "create")
        return await createManagedAGUIResponse(request, owned);
      if (operation === "attach")
        return await attachManagedAGUIResponse(request, owned);
      await cancelManagedChatRun(owned);
      return new Response(null, { status: 204 });
    } catch (error) {
      if (error instanceof AGUIInputError)
        return Response.json({ message: error.message }, { status: 400 });
      throw error;
    }
  }
  return {
    create: (request: Request) => handle(request, "create"),
    attach: (request: Request) => handle(request, "attach"),
    stop: (request: Request) => handle(request, "stop"),
  };
}
