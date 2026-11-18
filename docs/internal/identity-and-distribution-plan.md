# Identity and distribution milestone

This record tracks the v0.8.0 identity and distribution milestone following
v0.7.0. It separates implementation, qualification and publication from
external gates; it does not claim that independent assessment or production
soak has passed. Each RFE includes its caller, existing implementation, acceptance
criteria, boundaries and linked engineering record.

## Work order and traceability

1. [RFE #45: governance helper distribution](https://github.com/getrunkite/runkite/issues/45).
   Reuse the existing TypeScript and Python decision helpers. Verify built
   packages outside the checkout and wire publication without changing their
   decision-only semantics. See [distribution](governance-distribution.md).
2. [RFE #48: immutable publication recovery](https://github.com/getrunkite/runkite/issues/48).
   Build once, validate existing registry artifacts before accepting a rerun,
   distinguish an explicit missing version from a failed registry lookup, and
   record actual trusted-publishing outcomes. Only an observed genuine OIDC
   publication establishes live OIDC evidence.
3. [RFE #46: isolated usage](https://github.com/getrunkite/runkite/issues/46).
   Define explicit unknown, reported zero, partial and complete reports. Commit
   cumulative snapshots through the existing SQL completion boundary; retain
   provenance and settle committed reports without replay billing.
4. [RFE #49: workload identity](https://github.com/getrunkite/runkite/issues/49).
   Reuse externally issued client certificates, an operator URI-to-pool/kind
   allowlist and existing queue fencing. Bind each attempt to the authenticated
   URI, public key and certificate expiry. Shared fleet credentials alone do
   not establish individual workload identity.
5. [RFE #50: constrained delegation](https://github.com/getrunkite/runkite/issues/50).
   Extend existing RFC 8693 exchange with registered targets and actor material.
   Preserve bounded issuer-asserted identity metadata separately from secrets.
   Do not claim proof of possession for an ordinary bearer exchange.
6. [RFE #51: external audit correlation](https://github.com/getrunkite/runkite/issues/51).
   Extend existing action and credential metadata with safe correlation facts
   and a documented external audit join. Do not add another SIEM, vault,
   enterprise identity registry or network sensor.
7. [RFE #47: combined qualification](https://github.com/getrunkite/runkite/issues/47).
   Combine available disposable multi-plane, SQL/Redis, TLS and external-worker
   fixtures. Record the exact tested versions, faults and duration. Keep the
   external bridge experimental when broader qualification remains outstanding.
8. [RFE #52: independent assessment](https://github.com/getrunkite/runkite/issues/52).
   Supply a threat model, reproducible adversarial cases and reviewer handoff.
   Automated maintainer tests are not an independent assessment. An external
   reviewer and genuine deployment evidence remain external acceptance gates.

The combined RFE #47 composition is already implemented by
`scripts/test-isolated-ha.sh` and its manual Linux workflow, with a historical
hosted three-cycle pass on `96051cc` and a Linux baseline pass on `5c84c1f` in
[run 44062021430](https://github.com/getrunkite/runkite/actions/runs/44062021430)
(three cycles, 33.814 seconds). The Unreleased follow-up
[hardens the existing fixture and its evidence](constrained-execution.md#rfe-47-current-source-qualification-hardening-unreleased),
not a new runtime subsystem. Acceptance still requires executed available local
fixtures, exact source/artifact/version/fault/duration receipts, CI gates and
confirmed cleanup without paid cloud. Keep the external bridge experimental.
The frozen candidate passed locally on macOS: three cycles, nine managed cases,
55.693 seconds, gateway TLS, four controlled cancellation observations, three
live-generation observations and confirmed runtime/backend cleanup. See the
[evidence and artifact limits](identity-distribution-qualification.md#current-source-rfe-47-evidence-unreleased).
Candidate hosted qualification remains pending before RFE #47 acceptance;
RFE #52 stays a distinct independent-review gate and cannot be self-approved.

The order permits independent packaging and runtime work in parallel. Identity,
delegation and correlation must converge before the combined qualification.
Every implementation record must distinguish source behavior, fixture evidence,
published availability and outstanding external validation.

## Public and operator surfaces

Keep the README, changelog, API/OpenAPI, package READMEs, operator upgrade guide,
Admin/Review evidence displays, static support pages and engineering navigation
consistent with the implementation. The static website is deployed through
GitHub Pages and the existing public domain setup. A Cloudflare-hosted domain
does not require an AWS backend for documentation or the product homepage.
The optional live Admin availability page remains separate from the product.

New feature copy must name the version or say unreleased. A prepared publication
job does not make a helper a published package. A green rerun that verifies an
existing artifact does not prove that OIDC published it.

## Dependency PR reconciliation

The maintainer requested closing long-lived bot PRs. Live inspection on
2026-11-14 found twenty open Dependabot PRs with no submitted approvals. PR #28
was superseded by the already-resolved undici 8.12.2. The remaining proposals
were closed with an explicit handoff to [maintenance #53](https://github.com/getrunkite/runkite/issues/53),
which records their versions and compatibility work. They were not merged by
closing them. Major action/runtime/UI migrations remain separate from the eight
RFEs, and Dependabot/security alerts remain enabled. A second scheduled batch
of ten unreviewed dependency proposals (#54 through #63) was also consolidated
into that issue. No dependency upgrade was accepted merely by closing a PR.

## Release gate

Do not label this milestone independently assessed, universally isolated or
the most secure agent platform. Release qualification must identify unsupported
backends, deployment requirements, migrations and mixed-version restrictions.
External credential setup, actual first package publications, real OIDC proof,
independent review and production soak are marked pending until observed.

Executed results and their limits are recorded in the
[qualification record](identity-distribution-qualification.md).
