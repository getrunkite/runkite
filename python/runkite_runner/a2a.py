"""Agent-to-Agent (A2A) delegation client (master plan: "Agent-to-agent
(A2A): agent calls agent via the same Agent Protocol API").

`call_agent` is what a running agent's own node code calls to invoke
another agent as a sub-task -- it POSTs to the control plane's
`/internal/a2a/runs` endpoint (see internal/api/a2a.go for the full
server-side design: auth propagation, recursion limits, cost
attribution via root_run_id).

Deliberately takes the graph node's own `config` dict, not separate
run_id/user parameters -- every value this needs (the current run_id to
set as parent_run_id, the authenticated user to forward as
on_behalf_of) is already there, set by `build_run_config` in worker.py
for every run. A node function just calls
``await call_agent(config, "other_agent", {"messages": [...]})``.

Operational note: with ``wait=True`` (the default), the parent run's
worker slot stays occupied until the child finishes. A runner process
with ``concurrency=1`` therefore deadlocks on nested A2A -- the child
job cannot be dequeued until the parent frees its slot. Use
concurrency >= 2 (or ``wait=False`` + poll) for any graph that
delegates synchronously.
"""

from __future__ import annotations

import os
from typing import Any

import httpx


class A2AError(Exception):
    """Raised when the control plane rejects a delegation call --
    e.g. HTTP 400 for a recursion depth limit exceeded (see
    ErrA2ADepthExceeded in internal/api/server.go), or 404 for an
    unknown parent_run_id."""


async def call_agent(
    config: dict,
    agent_id: str,
    input: dict[str, Any],
    *,
    wait: bool = True,
    thread_id: str | None = None,
    run_config: dict[str, Any] | None = None,
    control_plane_url: str | None = None,
    timeout: float | None = None,
) -> dict[str, Any]:
    """Invoke another agent as a sub-task from within a running agent's
    own node code.

    Args:
        config: the RunnableConfig LangGraph passes to every node --
            must be the same `config` the calling node itself received
            (or its unmodified `configurable` sub-dict), so run_id/user
            can be forwarded correctly.
        agent_id: which agent to delegate to.
        input: the sub-agent's input, same shape as a normal run's input.
        wait: block until the sub-agent's run reaches a terminal status
            and return its result (default). False fires the sub-run
            and returns immediately with just the created run's data --
            the caller is then responsible for polling/streaming it
            itself, same as any other async run.
        thread_id: an existing thread to run on, e.g. to continue a
            specific sub-conversation. Defaults to a fresh thread per
            call -- most delegation calls are one-shot sub-tasks, not
            multi-turn conversations with the sub-agent.
        run_config: passed through as the sub-run's own `config`
            (distinct from this function's own `config` argument, which
            is the CALLER's LangGraph RunnableConfig, not the sub-run's).
        control_plane_url: defaults to RUNKITE_HTTP_URL, same env var
            convention as every other control-plane HTTP call in this
            runner.
        timeout: httpx request timeout in seconds. None (default) means
            no timeout -- a `wait=True` call blocks for however long the
            sub-agent actually takes, which the caller controls via its
            own input, not an arbitrary client-side cutoff.

    Returns:
        When wait=True: {"run": {...}, "values": {...}} -- the sub-run's
        final state and output, same shape as the client-facing
        `/runs/{id}/wait` response.
        When wait=False: just the created run object, status "pending".

    Raises:
        A2AError: the control plane rejected the call (e.g. recursion
            depth exceeded, unknown parent_run_id).
        RuntimeError: config has no run_id -- this wasn't called from
            within an actual graph node's execution (or with a config
            that build_run_config never touched).
    """
    configurable = config.get("configurable", {}) if config else {}
    parent_run_id = configurable.get("run_id")
    if not parent_run_id:
        raise RuntimeError(
            "call_agent: config has no configurable.run_id -- must be called "
            "with the RunnableConfig a graph node itself received, not a "
            "hand-built or empty one"
        )

    body: dict[str, Any] = {
        "agent_id": agent_id,
        "input": input,
        "parent_run_id": parent_run_id,
        "wait": wait,
    }
    if thread_id:
        body["thread_id"] = thread_id
    if run_config:
        body["config"] = run_config

    user = configurable.get("langgraph_auth_user")
    if user is not None and hasattr(user, "to_dict"):
        on_behalf_of = user.to_dict()
        if on_behalf_of:
            body["on_behalf_of"] = on_behalf_of

    base_url = control_plane_url or os.environ.get("RUNKITE_HTTP_URL", "http://localhost:2026")
    headers: dict[str, str] = {}
    runner_token = os.environ.get("RUNNER_TOKEN")
    if runner_token:
        headers["X-Runner-Kind"] = "python-langgraph"
        headers["X-Runner-Token"] = runner_token

    async with httpx.AsyncClient(timeout=timeout) as client:
        resp = await client.post(f"{base_url}/internal/a2a/runs", json=body, headers=headers)
        if resp.status_code >= 400:
            raise A2AError(f"call_agent({agent_id!r}) failed: {resp.status_code} {resp.text}")
        return resp.json()
