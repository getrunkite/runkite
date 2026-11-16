# A Trinity agent calling a Runkite specialist

Unreleased, source-checkout-only scoped caller update. Published v0.9.0 does
not provide the scoped grants and observation references this updated bridge
uses. The original v0.9.0 example used broader dedicated-tenant authority. This directory is
checkout source, not a separately published package or a Trinity upstream
integration.

A Trinity agent may need a specialist that already runs in Runkite. Keep the
Trinity agent's existing native loop and let it call that specialist as an MCP
tool. Runkite owns only the specialist's managed thread and runs. The surrounding
Trinity conversation does not become a Runkite run.

```text
Trinity agent, native Claude Code loop
  -> stdio MCP: invoke_specialist(operation_id, thread_id, message)
  -> Python launcher, adjacent Go bridge
  -> HTTPS: /mcp for first dispatch, REST for later observation
  -> existing Runkite-managed specialist runner
```

The bridge fixes the destination and specialist in operator configuration.
`launch.py` executes the adjacent `runkite-specialist-mcp` binary without a shell,
download or runtime installation. It adds no second model runtime, policy
engine, vault, scheduler or workflow coordinator.

## Configure Runkite first

Use a checkout containing the new `/mcp` `run_id` support and start your existing
Runkite deployment and runner. The existing
[echo agent](../echo_agent/graph.py) is a suitable deterministic first specialist.
Check that `tools/list` advertises `run_id` before relying on retry protection;
the bridge refuses servers that do not advertise it.

Provide a dedicated tenant with a named server-side API key whose explicit
permissions are `agents:<specialist_id>:run` and
`agents:<specialist_id>:observe`. Use the configured specialist ID, such as
`echo_agent`. The first grant permits scoped MCP discovery/invocation; the
second permits only exact public run GET/wait recovery, checked against the
stored agent. No general `read`, `write` or Admin grant is needed. The profile
requires a SQL backend with durable run incarnations.

The observation grant is agent-wide, including older retained runs. It is not
requester-only privacy, and run creation can continue a known tenant thread.
Keep the dedicated tenant/single trusted caller boundary. A fixed bridge target
alone never narrows server authority. Existing broad credentials still work,
but give the caller broader rights. Published v0.9.0 lacks this scoped profile.

Expose Runkite through your existing HTTPS reverse proxy. Configure the bridge
with the origin, such as `https://runkite.example.com`, without `/mcp`, a query,
user information or a fragment. It appends the existing API paths itself. See
[MCP server behavior](../../docs/mcp-server.md) for session affinity and retry
boundaries, and [authentication](../../docs/configuration.md) for plane setup.

## Build and package the Trinity agent

From the Runkite repository root, build for the architecture of your Trinity
agent container. This example selects Linux amd64; use `GOARCH=arm64` if your
container runs on Linux arm64:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -o examples/trinity_specialist_mcp/runkite-specialist-mcp \
  ./examples/trinity_specialist_mcp

agent_package_dir="$(mktemp -d /tmp/runkite-trinity-agent.XXXXXX)"
cp examples/trinity_specialist_mcp/CLAUDE.md \
  examples/trinity_specialist_mcp/template.yaml \
  examples/trinity_specialist_mcp/.mcp.json.template \
  examples/trinity_specialist_mcp/launch.py \
  examples/trinity_specialist_mcp/runkite-specialist-mcp \
  examples/trinity_specialist_mcp/.gitignore \
  "$agent_package_dir/"
cp LICENSE NOTICE "$agent_package_dir/"
printf '%s\n' "$agent_package_dir"
```

Edit `.mcp.json.template` in the printed staging directory before deployment:

- `RUNKITE_BRIDGE_URL`: the operator-approved public HTTPS origin.
- `RUNKITE_BRIDGE_AGENT`: the fixed configured specialist, initially `echo_agent`.
- `RUNKITE_BRIDGE_PROFILE`: a stable local profile name for this authority and
  destination. It is caller bookkeeping, not a server-verified tenant identity.
- `RUNKITE_BRIDGE_STATE`: an absolute persistent directory with an existing
  parent. Use a clean path without dot components or a trailing slash. The
  template uses `/home/developer/.runkite-specialist`.
- Keep `RUNKITE_API_KEY` as the `${RUNKITE_API_KEY}` placeholder. Supply its value
  through Trinity's credential setup, never in the template, arguments or Git.

At the pinned Trinity revision below, agent files are placed directly in
`/home/developer`, not `/home/developer/project`. The launcher and compiled binary
must therefore sit next to one another at the root of this agent package. The
agent home is a persistent volume; protect and retain its bridge state across
restarts. Intent receipts contain conversation input even though they do not
store final specialist output. Keep them out of Git, reports and shared folders.
The bridge creates only its final private state directory with mode `0700`;
it does not recursively create missing parents. Prepare the parent explicitly
inside the agent container or when provisioning its persistent volume:

```sh
mkdir -p /home/developer
# For a custom RUNKITE_BRIDGE_STATE, create its parent here instead.
```

Before accepting new or existing state, the bridge fsyncs the parent directory.
Before using a loaded intent or run reference, it fsyncs the state directory
itself. Each caller establishes these barriers even when another process has
just published the same file. A failed barrier stops the invocation before any
MCP call when preparing or loading caller state. A failed response-reference
publication instead leaves the already-sent operation unknown. This requires a
local filesystem that supports directory fsync and exclusive hard-link
publication. A readable receipt alone is insufficient
proof of persistence. These checks do not defend against another process
already running as the same OS user, filesystem rollback or manual deletion.

Keep this staging directory outside Git. Trinity's Git-aware archive path omits
ignored untracked files, including the compiled binary this repository correctly
ignores. Deploy the fresh prepared directory, not this source directory. For a
Git-based agent deployment, install the matching built binary separately before
enabling the MCP entry; no startup download is supplied.

Deploy this prepared directory through your existing Trinity installation's
agent deployment workflow. Select Claude Code, provide the dedicated Runkite key
and confirm the `runkite-specialist` MCP entry appears before invoking it. This
example adds no automatic deployment, platform installation or model billing.
Native Claude Code execution and environment propagation still require the
operator's runtime qualification described below.

## Invoke, observe and continue

The calling application must allocate and persist an operation ID, thread ID
and exact message before the first tool call. Use a new operation ID for each
intentional turn. A random ID remembered only by the model is insufficient: a
lost response must not cause the model to invent another ID and repeat work.
`CLAUDE.md` documents that rule for this template; application code remains
responsible for enforcing request ownership and identity reuse.

Invoke the specialist using the caller's saved values:

```json
{
  "operation_id": "caller-saved-operation-001",
  "thread_id": "caller-saved-specialist-thread",
  "message": "Summarize this task in one sentence."
}
```

IDs use 1 to 128 ASCII characters: the first must be a letter or digit, followed
by letters, digits, `_`, `.`, `:` or `-`. Messages must contain non-whitespace
text and fit within 8,192 UTF-8 bytes. This deliberately narrow example does not
accept arbitrary structured agent input or select another specialist per call.
Encoded tool arguments are capped at 32 KiB. Upstream responses are capped at
4 MiB, with a narrower 1 MiB decoding limit on REST observations. An oversized
result requires investigation through Runkite's existing APIs/Admin; it must
not cause a fresh operation ID or another dispatch.

Before dispatch, the bridge saves an immutable local intent binding the operation
to the profile, configured destination, agent, thread and message. Its stable
Runkite `run_id` is recorded there. Reusing an operation with different arguments
is rejected. A separate attempt marker records that delivery may have begun.

For a saved operation, the first content item and structured result contain
`operation_id`, `run_id`, `thread_id` and `outcome`. Remaining content carries the
specialist's result or failure. Use the returned run ID to inspect the specialist
in Runkite Admin. The outcomes are:

- `success`: Runkite returned a successful specialist result.
- `tool_error`: Runkite returned a tool-level failure, including a terminal run
  error, interruption or timeout. Do not turn it into an automatic fresh task.
- `delivery_unknown`: observation or result delivery did not complete. The
  worker may still run. Observe with the exact same saved arguments.
- `rejected`: the bridge refused the operation, configuration or unsafe retry.
  Resolve the reported problem without replacing its saved identity.

The delivery deadline is 45 seconds. It bounds this request, not the specialist's
execution. A timeout, disconnect or restarted caller does not prove cancellation.
On another invocation of an attempted operation, the bridge establishes a fresh
authenticated MCP session and checks discovery, then reads and validates the
retained run's identity through REST. It saves the SQL run incarnation reference in a separate private immutable
`.run-reference` file and sends it as `Runkite-Expected-Run-Reference` on
completion observation through `/runs/{run_id}/wait`. Later reads also send the
reference. Successful initial MCP results can provide the same reference in
`_meta["runkite/run_reference"]`, which the bridge saves before reporting it. It does not call the upstream agent tool again.
A missing run or changed saved reference, including deletion/recreation between
those two reads, fails closed instead
of creating replacement work. Do not interpret an unknown result as a successful
or failed external side effect.

For the next intentional conversational turn, keep the same thread ID and save a
new operation ID with the new message. Keep one outstanding turn per thread.
This is native continuity of the selected Runkite runner, not synchronization of
Trinity's complete chat history. No end-user identity, parent-run relationship,
cancellation or budget is automatically delegated across the two platforms.

## Recovery and security boundaries

Before the bridge first receives and durably saves an incarnation reference,
its saved run ID and input cannot prove that retention never disappeared. A
lost first response followed by deletion/recreation before the first successful
GET remains outside that proof. Protect run retention throughout this period;
never recreate a missing operation to obtain a reference. The reference is an
identity precondition, not a credential, permanent deduplication key or HTTP
ETag. See [the engineering contract](../../docs/internal/scoped-specialist-access.md).

Retain both caller receipts and Runkite runs for the entire retry window. Deleting
or pruning a run ends result recovery and the plane's underlying retry protection.
The bridge refuses a later observation when its retained run is absent. Its local
attempt marker prevents it from using MCP to recreate that operation, but this is
not a distributed transaction between local files, Runkite's store and its queue.
A crash, an ambiguous queue outcome or lost state may require operator
reconciliation. A retained pending run whose queue outcome is unknown stays
unresolved; observation does not repair or redispatch it. Do not erase attempt
markers, delete runs or allocate fresh IDs to bypass that uncertainty. External
tool effects still need their own idempotency strategy. Nothing here guarantees
exactly-once execution.

Keep a saved operation on the same target and tenant authority. The profile is
not proof of which tenant a credential authenticates. Key rotation must preserve
the same intended authority; do not reuse receipts with credentials for another
tenant or environment. Local state is trusted caller storage, not tamper-proof
evidence or a cross-agent authorization service.

Both Trinity's direct HTTP MCP configuration and this bridge protect outbound
destinations. The bridge normally requires public HTTPS, validates and pins the
resolved addresses, retains certificate verification and refuses redirects and
ambient proxy routing. Private, loopback and metadata destinations are rejected.
The local proof fixture enables a narrow literal-loopback HTTP exception only
for testing. Do not enable that exception in this template or weaken Trinity's
validator to make a deployment work.

The native agent can read its environment and run commands. Installing this
wrapper does not sandbox that agent or stop it using another client. Runkite
continues to enforce its configured authority on its own endpoints. Trinity
continues to govern the surrounding native execution.

## Qualify without paid model calls

Run the bridge's focused Go tests from the repository root:

```sh
go test ./examples/trinity_specialist_mcp
```

The real-plane fixture uses the HTTP API, SQLite, a loopback gRPC listener, the
existing Python echo runner and the built bridge plus launcher. This qualifies
the example's protocol boundary. It does not qualify deployment through the
`runkite serve` CLI, a live Trinity installation, a native model CLI or a hosted
provider. The command above runs the example's unit and HTTP boundary tests,
not that opt-in real-worker fixture.

To run the real-worker fixture, first prepare the existing dependencies. Use an
existing Python 3.11 or newer runner environment, or create one as follows:

```sh
python3.12 -m venv /tmp/runkite-trinity-runner-venv
/tmp/runkite-trinity-runner-venv/bin/python -m pip install -r python/requirements.txt
go mod download
```

Then run the qualification from the repository root:

```sh
python3.12 examples/trinity_specialist_mcp/qualify.py \
  --python /tmp/runkite-trinity-runner-venv/bin/python \
  --output /tmp/runkite-trinity-qualification.json
```

Choose a new output filename for each run. `--output` names a JSON file, not a
directory; existing receipts and log files are not overwritten. Dependency
preparation can access package registries. The qualification itself uses cached
Go modules, refuses toolchain downloads and makes no provider calls. It starts
only its own loopback fixture and child processes. Check the resulting receipt
and adjacent build/test logs before reporting a qualification result.

Separately qualify configuration against an explicitly supplied Trinity source
checkout pinned to
[`f70ebf6f231a981b032a965e3cc89a97f7ea6635`](https://github.com/abilityai/trinity/tree/f70ebf6f231a981b032a965e3cc89a97f7ea6635):

```sh
python3.12 examples/trinity_specialist_mcp/qualify_trinity_config.py \
  --trinity-checkout /absolute/path/to/trinity
```

This script requires Python 3.11 or newer, uses only the standard library and
does not fetch a checkout or dependencies. It refuses another revision or edits
to the three imported Trinity source files. It runs the actual validator,
credential-template renderer and Claude configuration writer against temporary
files. The literal upstream `/home/developer` path is redirected to the temporary
directory during that writer test. It verifies preservation of an existing MCP
entry, missing-credential withholding and rejection of unsafe configuration.
It makes no model calls or network requests and does not launch native CLIs.

Only the Claude configuration path is qualified here. At this pinned revision,
Gemini's additional-server adapter does not forward the configured `env` block,
and Gemini and Codex skip URL-only additional servers. Do not advertise runtime
parity from a valid JSON template. Actual native CLI launch, credential
propagation, MCP discovery and tool execution need separate verification before
recommending another runtime.
