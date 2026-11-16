# Exact positive policy identities

Status: Unreleased, source checkout only. Published v0.9.0 predates this change.

## Boundary and source proof

Configured graph keys pass unchanged through `ParseGraphEntries`, agent
bootstrap, exact SQL agent lookup, run admission and assignment binding.
Connector configuration and `Registry.Get` also use exact keys. MCP request
parsing preserves tool names into the bytes forwarded for dispatch. The static
policy evaluator previously trimmed those identities for both grant lookup and
overlay merging. SQL grant rows themselves retain their original strings.

A provider-free full-handler regression registers `echo` and ` echo `, admits a
caller authorized only for the latter, dequeues its assignment, and authenticates
the current runner kind/token/generation. A connector session granted only to
`echo` must return 403 rather than the synthetic downstream credential. The
regression covers both configured grants and real Admin SQL grant CRUD/reload.
It also covers distinct exact connector names. The Admin grant form preserves
its selected row ID and tenant/agent/connector bytes on create and edit. Tool
names use JSON string arrays instead of lossy comma/whitespace splitting; invalid
or ambiguous text is rejected before a write. Form roundtrip tests and real Admin
POST/PUT tests cover padded identities, comma/space tool names and exact row
selection. Grant-list filters preserve the selected exact scope as well.
App-owned Decide tests cover exact
authenticated tenant, agent, connector, tool and optional tenant-claim identity.

## Positive and restrictive semantics

Only exact tenant/agent/connector scope can confer a grant. Only an exact wire
tool name can match a positive allow entry. Grant overlay keys remain Go struct
tuples, so embedded separator bytes cannot collapse two scopes.

Restrictions intentionally preserve coverage. Predicate normalization/matching
and mandatory-HITL normalization/overlay behavior remain unchanged. A separate
restrictive-only pass retains tool-filter denials across legacy trim-equivalent
grant scopes, considering both the old trimmed tool and exact tool checks before
positive lookup. This avoids turning a previous denial into default allow.
Ambiguous padded configurations can deny more; the compatibility pass never
confers authority and uses the existing generic tool-denied reason. There is no
new policy mode, identifier redesign or automatic rule migration.

The decision cache now uses a comparable struct tuple instead of delimiter
concatenation. A regression covers separator-containing connector/tool pairs
that previously aliased an allowed cached decision. Its key also includes the
policy revision captured by `Engine.Decide`. A no-network controlled webhook
regression demonstrates the previous race: an old evaluation resumed after a
reload, repopulated a cleared cache with allow, and a new evaluation reused it.
The old revision namespace can no longer supply the current decision.

## Approval and upgrade scope

The existing policy fingerprint includes an explicit evaluator-semantics value.
A managed-approval regression uses a frozen pre-upgrade fingerprint with the same
configured rules. New approval refuses that old pending authority; an already
approved capability cannot be consumed; the current decision remains pending and
requires a new action ID. Historical rows remain unchanged.

Standalone app-owned approval rows intentionally do not pin managed policy/target
revisions. Already returned app-owned approvals and allow decisions are not
revoked by this change. Operators must pause those continuations and resubmit the
saved proposals under the new semantics, using the existing app expiration/review
contract. See [upgrade guidance](../ops-runbook.md#unreleased-policy-identity-upgrade).

Focused regressions use SQLite, real request middleware and synthetic credentials,
with no external provider calls. Full socket and live qualification are performed
by the integrating maintainer. This change does not claim that arbitrary local
tool bodies enforce connector policy or that cancellation undoes prior effects.
