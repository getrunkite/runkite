# A specialist boundary between Trinity and Runkite

Status: checkout-only example included with v0.9.0 source. It adds no
control-plane endpoint, new runner kind or registry package. Published v0.8.0
does not include the incoming MCP stable-run-ID contract this example requires.

## Caller and existing mechanisms

A Trinity agent can need a specialist already operated through Runkite. The
existing `newMCPServer` and `mcpToolHandler` in `internal/api/mcpserver.go` expose
configured agents and dispatch their managed runs. Existing REST get/wait
handlers in `internal/api/runs.go` observe retained durable status. They are
enough for this integration; there is no reason to replicate a scheduler,
approval engine, vault, model loop or authoritative execution database.

Trinity owns the caller's native loop. Runkite owns the invoked specialist's
thread, assignment and durable result. These are distinct executions. A Trinity
parent does not automatically become a Runkite A2A parent, and its cancellation
does not automatically cancel the specialist. The example does not claim that
Trinity's other tools pass through Runkite governance.

## Why an explicitly installed stdio caller

The pinned public Trinity source handles external MCP configuration differently
across its native runtimes. Generic external HTTP entries do not have uniform
configuration support, and its HTTP validator restricts destinations. The
example therefore packages a trusted caller using Trinity's supported stdio
configuration shape, rather than claiming a universal URL-only integration.

`launch.py` executes the adjacent, operator-built Go binary. It uses no shell,
automatic download or dynamic command selection. The binary uses Runkite's
existing official Go MCP SDK dependency. This is an example asset, not another
command inside the Runkite control-plane binary.

The caller independently preserves the destination boundary: an operator-fixed
public HTTPS origin, normal certificate checks, all-address DNS validation and
pinned dialing, no redirects, no inherited proxy and bounded responses. A
literal-loopback HTTP exception is explicit and only for local qualification.
No tool argument can select a destination, credential or specialist. This is
not a way to weaken Trinity's network validator or make arbitrary HTTP calls.

## Intent and result ownership

`invoke_specialist` requires a caller-owned operation ID, thread ID and message.
The caller must retain the intended operation before dispatch. A new ID is new
work even when the message is identical. Hashing message text would incorrectly
collapse legitimate repeated turns.

Private local receipts freeze the endpoint, operator profile, agent, thread,
message and derived run ID before any tool invocation. Complete files are
published without replacing concurrent writers and synced before dispatch.
Receipts are caller bookkeeping; they contain input, never the bearer credential
or final output. The operator profile is not a server-verified tenant claim.
Rotating credentials must preserve the intended tenant/principal authority.

The first attempt discovers the configured tool and requires its `run_id`
schema. It records an attempt marker before calling Runkite. A later attempt
validates the retained run through REST and observes `/wait`; it never calls
the upstream tool again. Pruning between reads can make the result unavailable,
but cannot turn this client's retry into dispatch. A missing run, unknown queue
outcome, transport failure or incompatible identity remains an error requiring
reconciliation. The marker cannot prove that work began or did not begin.

This still is not exactly-once business execution. The initial queue write and
run insertion are separate operations; tools need their own effect idempotency.
Receipt and run retention are operational requirements. Pending work is not
successful output. Durable error, interruption and timeout remain tool errors.

## Authority and qualification

The Unreleased source update uses a dedicated tenant's named credential with
`agents:<specialist>:run` plus explicit `agents:<specialist>:observe`. Published
v0.9.0 used broader read/write authority and lacks this profile. Exact GET/wait
recovery checks the stored agent and SQL incarnation reference. The bridge saves
that reference in a separate private file and refuses later replacement. Before
a first reference is received, it cannot prove that the run ID was never deleted
and reused. Retention and saved attempt markers remain required.

These grants are agent-scoped, not requester-private. Existing creation can
continue a known tenant thread, so dedicated-tenant/single-caller guidance stays.
A fixed target in a trusted client does not narrow its credential. The native
runtime can access its environment; the wrapper is not a sandbox or vault. See
[the scoped contract](scoped-specialist-access.md).

The [example guide](../../examples/trinity_specialist_mcp/README.md) separates
three kinds of evidence: pinned upstream configuration functions, the actual
Runkite HTTP/gRPC/SQLite implementation with a real provider-free Python runner
and built stdio caller, and native Trinity runtime execution. Only actually
executed checks establish qualification. A configuration function test is not
a native CLI run, model-driven tool selection or a live Trinity deployment.

The normal CLI does not currently expose loopback host flags. Local protocol
qualification uses an authenticated loopback API/gRPC fixture rather than
binding the released CLI on wildcard addresses and describing it as loopback.
No model provider, external connector or public server is needed for that test.

### Local evidence, 2026-11-16

- All 28 example Go tests passed, including authenticated HTTP MCP calls,
  REST-only observations, strict argument parsing, DNS/origin constraints,
  bounded responses and competing receipt writers in eight separate processes.
  The affected API package passed its normal suite as well.
- Seven configuration checks passed against unmodified public Trinity commit
  `f70ebf6f231a981b032a965e3cc89a97f7ea6635`. They call its real validator,
  credential renderer and Claude configuration writer with temporary paths.
- The built bridge and copied launcher passed the real authenticated loopback
  API/gRPC/SQLite and Python LangGraph qualification. Four intended turns made
  exactly four upstream MCP tool invocations, four durable managed runs and
  four runner assignments. Restart, retained result replay, lost completion
  delivery, terminal error, two-turn checkpoint continuity, conflicting intent,
  missing retention and retention disappearing between reads were checked.
- The supervisor's interruption check passed locally. The sandbox prevented
  inspecting owned process states for the normal-exit and timeout assertions;
  those cases are explicitly skipped locally and run in the Ubuntu workflow.
  They are not counted as local passes.
- The public handbook passed its link and interaction checks. Go/Python source
  formatting and coordinated version metadata remain checked separately.

The dedicated `Trinity specialist example` workflow repeats the example's Go
tests with race detection, pinned upstream configuration, process cleanup and
the actual provider-free worker qualification. Its result belongs to the
tested commit. It does not establish native Trinity deployment, Claude/Gemini
CLI execution, account credential propagation or upstream acceptance.
