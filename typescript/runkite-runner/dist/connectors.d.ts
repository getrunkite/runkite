export declare const HEADER_CONNECTOR_SESSION = "X-Runkite-Connector-Session";
export declare class ConnectorError extends Error {
}
export interface ConnectorHelperOptions {
    controlPlaneUrl?: string;
    /** Total operation deadline. Defaults to 30s for session minting, 60s for MCP. */
    timeoutMs?: number;
    /** Pass the managed factory's signal to stop in-flight connector delivery. */
    signal?: AbortSignal;
    sessionToken?: string;
}
/** Mint a pre-authenticated connector session for this run. */
export declare function getConnectorSession(config: Record<string, any> | null | undefined, name: string, opts?: ConnectorHelperOptions & {
    userContext?: Record<string, unknown>;
}): Promise<Record<string, unknown>>;
/** Proxy one JSON-RPC request. Only an explicit CP session rejection permits renewal. */
export declare function proxyConnectorMcp(config: Record<string, any> | null | undefined, name: string, request: Record<string, unknown>, opts?: ConnectorHelperOptions): Promise<Record<string, unknown>>;
