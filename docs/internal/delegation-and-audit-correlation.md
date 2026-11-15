# Constrained delegation and external audit correlation

Tracks [RFE #50](https://github.com/getrunkite/runkite/issues/50) and
[RFE #51](https://github.com/getrunkite/runkite/issues/51). Availability: v0.8.0.
These additions are not retroactive claims about v0.7.0.

## Why extend the existing boundary

Runkite already had `connector.oauth2TokenExchange`, external Vault database
leases and transactional action export. Replacing those with a new identity
provider, token database or security analytics engine would duplicate authority.
The missing link was a constrained exchange tied to the run's actual subject,
plus public correlation facts connecting one action to external audit records.

The user requesting a run, the configured agent, the worker holding an attempt
and a downstream delegated actor are distinct identities. A principal string is
not proof that a particular worker possessed a key. A successful token exchange
is not proof that a downstream operation executed.

## Admission identity evidence

`auth.JWTConfig.StrictIdentity` opts into configured issuer/audience, required
expiration and an explicit bounded subject. HTTPS JWKS retrieval refuses
redirects; the explicit development exception accepts literal loopback HTTP
only. Existing deployments retain their previous JWT compatibility behavior
when the profile is absent.

`models.IdentityProvenance` records issuer, subject, accepted audience, expiry
and at most four RFC 8693 `act` entries. These actor entries are assertions by
the JWT issuer, not independently verified actor key possession. Arbitrary
claims, bearer values and token IDs are excluded. The run manifest freezes a
copy, A2A inherits it from the stored parent, and SQL action creation captures
it from that manifest rather than accepting caller-provided evidence.

Expiry here describes the credential accepted at admission. It does not claim
that the original token remains usable throughout a long-running conversation.
The external issuer still checks a subject token when it is exchanged.

## Constrained RFC 8693 exchange

An optional connector `auth.token_exchange` enables registered `audiences`,
HTTPS `resources`, `max_ttl_seconds` and optional `actor_token_secret_ref` with
an operator-declared `actor_id`. At least one audience/resource and explicit
scopes are required. The actor assertion is resolved at use time through the
existing secret resolver. It is not a user OAuth token vault.

The constrained HTTP path requires JWT `strict_identity: true` and
`forward_token: true` at admission. `AuthResult` privately captures the exact
bearer token that authenticated that subject, independently of Extra claims,
claim aliases and forwarded headers. Private session serialization preserves
that typed assertion; `userContextFromAuth` copies it into the reserved
assignment field. The connector reads only that field. A supplied worker
body/header token must match it and cannot select another user's or tenant's
assertion. The new field cannot be seeded or overwritten by flat or nested
Extra, even when the typed assertion is absent.

`raw_token_field` remains an optional legacy passthrough setting. Naming it
`sso_token` does not establish this new proof. API-key or webhook Extra values
cannot establish it either. Pre-existing runs or sessions that contain only
an Extra token require fresh strict JWT admission; they are not promoted by an
upgrade. A2A inherits the typed assertion only alongside its verified frozen
parent principal and provenance. `on_behalf_of` cannot supply it, and the SDK
helpers omit the reserved field from their body while retaining ordinary
legacy claims.

This retains the explicit trust implication: the authenticating token reaches
the trusted runner through its assignment. Raw-token forwarding remains opt-in.
The assertion is excluded from the run manifest, action evidence and public
audit metadata. Applications wanting another identity exchange architecture
must provide a trusted integration, not hide a replacement token in tool
arguments.

The registered token endpoint must use HTTPS, with only an explicit loopback
development exception. Provider responses receive one strict object gate:
duplicate/case-variant protected fields, wrong types and explicit null scope
are rejected. The resulting token must be a bearer access token with a positive
TTL within the registered maximum. Returned scope may only narrow configured
scope. RFC omission of an unchanged scope remains valid. These checks constrain
what Runkite accepts; the issuer and resource server still enforce the actual
token's authority. This profile does not implement DPoP or claim key-bound
tokens from ordinary bearer results.

The nonsecret exchange contract participates in `target-v3-` fingerprints.
Changing its audience, resource, scopes, actor reference or maximum TTL requires
fresh managed approval. Existing v1/v2 descriptors retain their old meaning.
`DelegationContract` in action evidence records operator intent, not successful
exchange or verified actor-key use. Secret reference values are never exported.

## External audit join

`credentials.Manager` supplies server-owned `X-Runkite-Action-Id` and
`X-Runkite-Lease-Ref` headers to Vault operations. These are correlation labels,
not credentials or raw Vault lease handles. Vault returns public `request_id`
UUIDs. The provider accepts only canonical nonzero UUIDs as correlation facts;
arbitrary provider text is not copied into durable metadata.

Credential records retain issuance and latest successful renewal request IDs.
The final action observation and its transactional export also carry the
issuance request ID when known. If an acknowledgement is lost, its absence
remains absence. Never invent a request ID or retry issuance to obtain one.

An optional target setting `mcp.correlation_headers: true` sends the same public
action/lease references to the configured MCP service, including negotiated
transport. It is off by default and included in the target fingerprint. A
caller-supplied HTTP header cannot enable it or choose these references.

An external collector can join:

1. Runkite's `(tenant_id, action_id, sequence)` export, workload URI/key,
   admission issuer/subject, trace ID and optional credential reference.
2. Vault audit `request.id`, or explicitly configured audited correlation
   headers, to issuance/renewal/revocation requests.
3. A participating downstream service's audit record carrying the public action
   reference, or other authorized resource audit correlation.

Configure Vault audited headers deliberately and preserve its normal HMAC
redaction. Do not disable credential redaction or log usernames/passwords simply
to make a join easier. A Vault request proves a Vault API interaction. A proxy
response proves an observed response. Neither proves the business effect.
Use the existing SIEM/collector and receiver deduplication for at-least-once
events; there is no direct AuthMind API integration claim.

## Reconciliation and operator authority

A worker callback must present the exact current workload proof before it can
use credentials. Background `Manager.Reconcile` does not possess that worker's
private key. Its private liveness context checks current queue generation,
durable owner equality, expiry, policy and action instead. This check releases
no credentials and cannot be selected through HTTP fields.

The Admin-only renewal path similarly permits a deliberate operator renewal of
the exact current owner's lease. It does not extend a worker certificate or
release credential values. Revocation intentionally remains possible for an
old owner after reclaim. Treating missing worker proof as an automatic bypass
would be wrong; treating every background check as a hostile worker would revoke
healthy leases during ordinary reconciliation.

## Verification and limits

Tests cover wrong/missing admission claims, bounded actor capture, metadata
copying, registered target changes, response ambiguity, scope/TTL widening,
subject substitution, private secret errors, provider request correlation,
SQL round trips and reconciliation across owner changes. See the final
qualification record for executed commands and results.

External issuer integration, cryptographic delegation across arbitrary agents,
universal prompt-injection prevention, network discovery outside the plane,
independent assessment and global security rankings are not established by
these changes. Follow [the milestone plan](identity-and-distribution-plan.md)
and [workload identity](workload-identity.md) for the separate assignment boundary.
