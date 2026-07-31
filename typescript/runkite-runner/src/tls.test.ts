/**
 * Self-check for tls.ts: TLS/mTLS config for the gRPC channel and
 * fetch-based HTTP calls, mirroring the Python runner's
 * test_tls_utils.py -- same env vars, same semantics.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { grpcChannelCredentials, httpDispatcher } from "./tls.js";

// Good enough for the CA-only path -- grpcChannelCredentials/
// httpDispatcher just need a readable, PEM-shaped file to exercise the
// plumbing there; @grpc/grpc-js's root cert parsing doesn't validate
// the certificate's actual ASN.1 structure this early.
const FAKE_PEM = "-----BEGIN CERTIFICATE-----\nZmFrZQ==\n-----END CERTIFICATE-----\n";

// A real, static, self-signed EC cert/key pair (CN=runkite-test-dummy,
// 20-year validity) for the client-cert+key path specifically. Node's
// tls.createSecureContext (which @grpc/grpc-js's createSsl calls into)
// DOES validate a client cert/key's actual ASN.1 structure eagerly, at
// construction time -- unlike the CA-only path above and unlike
// undici's Agent (both lazy) -- so FAKE_PEM alone throws
// "asn1 encoding routines::too long" here. Never used for a live TLS
// handshake in this file (that's covered by the live e2e verification
// against a real control plane); this is purely so createSsl's own
// parsing succeeds.
const DUMMY_CERT = `-----BEGIN CERTIFICATE-----
MIIBjjCCATWgAwIBAgIUYNg7l64ZrzYYwe64t30F1oN4WKowCgYIKoZIzj0EAwIw
HTEbMBkGA1UEAwwScnVua2l0ZS10ZXN0LWR1bW15MB4XDTI2MDczMTA3NTIwN1oX
DTQ2MDcyNjA3NTIwN1owHTEbMBkGA1UEAwwScnVua2l0ZS10ZXN0LWR1bW15MFkw
EwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEyOp3weQRrudC0rqCQJowPvRhIppjSk74
qPpJLxipUSCwIGxpuZZCHNWtcZywldKo4f8+bs+fbJlga/pY4/ZmdaNTMFEwHQYD
VR0OBBYEFGbn9OmNNH3rIGPrhBIUBMkAAVDlMB8GA1UdIwQYMBaAFGbn9OmNNH3r
IGPrhBIUBMkAAVDlMA8GA1UdEwEB/wQFMAMBAf8wCgYIKoZIzj0EAwIDRwAwRAIg
MdIglKQWEVievUQF7CFTsJRKVfApuVtCxvGcJ7DRDPYCIH/CioRy88Uhcv1RELLB
yzMBnOx1tVVSFK5MRc5Qltrv
-----END CERTIFICATE-----
`;
const DUMMY_KEY = `-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgzH/FBLTb3LQRX8lv
AgGcWdFESl/5Kpl8yJuvqX3zKlChRANCAATI6nfB5BGu50LSuoJAmjA+9GEimmNK
Tvio+kkvGKlRILAgbGm5lkIc1a1xnLCV0qjh/z5uz59smWBr+ljj9mZ1
-----END PRIVATE KEY-----
`;

const ENV_KEYS = [
  "RUNKITE_TLS_CA_FILE",
  "RUNKITE_TLS_CLIENT_CERT_FILE",
  "RUNKITE_TLS_CLIENT_KEY_FILE",
  "RUNKITE_GRPC_TLS",
] as const;

function withEnv(vars: Partial<Record<(typeof ENV_KEYS)[number], string>>, fn: () => void): void {
  const prev: Record<string, string | undefined> = {};
  for (const key of ENV_KEYS) {
    prev[key] = process.env[key];
    delete process.env[key];
  }
  try {
    for (const [key, value] of Object.entries(vars)) process.env[key] = value;
    fn();
  } finally {
    for (const key of ENV_KEYS) {
      if (prev[key] === undefined) delete process.env[key];
      else process.env[key] = prev[key];
    }
  }
}

function tempCertFile(dir: string, name: string): string {
  const p = path.join(dir, name);
  writeFileSync(p, FAKE_PEM);
  return p;
}

test("grpcChannelCredentials() is undefined when RUNKITE_TLS_CA_FILE is unset", () => {
  withEnv({}, () => {
    assert.equal(grpcChannelCredentials(), undefined);
  });
});

test("httpDispatcher() is undefined when RUNKITE_TLS_CA_FILE is unset", () => {
  withEnv({}, () => {
    assert.equal(httpDispatcher(), undefined);
  });
});

test("CA file alone enables both gRPC and HTTP TLS without a client cert", () => {
  const dir = mkdtempSync(path.join(tmpdir(), "runkite-tls-test-"));
  try {
    const ca = tempCertFile(dir, "ca.pem");
    withEnv({ RUNKITE_TLS_CA_FILE: ca }, () => {
      assert.notEqual(grpcChannelCredentials(), undefined);
      const dispatcher = httpDispatcher();
      assert.notEqual(dispatcher, undefined);
      assert.equal(dispatcher!.constructor.name, "Agent");
    });
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("client cert + key together enable mTLS for both gRPC and HTTP", () => {
  const dir = mkdtempSync(path.join(tmpdir(), "runkite-tls-test-"));
  try {
    const ca = tempCertFile(dir, "ca.pem");
    const cert = path.join(dir, "client-cert.pem");
    const key = path.join(dir, "client-key.pem");
    writeFileSync(cert, DUMMY_CERT);
    writeFileSync(key, DUMMY_KEY);
    withEnv({ RUNKITE_TLS_CA_FILE: ca, RUNKITE_TLS_CLIENT_CERT_FILE: cert, RUNKITE_TLS_CLIENT_KEY_FILE: key }, () => {
      // grpcChannelCredentials doesn't expose its constructor args for
      // inspection (opaque ChannelCredentials object from @grpc/grpc-js)
      // -- not throwing while reading real cert/key files is the
      // meaningful assertion here; deeper behavior is covered by the
      // live e2e mTLS verification against a real control plane.
      assert.doesNotThrow(() => grpcChannelCredentials());
      assert.notEqual(httpDispatcher(), undefined);
    });
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("missing CA file throws instead of silently disabling TLS", () => {
  withEnv({ RUNKITE_TLS_CA_FILE: "/nonexistent/path/ca.pem" }, () => {
    assert.throws(() => grpcChannelCredentials());
    assert.throws(() => httpDispatcher());
  });
});

// RUNKITE_GRPC_TLS=1 alone (no CA file) enables gRPC TLS using the
// system trust store -- the "publicly-trusted cert, no custom CA
// needed" path gRPC has no URL-scheme signal for the way HTTP's
// https:// does. Must NOT leak into httpDispatcher(): HTTP needs no
// equivalent flag at all.
test("RUNKITE_GRPC_TLS=1 alone enables gRPC TLS (system trust) without affecting HTTP", () => {
  withEnv({ RUNKITE_GRPC_TLS: "1" }, () => {
    assert.notEqual(grpcChannelCredentials(), undefined);
    assert.equal(httpDispatcher(), undefined);
  });
});

test("RUNKITE_GRPC_TLS accepts common truthy spellings and rejects others", () => {
  for (const [value, wantEnabled] of [
    ["1", true],
    ["true", true],
    ["True", true],
    ["yes", true],
    ["0", false],
    ["false", false],
    ["", false],
  ] as const) {
    withEnv({ RUNKITE_GRPC_TLS: value }, () => {
      const enabled = grpcChannelCredentials() !== undefined;
      assert.equal(enabled, wantEnabled, `RUNKITE_GRPC_TLS=${JSON.stringify(value)} -> enabled=${wantEnabled}`);
    });
  }
});

test("RUNKITE_TLS_CA_FILE set takes precedence over RUNKITE_GRPC_TLS (no conflict)", () => {
  const dir = mkdtempSync(path.join(tmpdir(), "runkite-tls-test-"));
  try {
    const ca = tempCertFile(dir, "ca.pem");
    withEnv({ RUNKITE_TLS_CA_FILE: ca, RUNKITE_GRPC_TLS: "1" }, () => {
      assert.doesNotThrow(() => grpcChannelCredentials());
    });
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
