export declare const HEADER_CONNECTOR_SESSION = "X-Runkite-Connector-Session";
export declare class ConnectorError extends Error {
}
export interface ConnectorHelperOptions {
    controlPlaneUrl?: string;
    timeoutMs?: number;
    sessionToken?: string;
}
/** Mint a pre-authenticated connector session for this run. */
export declare function getConnectorSession(config: Record<string, any> | null | undefined, name: string, opts?: ConnectorHelperOptions & {
    userContext?: Record<string, unknown>;
}): Promise<Record<string, unknown>>;
/** Proxy one JSON-RPC request through the connector MCP gate. */
export declare function proxyConnectorMcp(config: Record<string, any> | null | undefined, name: string, request: Record<string, unknown>, opts?: ConnectorHelperOptions): Promise<Record<string, unknown>>;
