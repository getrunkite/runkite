"""Pre-execution status check (Runner Protocol §10.3).

After GetJob dequeues a RunAssignment, the runner MUST ask the control
plane whether the run is still executable before starting agent work --
a cancel that landed between dequeue and execute is otherwise a silent
race. Fail-open on transport errors: WatchCancels remains the primary
cancel path; a brief CP blip must not drop every job.
"""

from __future__ import annotations

import logging

import httpx

from .tls_utils import httpx_tls_kwargs

logger = logging.getLogger("runkite.runner")

# Terminal + interrupted: MUST NOT execute (PROTOCOL §10.3).
_SKIP_STATUSES = frozenset({"interrupted", "success", "error", "timeout"})


async def should_skip_run(
    http_address: str,
    run_id: str,
    *,
    runner_kind: str = "",
    runner_token: str = "",
) -> bool:
    """Return True when the assignment must be discarded without executing."""
    base = http_address.rstrip("/")
    headers: dict[str, str] = {}
    if runner_token:
        headers["X-Runner-Kind"] = runner_kind or "python-langgraph"
        headers["X-Runner-Token"] = runner_token
    try:
        async with httpx.AsyncClient(timeout=5.0, **httpx_tls_kwargs()) as client:
            resp = await client.get(f"{base}/internal/runs/{run_id}/status", headers=headers)
        if resp.status_code == 404:
            logger.warning("pre-exec status: run %s not found; discarding", run_id)
            return True
        if resp.status_code != 200:
            logger.warning(
                "pre-exec status: run %s HTTP %s; proceeding (fail-open)",
                run_id,
                resp.status_code,
            )
            return False
        status = (resp.json() or {}).get("status", "")
        if status in _SKIP_STATUSES:
            logger.info("pre-exec status: run %s is %s; discarding assignment", run_id, status)
            return True
        return False
    except Exception as e:
        logger.warning("pre-exec status: run %s check failed (%s); proceeding (fail-open)", run_id, e)
        return False
