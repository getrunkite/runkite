# Runkite: AAIF Sandbox thesis

Prepared for a Sandbox application against the [AAIF lifecycle policy](https://github.com/aaif/technical-committee/blob/main/governance/project-lifecycle-policy.md), updated October 15, 2026. Runkite is not an admitted AAIF project. This document describes its present implementation and proposed direction, not foundation endorsement.

## Who would use it, and why

An agent can answer a question before its operator can answer a different set of questions: who authorized the run, which credentials its tools used, why an action was approved, and what remains after a worker disappears. These operational questions persist when a team replaces one SDK or model with another. Reimplementing them inside every application couples operational authority to the framework that happened to ship first.

[Runkite](https://github.com/getrunkite/runkite) is a self-hosted control plane intended for teams operating agents across Python and TypeScript frameworks. Its intended users are application engineers who need durable managed execution, platform engineers sharing connector and approval controls across agents, and operators who need inspectable usage and audit records on infrastructure they own. A Go binary provides the plane and embedded Admin UI; separate runners retain their framework's model loop. Postgres and Redis are the supported high-availability pair.

The working implementation includes dispatch, durable Threads and Runs, streaming, cooperative cancellation, recovery, connector grants, human approval, secret resolution and reported usage accounting. Managed profiles include LangGraph, LangGraph.js, Vercel AI SDK, Microsoft Agent Framework, OpenAI Agents SDK, Pydantic AI and Google ADK. Each has a documented capability subset; this is not a claim to support every native framework feature. See the [implementation overview](../../README.md) and [limitations](../limitations.md).

## Interoperability without replacing the agent

Runkite offers two deliberate integration boundaries. A managed runner accepts assignments and reports events through its gRPC Runner Protocol. Clients create runs over HTTP and consume SSE or WebSocket output. An existing application can instead keep execution local and ask `POST /governance/decide` about one tool invocation. That second boundary creates no managed Thread or Run and applies no managed-run kill switch or spend cap. The caller executes after an allow and owns pending continuation.

MCP interoperability is already implemented. The `/mcp` endpoint uses the official MCP Go SDK and Streamable HTTP transport. `newMCPServer` exposes configured agents as tools; `mcpToolHandler` dispatches through the existing `createRunCtx` and `waitForRunResult` path rather than introducing separate execution semantics. Authentication, tenant ownership and session checks apply. Outbound connectors also let agents consume external MCP tools through Runkite's grants and approval path. See [MCP documentation](../mcp-server.md) and [source](../../internal/api/mcpserver.go).

These boundaries make the pre-adoption case concrete: an MCP client can invoke an operated agent, while a framework runner can keep its native loop and an existing model gateway. Tools that bypass the gate do not acquire its policy. Cancellation cannot reverse completed side effects. Runkite's internal agent delegation should not be confused with implementing the separate A2A interoperability specification. The [dependency scope](dependency-scope.md) distinguishes project code from SDKs and external services.

## Evidence and present community limits

The vLLM Semantic Router community accepted the proposed integration-blog topic in [issue #5879](https://github.com/vllm-project/semantic-router/issues/5879). The walkthrough is now [published on the vLLM Semantic Router blog](https://vllm-sr.ai/blog/runkite-ai-sdk-semantic-router/) following [blog PR #5883](https://github.com/vllm-project/semantic-router/pull/5883). A [public gist](https://gist.github.com/sharanharsoor/84f4dbd4913106d06f8b8156fa8f47a9) contains the sanitized patch and report for a working integration demo, outside Runkite main. The recorded local scenario uses native AI SDK, Semantic Router, Envoy and an Ollama model backend; vLLM serving was not tested. This is early external interest, not documented production adoption, an upstream partnership, or endorsement. Runkite operates the run, the SDK executes the agent, and Semantic Router routes model requests to configured backends.

Sharan Harsoor is currently the sole active maintainer. Runkite is not presenting a multi-organization community or claiming the adoption needed for Growth. Current source and v0.6.1 artifacts are Apache-2.0; previously published v0.6.0 artifacts retain BUSL-1.1. The [roadmap](../../ROADMAP.md) prioritizes reproducible integrations, contributor entry points and evidence from independent operators. Neutral governance would make these shared boundaries easier to influence before adoption patterns and ownership expectations settle.

## What would justify Growth

The objective is documented production use by at least two unaffiliated organizations, commits from at least two organizations over the preceding six months, named committers, and a public committer-acceptance process. A TC sponsor must accept a written growth plan defining Impact graduation, supported by sustained contributions and community participation. Graduation requires an absolute TC majority and Governing Board approval.

If admitted, Runkite would participate in the six-month sponsor checkpoint and aim to apply for Growth within twelve months; otherwise an Emeritus or archival discussion is expected. Sandbox provides standard infrastructure only, without mentorship, funding, marketing or security scanning. Admission also requires the applicable asset transfers and LF technical charter. The application seeks an accountable path toward a useful shared project, not a substitute for earning users or maintainers.
