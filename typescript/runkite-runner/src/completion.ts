/** Keep heartbeats active until durable completion is accepted by the plane. */
import type { ClientUnaryCall, Metadata } from "@grpc/grpc-js";
import type { ReportStatusRequest, ReportStatusResponse, RunnerServiceClient, StreamEventsResponse } from "./proto.js";

export class CompletionDeliveryError extends Error {}

async function bounded<T>(promise: Promise<T>, ms: number, onTimeout?: () => void): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      promise,
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => {
          reject(new Error("completion RPC timed out"));
          // Cancellation is cleanup. A failed cleanup cannot replace the timeout.
          try {
            onTimeout?.();
          } catch {
            /* keep the original timeout */
          }
        }, ms);
      }),
    ]);
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}

export async function finishRun(
  client: RunnerServiceClient,
  request: ReportStatusRequest,
  streamDone: Promise<StreamEventsResponse>,
  metadata: Metadata,
  options: {
    attempts?: number;
    retryDelayMs?: number;
    rpcTimeoutMs?: number;
    streamTimeoutMs?: number;
    cancelStream?: () => void;
  } = {},
): Promise<string | undefined> {
  const finalRequest = { ...request };
  try {
    const response = await bounded(streamDone, options.streamTimeoutMs ?? 30_000);
    if (!response.ok) throw new Error("plane did not acknowledge the event stream");
  } catch (error) {
    try {
      options.cancelStream?.();
    } catch {
      /* preserve the original transport failure */
    }
    // Leave the assignment recoverable without changing the agent's outcome.
    throw new CompletionDeliveryError("event delivery unconfirmed", { cause: error });
  }
  const attempts = options.attempts ?? 4;
  for (let attempt = 0; attempt < attempts; attempt++) {
    let pendingCall: ClientUnaryCall | void;
    try {
      const response = await bounded(
        new Promise<ReportStatusResponse>((resolve, reject) => {
          pendingCall = client.reportStatus(finalRequest, metadata, (err, value) =>
            err ? reject(err) : resolve(value),
          );
        }),
        options.rpcTimeoutMs ?? 5000,
        () => {
          if (pendingCall) pendingCall.cancel();
        },
      );
      if (response.superseded) return undefined;
      if (!response.ok) throw new Error("plane did not accept completion");
      return finalRequest.status;
    } catch (err) {
      if (attempt + 1 === attempts) throw new CompletionDeliveryError("completion delivery failed", { cause: err });
      await new Promise((resolve) => setTimeout(resolve, (options.retryDelayMs ?? 250) * 2 ** attempt));
    }
  }
  throw new CompletionDeliveryError("completion delivery failed");
}
