# Dynamic database credential lifecycle

Availability: v0.7.0. This document describes the new
credential lifecycle package and the managed connector integration qualified by
the [bounded fixture record](security-completion-qualification.md).
It does not describe a feature in the published v0.6.1 binaries.

## The caller that needs it

A trusted external MCP database executor needs a short-lived PostgreSQL identity
for an approved reporting operation. The agent must not receive the username or
password, choose a Vault path, or substitute a more powerful database role.

The existing `vault:` connector `secret_ref` resolves one KV value. It does not
issue a database identity, track a Vault lease or prove revocation. Extending that
string resolver to return a username/password pair would hide important lifecycle
and failure semantics. The new `internal/credentials` package therefore coordinates
an external Vault database secrets engine, while the ordinary connector gate still
owns grants, pending approval and dispatch evidence.

Vault remains responsible for workload authentication, role ACLs, generation of
credentials, storage encryption and backend revocation. Runkite adds neither a
second vault nor an independent policy engine.

## Authority and the real consumer boundary

`credentials.Owner` contains the trusted tenant, run ID, immutable run incarnation,
generation, random action ID and registered consumer ID. The server derives these
fields from its assignment and durable action, rather than accepting them from
tool arguments. `Manager.ReferenceFor` produces an opaque metadata correlation ID
without fetching a secret or contacting Vault.

The operator registers a binding with a fixed Vault origin, namespace, database
mount, role, consumer and TTL ceiling. A binding revision includes that contract.
The connector target revision must include the immutable binding revision as well
as the ordinary endpoint configuration. Reusing a binding's display ID after a
role change must not transfer an earlier approval to the new role.

`Manager.Execute` accepts a trusted synchronous callback. Only that callback can
read the ephemeral `UserPassword.Values()`. The managed connector uses the values
as HTTP Basic authentication to its configured credential-aware MCP executor.
They do not enter JSON-RPC arguments, the runner session response, the model loop,
checkpoints, Admin or Review DTOs. The configured executor is trusted to handle the
credential. TLS protects that handoff; the local fixture uses an explicit loopback
exception. Ordinary MCP implementations that understand only a bearer token cannot
consume a PostgreSQL username/password pair without an adapter.

`examples/vault_database_executor` is a separate process. It exposes a fixed
`report_count` tool, opens a fresh PostgreSQL connection from Basic credentials,
counts synthetic reporting rows and closes the connection. It accepts no arbitrary
SQL and never echoes credential values. Initialization, ping and tool listing do
not require database credentials; resources and prompts are unsupported. This
example is not embedded in the Go control-plane binary.

## Durable intent before external operations

`Manager.Execute` records `issuance_intent` before the Vault request. The store has
a unique owner/binding key, so replaying the same action does not issue again or
repeat the consumer callback. After issuance, it durably records the encrypted
cleanup handle, actual returned TTL, renewal flag and one-time use start before
releasing credentials. Current authority is checked again after slow provider and
storage work. A stale owner receives no credential and its old lease is queued for
cleanup.

The lifecycle states deliberately distinguish requests from confirmations:

- `issuance_intent`: the request may be in flight. It is not proof of issuance.
- `not_issued`: a local preflight failure prevented a provider request.
- `issuance_unknown`: a response was lost or incomplete. The credential may exist.
- `active`: a complete grant was durably saved. Its TTL is the provider's grant.
- `renewal_intent` and `renewal_unknown`: requested renewal and uncertain outcome.
- `revocation_intent` and `revocation_unknown`: requested cleanup and uncertainty.
- `revoked`: synchronous provider success or an authenticated lookup confirming
  the exact lease is absent. A clock passing its expected expiry is insufficient.

These states describe credential access, not tool success. A completed tool can
have cleanup pending. A confirmed revocation does not undo a database operation or
prove that every downstream application closed a pre-existing connection.

## Vault protocol details that affect correctness

The database API creates a new credential with
`GET /v1/{mount}/creds/{role}`. There is no issuance idempotency key. An ambiguous
transport failure must not trigger another GET. The client disables redirects and
connection reuse, preventing `net/http` from replaying this GET on a reused failed
connection. Response sizes and request duration are bounded; errors omit provider
bodies, URLs, tokens and raw lease IDs.

Renewal uses `POST /v1/sys/leases/renew`. `increment` is a request, not a guarantee.
The manager records the returned `lease_duration` and refuses to use a grant above
the registered ceiling. The expected expiry starts at request initiation, which
is conservative when the response is delayed. Only the current exact owner can
renew. A stale attempt cannot renew or revoke a replacement attempt's lease by
guessing its reference.

Execution uses a callback context whose deadline falls before the actual returned
grant expiry. The HTTP/database consumer must use that context. There is no automatic
renewal loop; a manual renewal does not extend an already released callback's
deadline. A timeout can interrupt delivery while an external operation has already
started, so it still requires the ordinary action-evidence investigation.

Cleanup uses `POST /v1/sys/leases/revoke` with `sync: true`. The default API queues
revocation, so an unqualified success response would not prove backend cleanup.
Known-handle recovery uses `/sys/leases/lookup`. An arbitrary HTTP 404, permission
error or unavailable Vault server does not establish absence. The client recognizes
the provider's specific authenticated `invalid lease` response, otherwise retaining
an unknown outcome. The force-revoke and broad lease-list APIs are not used.

Vault enforces the database role's actual `default_ttl` and `max_ttl`. A local
`MaxTTL` check cannot bound a credential whose issuance response and handle were
lost. Operators must configure and protect the provider role's ceiling. Someone
with Vault role-administration rights can change that ceiling or the database
creation statements; Runkite does not attest those remote administrative changes.

## Protecting cleanup handles without storing credentials

`credential_leases` stores metadata plus a separate private `protected_handle`
column. It never stores a username, password or workload token. Raw provider lease
IDs are AES-256-GCM encrypted with fresh nonces. Associated data binds ciphertext to
the complete owner, record ID, binding ID and immutable binding revision, so copying
it to another tenant, generation, incarnation or action fails authentication.

The operator supplies a private keyring file. Its format is:

```json
{"active":"key-1","keys":{"key-1":"<base64-encoded 32-byte key>"}}
```

The keyring is outside the database, source tree and public configuration. Back it
up separately, restrict it to its process owner and deploy the same retained keys
to every replica. New encryption uses `active`; old keys must remain while any
lease may still need cleanup. Removing a key prevents recovery of its outstanding
handles. Database encryption does not replace filesystem isolation, host controls
or encryption of backups.

The Vault workload token is read from a separate operator-mounted token file on
every request. It may be refreshed by Vault Agent or another existing workload
authentication mechanism. The package does not store bootstrap credentials or
renew the workload identity itself. Both files must be regular files owned by the
process user without group/world permissions. Symlinks are rejected. A Vault Agent
file sink with restrictive permissions fits this contract; a symlink-based projected
mount must be materialized into a private regular file before use.

`Record` JSON and metadata search omit protected handles; `UserPassword` and provider
grants redact printing and JSON. Error paths use fixed reason codes. Secret values
exist briefly in trusted process memory. Go strings and third-party drivers can
copy them, so this is not a guarantee of memory zeroization.

## Restart, reclaim and outage recovery

The reconciler never issues another credential or replays a tool. It scans bounded
pages and rotates past active records, so a live old row cannot starve newer cleanup.
Rows whose provider or decryption key is unavailable also rotate without being
marked resolved. Lost-handle issuance records remain visible to metadata search but
are excluded from actionable recovery scans.
Known old handles remain revocable after run retention or generation changes; the
table has no cascade that deletes cleanup responsibility with the original run.
Renewal requires current authority, but cleanup of an exact old owner deliberately
does not. Revocation uses only the original binding revision's provider.

Retain old binding configurations with `Retired: true` until their outstanding
leases have confirmed cleanup. A changed Vault address or namespace is not a safe
place to send an old lease handle. Missing historical binding configuration or a
missing decryption key blocks cleanup and must remain visible as an operational
problem. Never fall back to the current binding merely because its name matches.

A lost issuance response may leave no usable cleanup handle. Such records stay
`issuance_unknown` and cannot be resolved by guessing a lease ID or granting broad
Vault listing privileges. Investigate the provider's own audit and lease records
and its configured role TTL. Do not label them revoked merely because an estimated
TTL elapsed. A lost SQL acknowledgement after issuance has a similar unavoidable
gap if neither durable handle storage nor immediate best-effort cleanup succeeds.

An active credential can still race with cancellation between the final check and
an external send. Cross-system transactions cannot make Vault issuance, SQL state,
queue generation and a database business effect one atomic operation. Existing
connector action evidence remains the record of intent and observed response.

## Source and qualification

- `internal/credentials/types.go`: immutable ownership, states and store contract.
- `manager.go`: issue/use intent, current-owner renewal, exact old-lease cleanup and
  bounded metadata reconciliation.
- `vault.go`: restricted role path, rotating token file and Vault HTTP operations.
- `protect.go` and `private_file*.go`: handle encryption and private file checks.
- `sqlstore.go`: shared SQL storage, CAS, tenant-scoped metadata search and private
  handle separation.
- `examples/vault_database_executor`: the external PostgreSQL credential consumer.

Unit tests cover single issuance across replicas, restart without replay, unknown
outcomes, stale generation/incarnation ownership, actual TTL, callback failure and
panic cleanup, authenticated ciphertext binding, token rotation, file permissions,
redirect rejection and opaque provider diagnostics. The opt-in live fixture is:

```sh
RUNKITE_TEST_VAULT_DATABASE=1 go test -count=1 -v \
  ./examples/vault_database_executor -run '^TestLiveVault'
```

It creates an owned local Vault process and PostgreSQL container with synthetic
data, a restricted workload token and an enforced provider role TTL. It tests real
database reads, rejected writes, renewal, revoked-login failure and lost provider
acknowledgements. A harness existing in source is not a passing qualification run;
record the actual run results before release. It is not a Vault HA failover soak,
live provider benchmark or proof of every backend plugin's revocation behavior.

## Primary references

- [Vault Database HTTP API](https://developer.hashicorp.com/vault/api-docs/secret/databases)
- [Vault lease lookup, renewal and synchronous revocation](https://developer.hashicorp.com/vault/api-docs/system/leases)
- [Vault lease concepts and TTL](https://developer.hashicorp.com/vault/docs/concepts/lease)
- [Vault PostgreSQL database plugin](https://developer.hashicorp.com/vault/api-docs/secret/databases/postgresql)
- [Vault Agent token-file auto-auth](https://developer.hashicorp.com/vault/docs/agent-and-proxy/autoauth/methods/token_file)

No feature here depends on Vault Agentic IAM, its agent registry or an Enterprise
agent policy engine. Availability of unrelated Vault features follows their own
licenses and editions.
