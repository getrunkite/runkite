# External isolated workload prototype

Status: experimental, available in published runners since v0.7.0.
OpenShell remains an external product. The
JSONL bridge and fleet pools are separate controls. Neither a fake-runtime unit
test nor Helm rendering establishes an enforced sandbox.

The v0.7.0 disposable qualification passed using an installed Python runner, real
SQLite plane, pooled worker and external OpenShell 0.1.8 with Landlock
`hard_requirement`. This covers the explicit fixture workload, parent restart,
receipt reclaim, uncertain-intent refusal, cancellation and confirmed cleanup.
It does not qualify unchanged SDKs or live providers. The separate Helm egress
profile passed on a disposable kind/default-kindnet cluster. Read the
[qualification record and exact limits](../../docs/internal/constrained-execution.md#verification-and-remaining-qualification).

Build this offline fixture with your container engine and record its immutable
image ID or repository digest. Its Dockerfile contains a deliberately readable
synthetic file outside the policy. Confirm ordinary container execution can read
that file before using it as a negative confinement probe.

The trusted host needs the pinned OpenShell 0.1.8 CLI, an operator-managed
gateway and the normal Runkite runner dependencies. Do not run the Runkite worker
inside the workload image. Do not mount its environment, database credentials,
CLI credential directory or container socket into the workload.

Configure the control plane and trusted worker with the same agent file:

```json
{
  "runner_kind": "python-isolated",
  "execution_pool": "isolated",
  "execution_pools": {
    "isolated": {
      "runner_kind": "python-isolated",
      "tenants": ["default"],
      "agents": ["isolated_echo"]
    }
  },
  "graphs": {"isolated_echo": "workload.py:main"},
  "isolated_execution": {
    "provider": "openshell",
    "cli": "/absolute/path/to/openshell",
    "gateway_endpoint": "https://your-runtime.example",
    "workspace": "default",
    "image": "registry.example/isolated-workload@sha256:REPLACE_WITH_ACTUAL_DIGEST",
    "policy_file": "policy.yaml",
    "command": ["python3", "/app/workload.py"]
  }
}
```

The placeholder image is not executable configuration. Use a real digest or a
local immutable `sha256:<image-id>` supported by your runtime. Configure distinct
`RUNNER_TOKEN_POOL_ISOLATED` on the plane and its matching `RUNNER_TOKEN` only on
the trusted worker. Start the worker with:

```sh
python -m runkite_runner.isolated --config /path/to/langgraph.json --execution-pool isolated
```

The graph reference identifies the routed graph; the trusted worker does not
import that file. The image owns its workload implementation. This example uses
no model or paid provider. An SDK loop can speak the same bridge, but existing
SDK adapters are not automatically compatible with it. Optional `providers`
lists existing OpenShell provider names. Attach credentials through the chosen
external issuer; the bridge does not upload or copy a credential database.

The bridge sends only `input`, prior opaque `state` and safe run identifiers.
It excludes assignment configuration, user claims, manifests and environment.
The workload emits bounded JSON lines: incremental `event` frames, optional
`tool_call` frames, and one `result` with status/state/output. Tool requests go
through the parent's existing run-bound MCP proxy. Deny and pending responses
remain denies or holds; no automatic approval or polling is added.

Published v0.7.0 has no supported workload usage-reporting contract. v0.8.0
introduced [versioned cumulative reports](../../docs/internal/isolated-usage.md)
with durable acknowledgement and receipt/cancellation settlement. This offline
example reports an explicit zero; `usage_fixture` values are synthetic test
measurements only. A different workload must report its own measurements and
wait for `usage_ack`. Unknown and partial usage stay marked as such. Claims are
not provider-verified billing or a hard spend cap against a hostile workload.

State and incarnation-bound receipts use the plane's SQL opaque checkpoint CAS.
A committed result can replay after delivery failure without starting the
workload again. A retained launch intent without a committed result is uncertain:
reclaim refuses to launch again. Investigate effects and the named sandbox,
confirm runtime cleanup, then use a fresh thread for an explicitly reconciled
operation. There is no automatic force-clear or tool retry. Receipts are bounded
to 128 turns and the checkpoint/frame size limit may require a new thread sooner.

Cleanup reports whether the owned runtime record disappeared after a delete
request. It is separate from the agent result and cannot undo external effects.
Cancellation stops the owned sandbox when the runtime accepts and completes the
cleanup; an unavailable runtime can leave cleanup unconfirmed. There is no claim
of provider revocation, signed attestation or global instant cancellation.

Use `scripts/qualify-isolated.py` only against a disposable, explicitly supplied
loopback OpenShell gateway and the built fixture image. Read the qualification
results and remaining release blockers in
[the engineering note](../../docs/internal/constrained-execution.md).

<a id="optional-effective-policy-qualification-unreleased"></a>

## Optional effective-policy qualification (v0.10.0 checkout)

The checkout qualifier accepts `--prover /absolute/path/to/openshell-prover`
with the checksum-verified official OpenShell 0.1.8 CLI and checker. It creates a
fresh owned sandbox, observes matching current/loaded policy before workload
exec and after probes, and compares the captured configuration with the original
boundary under the pinned checker's model. Unsupported inputs, composition or
observed drift fail. Without the flag, the baseline reports effective-policy
checking as `not_requested` even when its other controls pass.

Use the [disposable TLS fixture commands](../../docs/internal/openshell-effective-policy.md#reproduce-on-a-disposable-local-engine)
and inspect policy, TLS and cleanup summaries together. The recorded local
macOS result used in-memory checkpoint/usage fixtures and a provider-free
workload. It passed both created incarnations and four separate real checker
controls. Inspect the separate Linux workflow's actual current result for that
environment. No blocked URL was supplied, so no new runtime-network reachability
claim follows.
This adds no production worker gate, atomic exec fence, continuous enforcement,
general provider-state attestation or independent certification. Keep this
profile experimental and repeat runtime probes on the intended deployment.
