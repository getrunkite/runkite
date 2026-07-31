/**
 * Shared TLS/mTLS configuration for connections FROM this runner TO the
 * control plane -- both the gRPC bridge (proto.ts's createRunnerClient)
 * and the proxy-mode HTTP calls (store.ts, vectorstore.ts, a2a.ts).
 * Direct TypeScript/undici port of the Python runner's tls_utils.py --
 * same env vars, same semantics, same defaults.
 *
 * - RUNKITE_TLS_CA_FILE: verify the control plane's server certificate
 *   against this CA instead of the system trust store (it REPLACES the
 *   system store for that verification, not "in addition to" -- the
 *   same behavior credentials.createSsl and undici's own Agent
 *   `connect.ca` both already have). Required for a self-signed or
 *   internal-CA-signed control plane cert.
 * - RUNKITE_GRPC_TLS: enables gRPC TLS using the system trust store
 *   when RUNKITE_TLS_CA_FILE is NOT set -- the gRPC equivalent of what
 *   an https:// URL already gives HTTP for free. gRPC has no URL
 *   scheme to carry that signal the way HTTP's http:// vs https:// does,
 *   so without this there would be no way to ask for "TLS, but a
 *   publicly-trusted cert, no custom CA needed" on the gRPC side at all
 *   -- only "plaintext" or "TLS with a specific custom CA". Passes
 *   `null` for rootCerts to credentials.createSsl, which falls back
 *   through gRPC's own bundled default roots to Node's own system trust
 *   store (see @grpc/grpc-js's own createSsl source). Ignored
 *   (redundant, not a conflict) when RUNKITE_TLS_CA_FILE is also set.
 *   HTTP proxy-mode calls need no equivalent: fetch already defaults to
 *   system-trust verification for any https:// base URL with nothing
 *   extra set.
 * - RUNKITE_TLS_CLIENT_CERT_FILE / RUNKITE_TLS_CLIENT_KEY_FILE: this
 *   runner's own client certificate for mTLS, when the control plane
 *   requires one (TLS_CLIENT_CA_FILE / GRPC_TLS_CLIENT_CA_FILE on the
 *   Go side -- see cmd/tls.go). Independent of which trust store is in
 *   use above -- mTLS and server-cert verification are orthogonal.
 *
 * All are optional and off by default -- unset envs mean exactly
 * today's plaintext gRPC / whatever the httpBaseUrl's own scheme already
 * implies for HTTP, matching every other env-var-driven piece of this
 * runner's config (RUNNER_TOKEN, RUNKITE_GRPC_URL, etc).
 */
import { readFileSync } from "node:fs";
import { credentials, type ChannelCredentials } from "@grpc/grpc-js";
import { Agent } from "undici";

function readIfSet(path: string | undefined): Buffer | undefined {
  return path ? readFileSync(path) : undefined;
}

function isTruthyEnv(value: string | undefined): boolean {
  return /^(1|true|yes)$/i.test((value ?? "").trim());
}

/**
 * Returns TLS channel credentials for createRunnerClient, or undefined
 * if neither RUNKITE_TLS_CA_FILE nor RUNKITE_GRPC_TLS is set -- callers
 * should fall back to credentials.createInsecure() in that case,
 * preserving today's default plaintext behavior exactly.
 *
 * rootCerts is undefined (system trust store, via createSsl's own
 * fallback chain) when RUNKITE_TLS_CA_FILE is unset but
 * RUNKITE_GRPC_TLS is -- see this module's own doc comment for why
 * that combination needs to exist at all.
 */
export function grpcChannelCredentials(): ChannelCredentials | undefined {
  const caFile = process.env.RUNKITE_TLS_CA_FILE;
  if (!caFile && !isTruthyEnv(process.env.RUNKITE_GRPC_TLS)) return undefined;
  const rootCerts = readIfSet(caFile);
  const clientKey = readIfSet(process.env.RUNKITE_TLS_CLIENT_KEY_FILE);
  const clientCert = readIfSet(process.env.RUNKITE_TLS_CLIENT_CERT_FILE);
  return credentials.createSsl(rootCerts ?? null, clientKey ?? null, clientCert ?? null);
}

/**
 * Returns an undici Agent configured for TLS verification/mTLS against
 * the control plane's HTTP API, to pass as `{ dispatcher }` on fetch()
 * calls -- or undefined if RUNKITE_TLS_CA_FILE is unset, in which case
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
export function httpDispatcher(): Agent | undefined {
  const caFile = process.env.RUNKITE_TLS_CA_FILE;
  if (!caFile) return undefined;
  return new Agent({
    connect: {
      ca: readFileSync(caFile),
      cert: readIfSet(process.env.RUNKITE_TLS_CLIENT_CERT_FILE),
      key: readIfSet(process.env.RUNKITE_TLS_CLIENT_KEY_FILE),
    },
  });
}

/**
 * RequestInit widened with the `dispatcher` option -- the global fetch's
 * own TypeScript type doesn't declare it (a known Node.js typing gap;
 * see https://github.com/nodejs/node/issues/48977), even though the
 * runtime accepts it since Node's fetch is undici under the hood. Type
 * call sites' options objects as this (via an explicit `const opts:
 * FetchInit = {...}`, not an inline literal) so passing `dispatcher`
 * through to the global `fetch()` type-checks without an `any` cast.
 */
export type FetchInit = RequestInit & { dispatcher?: Agent };
