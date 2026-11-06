# Dependency and license scope

This record supports foundation review of Runkite. It describes selected direct
dependencies and retained notices, not a complete license audit or an assertion
that foundation intellectual property requirements have been satisfied.

Reviewed on 2026-11-06 against main commit
`edf36c133955aeea3d9c28b353e64b0a64a3a3b4`.

## First-party source and published versions

Current first-party source and v0.6.1 use Apache-2.0. The v0.6.0 release and
earlier published artifacts retain their shipped BUSL-1.1 terms, including
their change-license provisions. Dependencies and bundled assets retain their
own terms. See the [Apache-2.0 transition record](../internal/apache-2.0-transition.md)
for the release boundary, notice distribution paths and outstanding rights
review. That record does not certify permission to relicense every contribution.

## Direct dependency evidence

The [Go module declaration][go-mod] pins module versions. The [TypeScript runner
manifest][ts-manifest] and [Admin manifest][admin-manifest] generally declare
version ranges; their [runner lockfile][ts-lock] and [Admin lockfile][admin-lock]
record the resolved versions considered here. npm license fields are upstream
package metadata, not an independent verification of every distributed file.

Selected Go module license texts were checked in the public module cache at the
exact versions declared in `go.mod`:

- gRPC v1.83.2, OpenTelemetry v1.45.0, NATS client v1.52.0, Prometheus client
  v1.24.0 and MongoDB driver v2.8.0 carry Apache-2.0.
- pgx v5.10.0 and kafka-go v0.4.51 carry MIT. The go-redis v9.21.0,
  protobuf v1.36.11 and modernc SQLite v1.54.0 license files use BSD-style
  terms. coder/websocket v1.8.15 carries ISC.
- `github.com/go-sql-driver/mysql v1.10.0` and
  `github.com/hashicorp/golang-lru/v2 v2.0.7` carry **MPL-2.0**.
  The Go dependency set is not exclusively permissive.
- `github.com/modelcontextprotocol/go-sdk v1.7.0` includes an upstream
  MIT-to-Apache-2.0 transition notice. It retains MIT terms for contributions
  without relicensing consent and identifies documentation contributions,
  excluding specifications, as CC-BY-4.0. It should not be described as
  uniformly Apache-2.0.

The runner lockfile identifies gRPC and OpenTelemetry packages and optional
AI SDK `ai 7.0.155` as Apache-2.0. LangChain/LangGraph packages, `pg 8.22.0`,
`undici 8.12.2` and `zod 4.6.7` are recorded as MIT. The Admin lockfile records
React, Radix UI, TanStack Table and most other UI packages as MIT;
`class-variance-authority 0.7.1` as Apache-2.0; and `lucide-react 1.28.0` as ISC.

The [Python manifest][python-manifest] does not pin its base dependencies,
including gRPC, LangGraph, HTTP clients and `psycopg[binary,pool]`. Psycopg's
[current upstream metadata][psycopg-license] declares **LGPL-3.0-only**.
This is an explicit reciprocal-license disclosure, not verification of the
version or native libraries selected by every installation.

Python extras pin `agent-framework-core==1.23.0`, `openai-agents==0.24.0`,
`pydantic-ai-slim==2.63.0` and `google-adk==2.13.0`. Current upstream license
texts identify [Microsoft Agent Framework][maf-license],
[OpenAI Agents SDK][openai-license] and [Pydantic AI][pydantic-license] as MIT,
and [Google ADK][adk-license] as Apache-2.0. Those upstream checks do not
establish the contents or transitive dependency licenses of each pinned wheel.

## Bundled attribution

The root [NOTICE](../../NOTICE) identifies two retained third-party materials:

- Inter and JetBrains Mono fonts use OFL-1.1. Their copyright notices and
  license texts are in [admin-ui/public/licenses/](../../admin-ui/public/licenses/).
- Agent Protocol derived specification material uses MIT. Its original notice
  is retained in [spec/third-party/agent-protocol-MIT.txt](../../spec/third-party/agent-protocol-MIT.txt)
  and the `x-third-party-notices` field of `spec/openapi.json`.

These notices identify bundled material already recorded by the project.
They are not a complete notice bundle for every linked module or installed
runner dependency.

## Runtime infrastructure is separate

PostgreSQL and Redis form the supported external HA service pair. Alternative
storage and queue integrations are clients of separately deployed services.
Those server products, container base images and provider models retain their
own licenses. A client library's license does not establish its server's
license. The Runkite Apache-2.0 OCI label describes first-party code, not all
software installed in an image.

## Remaining review

Before making a comprehensive compliance statement, review the actual release
artifacts and their full dependency graphs, including Go indirect modules,
Python resolution and native wheels, npm transitive dependencies, examples,
build tools and container layers. Record the versions shipped and preserve the
required license texts, attribution and any source or redistribution obligations.
MPL and LGPL components need explicit consideration in that distribution review.

First-party contribution rights and any contractual restrictions also need
owner confirmation, as described in the transition record. This document does
not resolve those questions, certify existing binaries or attest to a
foundation's IP policy. It does not propose changing third-party licenses.

[go-mod]: https://github.com/getrunkite/runkite/blob/edf36c133955aeea3d9c28b353e64b0a64a3a3b4/go.mod
[python-manifest]: https://github.com/getrunkite/runkite/blob/edf36c133955aeea3d9c28b353e64b0a64a3a3b4/python/pyproject.toml
[ts-manifest]: https://github.com/getrunkite/runkite/blob/edf36c133955aeea3d9c28b353e64b0a64a3a3b4/typescript/runkite-runner/package.json
[ts-lock]: https://github.com/getrunkite/runkite/blob/edf36c133955aeea3d9c28b353e64b0a64a3a3b4/typescript/runkite-runner/package-lock.json
[admin-manifest]: https://github.com/getrunkite/runkite/blob/edf36c133955aeea3d9c28b353e64b0a64a3a3b4/admin-ui/package.json
[admin-lock]: https://github.com/getrunkite/runkite/blob/edf36c133955aeea3d9c28b353e64b0a64a3a3b4/admin-ui/package-lock.json
[psycopg-license]: https://github.com/psycopg/psycopg/blob/master/psycopg/pyproject.toml
[maf-license]: https://github.com/microsoft/agent-framework/blob/main/LICENSE
[openai-license]: https://github.com/openai/openai-agents-python/blob/main/LICENSE
[pydantic-license]: https://github.com/pydantic/pydantic-ai/blob/main/LICENSE
[adk-license]: https://github.com/google/adk-python/blob/main/LICENSE
