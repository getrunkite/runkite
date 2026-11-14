import { type ChannelCredentials } from "@grpc/grpc-js";
import { Agent } from "undici";
export declare function requireWorkloadHTTPS(url: string): void;
/** Reject the current grpc-js IP SNI incompatibility before its polling loop. */
export declare function validateGrpcTLSTarget(address: string, nodeVersion?: string): void;
/**
 * Returns TLS channel credentials for createRunnerClient, or undefined
 * if no CA, client certificate or RUNKITE_GRPC_TLS is configured. Callers
 * should fall back to credentials.createInsecure() in that case,
 * preserving today's default plaintext behavior exactly.
 *
 * rootCerts is undefined (system trust store, via createSsl's own
 * fallback chain) when RUNKITE_TLS_CA_FILE is unset but
 * RUNKITE_GRPC_TLS is -- see this module's own doc comment for why
 * that combination needs to exist at all.
 */
export declare function grpcChannelCredentials(): ChannelCredentials | undefined;
/**
 * Returns an undici Agent configured for TLS verification/mTLS against
 * the control plane's HTTP API, to pass as `{ dispatcher }` on fetch()
 * calls. Returns undefined when neither a custom CA nor client certificate is set;
 * otherwise client material also works with system server trust. Default
 * callers should omit the dispatcher option entirely and let fetch use
 * its own default (system trust store), correct as-is for both a plain
 * http:// base URL (the CA is simply unused) and an https:// one with a
 * publicly-trusted certificate.
 *
 * A custom Agent is the only way to give Node's built-in fetch a custom
 * CA/client cert at all -- the RequestInit type has no ca/cert/key
 * fields of its own; see https://undici.nodejs.org/#/docs/api/Agent.
 * Deliberately still called through the GLOBAL fetch (not undici's own
 * exported fetch) at every call site -- Node's built-in fetch honors
 * this same `dispatcher` option (it's undici under the hood) while
 * staying interceptable by tests that mock globalThis.fetch directly,
 * which importing undici's fetch export would silently stop being.
 */
export declare function httpDispatcher(): Agent | undefined;
/**
 * RequestInit widened with the `dispatcher` option -- the global fetch's
 * own TypeScript type doesn't declare it (a known Node.js typing gap;
 * see https://github.com/nodejs/node/issues/48977), even though the
 * runtime accepts it since Node's fetch is undici under the hood. Type
 * call sites' options objects as this (via an explicit `const opts:
 * FetchInit = {...}`, not an inline literal) so passing `dispatcher`
 * through to the global `fetch()` type-checks without an `any` cast.
 */
export type FetchInit = RequestInit & {
    dispatcher?: Agent;
};
