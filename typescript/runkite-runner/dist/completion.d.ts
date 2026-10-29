/** Keep heartbeats active until durable completion is accepted by the plane. */
import type { Metadata } from "@grpc/grpc-js";
import type { ReportStatusRequest, RunnerServiceClient, StreamEventsResponse } from "./proto.js";
export declare class CompletionDeliveryError extends Error {
}
export declare function finishRun(client: RunnerServiceClient, request: ReportStatusRequest, streamDone: Promise<StreamEventsResponse>, metadata: Metadata, options?: {
    attempts?: number;
    retryDelayMs?: number;
    rpcTimeoutMs?: number;
    streamTimeoutMs?: number;
    cancelStream?: () => void;
}): Promise<string | undefined>;
