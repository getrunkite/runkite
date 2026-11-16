# Langflow MCP caller boundary

Status: Unreleased checkout recipe. No release or installation label changes.

The caller path is Langflow MCP Tools -> native LFX HTTP client -> the public
Runkite `/mcp` transport -> an existing managed runner. Runkite owns only the
specialist run. Langflow's project, flow, conversation and surrounding agent
loop remain external. No upstream fork, new runner adapter, model proxy,
policy engine, vault or embedded UI is introduced.

## Source and package pin

Read Langflow commit `260cbf347cc5563cc48c79094c175ca31532ea2b`:

- [`MCPToolsComponent`](https://github.com/langflow-ai/langflow/blob/260cbf347cc5563cc48c79094c175ca31532ea2b/src/lfx/src/lfx/components/models_and_agents/mcp_component.py)
  delegates discovery/conversion to `update_tools` and executes the resulting
  tool coroutine. The component propagates MCP `isError` as a failure.
- [`lfx.base.mcp.util`](https://github.com/langflow-ai/langflow/blob/260cbf347cc5563cc48c79094c175ca31532ea2b/src/lfx/src/lfx/base/mcp/util.py)
  supplies `MCPStreamableHttpClient`, native session pooling, one session-bust
  retry, header handling and JSON Schema -> StructuredTool conversion. This
  is the exercised native path, not source extraction or an MCP client mock.
- [`lfx` package metadata](https://github.com/langflow-ai/langflow/blob/260cbf347cc5563cc48c79094c175ca31532ea2b/src/lfx/pyproject.toml)
  declares version 1.13.2 and `mcp>=1.30.0,<2.0.0`. A prepared MCP 2.x
  environment therefore is not an equivalent native-client environment.

Qualification pins `lfx==1.13.2` and `mcp==1.30.0`. The PyPI LFX wheel's native
utility and MCP component match that commit byte-for-byte. The probe checks
SHA-256 `fc967fe3a496a682d67119404a3d4280dfbe1bc831b21d65f9e4f8e0f39e4e1f`
for the utility and
`e125822930136495277cddf45a2ea5a5588b8a5f8f3f2e00ead8f16f9f56273e`
for the component before execution. Selected protocol/conversion dependency
versions are recorded in its receipt, rather than implied by a mutable latest
install. The example does not lock every transitive dependency.

## Qualification mechanism

`examples/langflow_mcp/qualify.py` reuses the existing checkout qualification
supervisor for bounded process-group cleanup and sanitized environments.
`TestLangflowMCPQualification` starts the real SQLite API/auth middleware,
MCP HTTP server, gRPC bridge and Python LangGraph worker on ephemeral loopback
ports. The graph is scripted and has no model or downstream tool. A separate
interpreter imports the installed native LFX implementation without mocks or
module substitution, and uses `update_tools` then `StructuredTool.ainvoke`.

The client receives a new task-specific config directory, no ambient provider
or deployment credentials, disabled telemetry and an explicit loopback SSRF
host exception. TLS verification is not disabled. A network guard rejects
non-loopback connections. No Langflow user/project database or account is read.

Before dispatch, the probe saves and fsyncs the exact fixture intent. It tests
native discovery and bearer authentication, schema conversion, a real echo
result, fresh-session retry of the saved operation, changed-input conflict,
terminal graph failure, retry of that failure and rejected authentication.
The Go fixture independently requires two retained runs and exactly two real
runner assignments for the two intentional operations. There is no simulated
worker completion or fake successful response.

The baseline fixture uses published v0.9.0 read/write authority. The optional
`--scoped-source` mode requires the new source-only
`agents:echo_agent:run` grant only. The native probe exclusively calls `/mcp`;
the run grant admits its complete transport lifecycle, discovery and invocation,
including retained retries. It needs no REST observation authority. That mode
does not test the REST observation-reference protocol. A separate application
recovery implementation would need the additive observe grant; its conformance
belongs to the scoped-specialist feature tests.

The dedicated `.github/workflows/langflow-mcp.yml` runs both profiles using
separate client/runner Python 3.12 environments. It prepares Go modules before
the bounded qualifier, validates both fixed receipt schemas and retains only
their two allowlisted synthetic JSON files. Each profile starts its own local
plane/worker and gets a new receipt path. The scoped gate requires the matching
source authority feature, uses only the run grant and cannot fall back to broad permissions. Adding the
gate does not turn baseline evidence into scoped qualification.

## Limits and deployment checks

Observed November 16, 2026: the [native receipt](../../examples/langflow_mcp/qualification.json)
passed against the v0.9.0 baseline `a068c6b89d0cae5337881b73248f6fe865d43745`
with this uncommitted example and test. MCP negotiated `2025-11-25`. There were
exactly two retained runs and two real assignments despite reconnect and
failed-run retries. Installed LFX 1.13.2, MCP 1.30.0, LangChain Core 1.8.1 and
Pydantic 2.13.5 were used. The baseline read/write profile was tested; the
optional scoped profile still needs qualification in the integrated source.

The manual [application/operator recipe](../../examples/langflow_mcp/README.md)
uses the documented Langflow server-registration and MCP Tools UI. Native
client success does not prove visual-editor field binding, credential-variable
resolution, Table rendering, Langflow persistence, an agent's tool choices,
production load-balancer session affinity or real provider behavior. Those
must be checked in the adopting Langflow deployment before claiming support.

The stock component is not a durable Runkite receipt store. Its session retry
can repeat `tools/call`; stable application-owned IDs and retained Runkite runs
remain essential. Model-supplied IDs are not a reliable authority or retry
boundary. Raw MCP metadata can be lost during presentation, so source-only
run references must be saved by application code before REST observation.
Pruning ends create-retry protection. Unknown delivery, pending approval and
cancellation do not prove remote business effects or rollback.

No typed nested input-schema support is claimed here. The tested envelope
contains `message`, opaque `input`, `thread_id` and optional stable `run_id`.
`input` overrides `message`; reconnect after catalog changes. No automatic
approval, cancel grant or background MCP job surface is added.
