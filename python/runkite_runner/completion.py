"""Keep the lease alive until the plane accepts a durable completion."""

from __future__ import annotations

import asyncio
import logging

from . import runner_pb2

logger = logging.getLogger("runkite.runner")


class CompletionDeliveryError(Exception):
    """Completion is unconfirmed. Do not replace it with a second status."""


async def finish_run(
    stub,
    run_id: str,
    generation: int,
    status: str,
    stream_task,
    auth_metadata: list,
    *,
    attempts: int = 4,
    retry_delay: float = 0.25,
    rpc_timeout: float = 5.0,
) -> str | None:
    """Called while the heartbeat is active; never re-executes the agent.

    An uncertain stream leaves the assignment recoverable. It must not
    rewrite the agent outcome or report success before output is confirmed.
    """
    error_message = "see error event" if status == "error" else ""
    try:
        response = await asyncio.wait_for(stream_task, timeout=30.0)
        if response is None or not response.ok:
            raise RuntimeError("plane did not acknowledge the event stream")
    except Exception as exc:
        raise CompletionDeliveryError(f"event delivery unconfirmed for {run_id}") from exc

    request = runner_pb2.ReportStatusRequest(
        run_id=run_id,
        status=status,
        error_message=error_message,
        generation=generation,
    )
    for attempt in range(attempts):
        try:
            response = await asyncio.wait_for(
                stub.ReportStatus(request, metadata=auth_metadata),
                timeout=rpc_timeout,
            )
            if response.superseded:
                return None
            if not response.ok:
                raise RuntimeError("plane did not accept completion")
            return status
        except Exception as exc:
            if attempt + 1 == attempts:
                raise CompletionDeliveryError(f"completion delivery failed for {run_id}") from exc
            logger.warning("Retrying completion for %s after %s", run_id, type(exc).__name__)
            await asyncio.sleep(retry_delay * 2**attempt)
    raise CompletionDeliveryError(f"completion delivery failed for {run_id}")
