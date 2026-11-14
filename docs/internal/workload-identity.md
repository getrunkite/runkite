# Verified workload identity and assignment ownership

Availability: unreleased after v0.7.0. This is an opt-in strict deployment
profile. Existing kind tokens and execution pools keep their compatibility
behavior unless the profile is enabled.

## The authority we needed to separate

A pool credential answers which fleet may receive work. It cannot distinguish
two replicas holding that credential. Before this profile, a peer that learned
another assignment's run ID and generation could present the same fleet
authority. Generation fencing separates attempts, but does not identify the
holder within one attempt.

We reuse externally issued client certificates and the existing direct TLS
listeners. Runkite does not issue identities, store workload private keys or
operate a second identity provider. TLS proves possession of the certificate
key. The certificate's single URI SAN identifies the external workload, and an
operator allowlist maps that exact URI to permitted runner kinds or pools.
The SAN and SHA-256 SPKI fingerprint together identify the assignment holder.
Replicas may share a service URI if each receives a separate key. Cloned keys
and certificates represent the same holder and cannot be distinguished here.

## Configuration and deployment boundary

```sh
RUNKITE_WORKLOAD_IDENTITY=mtls
RUNKITE_WORKLOAD_IDENTITIES_FILE=/etc/runkite/workload-identities.json
RUNKITE_WORKLOAD_MAX_CERT_TTL=1h
```

The allowlist is bounded JSON, loaded once at process startup:

```json
{
  "identities": {
    "python-langgraph": ["spiffe://example.org/agents/langgraph"],
    "pool-finance": ["spiffe://example.org/agents/finance"]
  },
  "denied_key_sha256": []
}
```

Every configured token kind must be represented. Unknown fields/kinds, duplicate
URIs or key fingerprints, ambiguous JSON,
noncanonical URI SANs and certificate lifetimes outside the configured bound
fail closed. The maximum checks the leaf's full `NotAfter - NotBefore`
interval. Its default is one hour; operators may configure one minute through
24 hours. Issuance, key custody, uniqueness and revocation remain the external
issuer's responsibility.

For a shared service URI, place a compromised holder's lowercase 64-character
SPKI SHA-256 fingerprint in `denied_key_sha256` (at most 4096 entries). The
fingerprint appears in assignment and action evidence; it is not a secret.
Use the public-key fingerprint, not a whole-certificate digest. Retirement is
complete only after replacing or draining every HTTP and gRPC replica with the
updated file. Restart closes old streams. This is a bounded operator response,
not live external-CA revocation. Other keys under the service URI remain allowed.

Both listener triples are mandatory: `TLS_CERT_FILE`, `TLS_KEY_FILE`,
`TLS_CLIENT_CA_FILE`, and their `GRPC_TLS_` equivalents. HTTP and gRPC must reach
Runkite with direct verified client TLS. TLS termination followed by forwarded
identity headers is unsupported. The current HTTP listener is shared with the
client and Admin APIs, so enabling client-certificate verification also requires
their callers to present accepted client certificates. Do not enable this on a
public browser listener without planning that access boundary.

Supported strict storage is Postgres, MySQL or SQLite, with Redis or inprocess
queues. Strict NATS, Kafka, Mongo and custom transports without the explicit
ownership capability fail startup. Use the existing HTTP checkpoint/store/vector
proxies. Direct database credentials bypass this request boundary and must not
be given to an untrusted worker. This profile does not secure direct SQL access.

## Capturing and checking the holder

`auth.RunnerTokens.AuthenticateWorkloadTLS` requires `VerifiedChains`, one
allowed SPIFFE URI SAN, a bounded live client certificate and its verified
public key. A CN, an unverified presented certificate, a `worker_id`, or any
forwarded identity header supplies no authority. Existing fleet token and
tenant/agent pool checks remain required.

`transport.WithWorkloadIdentity` attaches an immutable value to the trusted
server context. `RunAssignment.WorkloadOwner` is additive protocol metadata.
The queue records that owner when it records the assignment in flight. The
inprocess implementation uses its queue lock. Redis stores a separate owner
hash in the same promotion script, preserving arbitrary input JSON bytes. Its
lookup reads assignment and owner atomically, and renew/ack/nack compare the
owner inside their mutation scripts. Redis checks expiry using its clock too;
keep replica and Redis clocks synchronized.

Owned queue mutations reject missing actors. Control-plane failed-delivery
cleanup and reaper acknowledgement use an explicit private trusted context;
they still require the exact retained generation. This queue capability is
separate from the explicit trusted SQL completion path and has no wire selector.

The bridge checks the owner before events, heartbeat and completion. Each
streamed message rechecks expiry. Cancel watchers are scoped by kind and owner,
and expire with their certificate proof. Run-bound internal HTTP requests derive
their owner from the same mTLS certificate and must match the queue assignment.
The status route additionally matches the requested run path to the bound run.
An allowlisted peer sharing the fleet token and service URI but holding another
key cannot take over the active assignment by guessing its run or generation.

`state.BindAttemptWorkload` records the owner under the durable SQL attempt
lock. The same generation cannot acquire another owner. Checkpoint writes,
usage reports and ordinary completion validate this durable owner under their
existing transaction locks. Completion replay also retains the original
holder requirement. Trusted control-plane cancellation, timeout, poison recovery
and broker-observed completion use an explicit internal system path; no HTTP
body or runner status field selects it.

General KV and vector proxy handlers authenticate the holder at HTTP request
admission. They do not share the checkpoint/usage/completion attempt lock with
arbitrary backing stores. An already admitted KV/vector or external operation
can finish after reclaim. This profile prevents a peer from starting another
holder's request; it does not promise instantaneous revocation or rollback of
every operation already in flight.

Owned `Nack` advances generation before requeueing, so a replacement can acquire
a different owner without reusing the prior durable authority. Reclaim already
advances generation and captures the new holder at the next dequeue. The queue
retry ceiling therefore also counts owned delivery failures that use Nack.
Monitor delivery/database failures before increasing that ceiling. The queue
and SQL transactions are separate operations. Failure before delivery retains
or requeues recoverable work; these checks do not make a distributed transaction
with external tools and do not provide exactly-once external execution.

## Expiry, rotation and revocation

The current strict profile uses the existing HTTP listener's required client
authentication. Direct Admin browsers and public API clients therefore also
need a client certificate trusted by that listener. Alternatively, an operator
can place a public ingress proxy in front of public/Admin routes and have that
proxy authenticate upstream with its own certificate. Keep that ingress URI
out of worker allowlists, restrict its forwarded paths, and keep runner/internal
routes private. A forwarded certificate header does not establish worker proof.
This is an external deployment pattern, not an embedded proxy or a qualified
vendor-specific configuration.

Both the original binding and current proof must remain unexpired. A new
certificate does not extend an old assignment, even if its URI and key remain
unchanged. Drain and finish assignments before rotation, or allow the ordinary
reclaim path to install a fresh generation and holder. Restart/rebuild runner
TLS clients together. Replacing certificate files during an active job can
make HTTP and gRPC present different proofs and is refused.
Set run timeouts and worker drain windows below the remaining certificate
lifetime. Retrying a job that can never finish before expiry adds recovery work
and can repeat effects that occurred before its durable receipt.

Python and TypeScript use the same client certificate/key environment variables
on both transports. Client material also enables secure gRPC with system server
roots. Python supplies httpx a prepared `SSLContext`, with the client chain
already loaded, because some httpx versions ignore `cert=` when `verify=` is a
CA path. When a client certificate is configured, both helpers reject non-HTTPS
requests before sending headers or bodies, including checkpoint, usage and
connector requests. Certificate-owned preflight also refuses plaintext before
constructing a client. HTTP without client credentials retains its compatibility
behavior. Neither helper silently drops a configured client key. Permanent
authentication/owner denials stop heartbeat and signal cooperative cancellation;
transient outages retain the existing retry behavior.

TypeScript gRPC TLS on Node.js 26 or newer requires a DNS endpoint and matching
certificate DNS SAN. The current `@grpc/grpc-js` supplies the target IP as TLS
SNI for literal-IP endpoints, which [Node.js rejects](https://nodejs.org/api/tls.html#tlsconnectoptions-callback).
The runner detects literal IPv4/IPv6 targets before starting its poll loop and
reports the DNS requirement. It does not override the verification hostname,
disable certificate verification or change how HTTPS IP SANs are checked.

Allowlist and key-retirement changes require restarting every replica. The
loaded policy is checked before every message and HTTP owner operation, but
editing its file alone does not change any running replica. Short-lived credentials
limit stale authority, but this profile does not poll CRLs, OCSP or a live
SPIFFE Workload API. It is not continuous process attestation. It cannot undo
side effects, stop arbitrary code that ignores cancellation, observe calls
outside the plane, or protect a compromised control-plane host or issuer.

## Upgrade and regression proof

Drain all runners and replace every control-plane replica before enabling the
profile. Do not mix strict and legacy replicas against the same queue or SQL
store. Existing ownerless in-flight work is not silently adopted. No additional
schema migration is required for the owner metadata itself; accompanying
features can require their own migrations.

`internal/bridge/workload_identity_test.go` exercises real verified mTLS with
two keys under the same SAN and fleet token, denied event/completion/heartbeat
and HTTP operations, foreign run paths, and replacement ownership after reclaim.
`internal/transport/conformance/workload.go` exercises persisted ownership,
peer mutation rejection, original input preservation and fresh generation on
owned Nack through inprocess and Redis queue clients. Shared SQL usage/attempt
tests cover same-generation ownership, stale holder rejection and durable writes.
These are bounded regression tests, not a hostile-code or identity-provider
certification.
