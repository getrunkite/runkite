"""Generic, framework-agnostic Runner Protocol worker loop.

Extracted from worker.py's LangGraph-specific loop to prove the master
plan's own claim: "a runner in any other language [or framework] is
valid the moment it implements the Runner Protocol." This module
contains only the gRPC polling/streaming/status-reporting mechanics --
zero LangGraph imports anywhere in this file -- parameterized by a
FrameworkAdapter (below) that does the actual, framework-specific work
of executing one run. python/adapters/*/adapter.py (CrewAI, LlamaIndex,
plain LangChain) all sit on top of this same loop.

worker.py (the production LangGraph runner) does NOT use this module --
it predates it, is heavily tested/production-proven as-is, and has
LangGraph-specific platform features (Factory Graphs, checkpoint/store
dual mode, custom routes) this generic loop deliberately omits. Porting
it onto this shared loop is a reasonable follow-up, not required to
prove genericness, which is this module's only job.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
import logging
import os
import time
from collections.abc import Awaitable, Callable
from typing import Any, Protocol

import grpc

from . import runner_pb2, runner_pb2_grpc
from .heartbeat import heartbeat_loop
from .run_status import should_skip_run
from .tls_utils import grpc_channel_credentials
from .tracing import init as init_tracing
from .tracing import run_span, set_run_status

logger = logging.getLogger("runkite.runner")

EventCallback = Callable[[dict], Awaitable[None]]


class RunCancelled(Exception):
    """Raised by run_cancellable when cancel_event fires before the
    underlying coroutine completes. Adapters should catch this
    specifically (not just Exception) and emit an "interrupted" end
    event, not "error" -- a cancelled run is not a failure."""


async def run_cancellable(coro, cancel_event: asyncio.Event | None):
    """Runs `coro` to completion, racing it against cancel_event.

    The three thin framework adapters (CrewAI/LlamaIndex/plain
    LangChain) each make a single blocking call into their framework
    (crew.akickoff/engine.achat/runnable.ainvoke) with no natural
    per-step loop to check cancellation inside, unlike worker.py's
    LangGraph runner which checks cancel_event on every streamed chunk.
    This is the equivalent for a single-call adapter: race the call
    against cancel_event.wait(), and if cancellation wins, call
    task.cancel() (best-effort -- not every framework call honors
    asyncio cancellation cooperatively at every point, but this at
    least unblocks the caller immediately rather than waiting for the
    framework call to finish on its own) and raise RunCancelled instead
    of returning the framework's result.
    """
    task = asyncio.ensure_future(coro)
    if cancel_event is None:
        return await task

    cancel_wait = asyncio.ensure_future(cancel_event.wait())
    try:
        done, _ = await asyncio.wait({task, cancel_wait}, return_when=asyncio.FIRST_COMPLETED)
        if task in done:
            return task.result()
        task.cancel()
        with contextlib.suppress(BaseException):
            await task
        raise RunCancelled()
    finally:
        if not cancel_wait.done():
            cancel_wait.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await cancel_wait


class FrameworkAdapter(Protocol):
    """What generic_worker needs from a framework-specific adapter -- an
    adapter shim that translates between the framework's own execution API
    and RunAssignment/RunEvent.

    Optional opaque checkpoints (non-LangGraph multi-turn): set
    ``checkpoint_framework`` to a non-empty string (e.g. ``"llamaindex"``)
    and implement ``prepare_checkpoint_input`` / ``serialize_checkpoint``.
    When RUNKITE_HTTP_URL (or the worker http address) is set, generic_worker
    loads GET .../adapter-state before execute and PUTs that same id after
    success/interrupted (never .../latest — that path is lexicographic).
    """

    async def load_config(self, config_path: str) -> None:
        """Load agent definitions from langgraph.json (or equivalent)."""
        ...

    async def execute(self, assignment: dict, event_callback: EventCallback, cancel_event: asyncio.Event) -> str:
        """Run one assignment, emit RunEvents via event_callback, return
        the final status: 'success', 'error', or 'interrupted'. Adapters
        are not required to emit anything before returning other than
        (eventually) an "end"/"error" event -- generic_worker emits a
        fallback "error" event itself if execute() raises instead of
        returning a status, so a buggy adapter can't leave a run stuck
        "running" forever from the control plane's point of view."""
        ...


def make_event_factory(run_id: str) -> Callable[..., dict]:
    """Returns a make_event(method, data, namespace=None) closure with
    per-run sequential numbering -- identical RunEvent shape
    (event_id/seq/method/namespace/data/ts) to worker.py's LangGraph
    runner, so the control plane, Admin UI, and any client treat events
    from any framework identically, regardless of which adapter produced
    them."""
    seq = 0

    def _serialize(obj: Any) -> Any:
        if obj is None or isinstance(obj, (str, int, float, bool)):
            return obj
        if isinstance(obj, dict):
            return {k: _serialize(v) for k, v in obj.items()}
        if isinstance(obj, (list, tuple)):
            return [_serialize(item) for item in obj]
        if hasattr(obj, "model_dump"):  # pydantic v2 (langchain-core messages, etc.)
            return obj.model_dump()
        if hasattr(obj, "dict"):  # pydantic v1
            return obj.dict()
        return str(obj)

    def make_event(method: str, data: Any, namespace: list | None = None) -> dict:
        nonlocal seq
        seq += 1
        return {
            "event_id": f"{run_id}_evt_{seq}",
            "seq": seq,
            "method": method,
            "namespace": namespace or [],
            "data": _serialize(data),
            "ts": int(time.time() * 1000),
        }

    return make_event


async def run_worker(
    adapter: FrameworkAdapter,
    config_path: str,
    grpc_address: str,
    runner_kind: str,
    concurrency: int = 1,
    http_address: str | None = None,
) -> None:
    """Main worker loop: poll for jobs, execute via `adapter`, stream
    events back. Mirrors worker.py's run_worker, minus LangGraph-specific
    checkpoint/store/custom-app wiring -- those are LangGraph Platform
    concerns, not something the Runner Protocol itself requires.

    concurrency controls how many jobs this process handles at once (see
    _poll_loop's dispatcher below) -- default 1 preserves the original
    one-job-at-a-time behavior exactly."""
    tracing_shutdown = init_tracing()

    await adapter.load_config(config_path)

    runner_token = os.environ.get("RUNNER_TOKEN", "")
    auth_metadata = [("runner-kind", runner_kind), ("runner-token", runner_token)] if runner_token else []
    http_address = http_address or os.environ.get("RUNKITE_HTTP_URL", "http://localhost:2026")

    # Keepalive so the control plane detects a dead/crashed runner
    # quickly -- see worker.py's run_worker for the full rationale
    # (matches cmd/serve.go's keepalive.ServerParameters).
    grpc_options = [
        ("grpc.keepalive_time_ms", 2000),
        ("grpc.keepalive_timeout_ms", 2000),
        ("grpc.keepalive_permit_without_calls", 1),
    ]
    # TLS opt-in via RUNKITE_TLS_CA_FILE -- see tls_utils and worker.py's
    # own run_worker for the identical rationale.
    tls_creds = grpc_channel_credentials()
    if tls_creds is not None:
        channel = grpc.aio.secure_channel(grpc_address, tls_creds, options=grpc_options)
    else:
        channel = grpc.aio.insecure_channel(grpc_address, options=grpc_options)
    stub = runner_pb2_grpc.RunnerServiceStub(channel)
    logger.info(f"Worker ready. Polling for jobs as runner_kind={runner_kind}")

    # Same cancel race the TypeScript runner already fixed: a WatchCancels
    # signal can arrive BEFORE this poll loop has registered the run's
    # Event (GetJob -> parse -> register). Dropping that signal means a
    # cancel that races job dispatch is silently ignored. pre_cancelled
    # remembers early signals until register_run claims them; orphans
    # expire so a cancel for a never-seen run_id can't leak forever.
    pending_cancels: dict[str, asyncio.Event] = {}
    pre_cancelled: set[str] = set()
    pending_cancels_lock = asyncio.Lock()

    async def _expire_pre_cancel(run_id: str):
        await asyncio.sleep(60)
        async with pending_cancels_lock:
            pre_cancelled.discard(run_id)

    async def watch_cancels():
        while True:
            try:
                stream = stub.WatchCancels(
                    runner_pb2.WatchCancelsRequest(runner_kind=runner_kind), metadata=auth_metadata
                )
                async for signal in stream:
                    run_id = signal.run_id
                    logger.info(f"Cancel signal received via gRPC for run {run_id}")
                    async with pending_cancels_lock:
                        ev = pending_cancels.get(run_id)
                        if ev is not None:
                            ev.set()
                        else:
                            pre_cancelled.add(run_id)
                            asyncio.create_task(_expire_pre_cancel(run_id))
            except grpc.aio.AioRpcError as e:
                logger.error(f"WatchCancels error: {e.code()} {e.details()}")
                await asyncio.sleep(1)
            except Exception as e:
                logger.exception(f"WatchCancels error: {e}")
                await asyncio.sleep(1)

    cancel_watcher_task = asyncio.create_task(watch_cancels())

    # See worker.py's run_worker for why this needs to survive
    # _poll_loop's own cancellation: spawned job tasks are independent
    # of the dispatcher loop that created them.
    in_flight_tasks: set[asyncio.Task] = set()

    try:
        await _poll_loop(
            stub,
            adapter,
            runner_kind,
            auth_metadata,
            pending_cancels,
            pre_cancelled,
            pending_cancels_lock,
            concurrency=concurrency,
            in_flight=in_flight_tasks,
            http_address=http_address,
            runner_token=runner_token,
        )
    finally:
        cancel_watcher_task.cancel()
        if in_flight_tasks:
            logger.info(f"Draining {len(in_flight_tasks)} in-flight job(s) before shutdown...")
            await asyncio.gather(*in_flight_tasks, return_exceptions=True)
        tracing_shutdown()


async def register_run(
    pending_cancels: dict[str, asyncio.Event],
    pre_cancelled: set[str],
    lock: asyncio.Lock,
    run_id: str,
) -> asyncio.Event:
    """Claim (or create) the cancel Event for run_id. If a cancel signal
    already arrived (pre_cancelled), the Event is returned already set."""
    async with lock:
        ev = pending_cancels.get(run_id)
        if ev is None:
            ev = asyncio.Event()
            pending_cancels[run_id] = ev
        if run_id in pre_cancelled:
            pre_cancelled.discard(run_id)
            ev.set()
        return ev


def _log_trace_context(run_id: str, tc: dict | None) -> None:
    """Log W3C / correlation fields from RunAssignment.trace_context.
    Span activation (when OTEL_* is configured) is handled by run_span.
    """
    tc = tc or {}
    parts = [f"run_id={run_id}"]
    for key in ("correlation_id", "traceparent", "tracestate"):
        if val := tc.get(key):
            parts.append(f"{key}={val}")
    if len(parts) > 1:
        logger.info("trace " + " ".join(parts))


async def _handle_job(
    stub,
    adapter: FrameworkAdapter,
    response,
    auth_metadata: list,
    pending_cancels: dict,
    pre_cancelled: set,
    pending_cancels_lock: asyncio.Lock,
    http_address: str = "http://localhost:2026",
    runner_kind: str = "",
    runner_token: str = "",
) -> None:
    """Execute one dispatched job end-to-end. Split out of _poll_loop so
    the dispatcher can run one of these per concurrency slot as an
    independent asyncio.Task -- see worker.py's _handle_job for the full
    rationale (identical structure, generalized to call
    adapter.execute(...) instead of the LangGraph-specific
    execute_run(...))."""
    from .checkpoint import resolve_checkpoint_http_url
    from .opaque_checkpoint import OpaqueCheckpointClient, adapter_supports_checkpoints
    from .tenant_ctx import bind_run, bind_tenant, reset_run, reset_tenant

    run_id = None
    stream_task = None
    status = "error"
    # Fencing token -- see heartbeat.py's docstring. Initialized here
    # so the outer except's own ReportStatus call always has a defined
    # value even if json.loads/assignment["run_id"]
    # itself raised first.
    generation = 0
    try:
        assignment = json.loads(response.assignment_json)
        run_id = assignment["run_id"]
        # Defaults to 0 (unfenced) for a control plane that predates
        # this field, same convention as the Go side.
        generation = assignment.get("generation", 0)
        logger.info(f"Got job: run_id={run_id} graph_id={assignment['graph_id']}")
        _log_trace_context(run_id, assignment.get("trace_context"))

        if await should_skip_run(
            http_address,
            run_id,
            runner_kind=runner_kind,
            runner_token=runner_token,
            tenant_id=assignment.get("tenant_id") or "",
        ):
            return

        tenant_token = bind_tenant(assignment.get("tenant_id"))
        run_token = bind_run(run_id, generation)
        try:
            with run_span(
                run_id,
                graph_id=assignment.get("graph_id", ""),
                thread_id=assignment.get("thread_id", ""),
                tenant_id=assignment.get("tenant_id") or "",
                trace_context=assignment.get("trace_context"),
            ) as span:
                event_queue: asyncio.Queue = asyncio.Queue()

                async def event_generator():
                    while True:
                        item = await event_queue.get()
                        if item is None:
                            return
                        yield item

                async def send_event(event: dict):
                    await event_queue.put(
                        runner_pb2.RunEventProto(run_id=run_id, event_json=json.dumps(event), generation=generation)
                    )

                cancel_event = await register_run(
                    pending_cancels,
                    pre_cancelled,
                    pending_cancels_lock,
                    run_id,
                )

                # Started as soon as run_id is known, before StreamEvents' first
                # message -- see worker.py's mirrored change / heartbeat.py.
                # Shares cancel_event with adapter.execute below -- a superseded
                # heartbeat sets the SAME event a real cancel signal would.
                heartbeat_task = asyncio.create_task(
                    heartbeat_loop(stub, run_id, auth_metadata, generation=generation, cancel_event=cancel_event)
                )

                stream_task = None
                status = "error"
                try:
                    stream_call = stub.StreamEvents(event_generator(), metadata=auth_metadata)
                    stream_task = asyncio.ensure_future(stream_call)

                    try:
                        # Non-LangGraph frameworks have no checkpoint_id
                        # resume path. Fail loudly rather than silently
                        # ignoring checkpoint_ref (the audit finding that
                        # previously bit the LangGraph runners).
                        cref = assignment.get("checkpoint_ref")
                        if isinstance(cref, str) and cref.strip():
                            make_event = make_event_factory(run_id)
                            await send_event(
                                make_event(
                                    "error",
                                    {
                                        "message": (
                                            "checkpoint_ref is only supported by LangGraph runners "
                                            "(python-langgraph / typescript-langgraphjs); "
                                            f"this runner_kind ({runner_kind or 'generic'}) cannot time-travel"
                                        ),
                                        "type": "CheckpointRefUnsupported",
                                    },
                                )
                            )
                            status = "error"
                        else:
                            status = await _execute_with_opaque_checkpoint(
                                adapter,
                                assignment,
                                send_event,
                                cancel_event,
                                http_address=http_address,
                                runner_kind=runner_kind,
                                runner_token=runner_token,
                                resolve_http_url=resolve_checkpoint_http_url,
                                OpaqueCheckpointClient=OpaqueCheckpointClient,
                                adapter_supports_checkpoints=adapter_supports_checkpoints,
                            )
                    except Exception as e:
                        # A misbehaving adapter that raises instead of returning
                        # a status must not leave the run stuck "running"
                        # forever from the control plane's perspective -- see
                        # FrameworkAdapter.execute's doc comment.
                        logger.exception(f"Run {run_id} failed in adapter.execute: {e}")
                        make_event = make_event_factory(run_id)
                        await send_event(make_event("error", {"message": str(e), "type": type(e).__name__}))
                        status = "error"
                finally:
                    heartbeat_task.cancel()
                    with contextlib.suppress(asyncio.CancelledError):
                        await heartbeat_task
                    # Always clear the cancel registration, even on an
                    # unexpected failure above -- otherwise it leaks an entry in
                    # pending_cancels for every job that hits this path.
                    async with pending_cancels_lock:
                        pending_cancels.pop(run_id, None)

                await event_queue.put(None)
                if stream_task is not None:
                    try:
                        await stream_task
                    except Exception as e:
                        logger.error(f"Stream finalization error: {e}")

                # Always report once run_id is known -- StreamEvents setup
                # failures used to skip this and leave the run "running" forever.
                await stub.ReportStatus(
                    runner_pb2.ReportStatusRequest(
                        run_id=run_id,
                        status=status,
                        error_message="" if status != "error" else "see error event",
                        generation=generation,
                    ),
                    metadata=auth_metadata,
                )

                set_run_status(span, status)
                logger.info(f"Run completed: run_id={run_id} status={status}")
        finally:
            reset_run(run_token)
            reset_tenant(tenant_token)

    except grpc.aio.AioRpcError as e:
        logger.error(f"gRPC error handling run_id={run_id}: {e.code()} {e.details()}")
    except Exception as e:
        logger.exception(f"Worker error handling run_id={run_id}: {e}")
        if run_id is not None:
            try:
                await stub.ReportStatus(
                    runner_pb2.ReportStatusRequest(
                        run_id=run_id,
                        status="error",
                        error_message=str(e),
                        generation=generation,
                    ),
                    metadata=auth_metadata,
                )
            except Exception:
                pass


async def _execute_with_opaque_checkpoint(
    adapter: FrameworkAdapter,
    assignment: dict,
    send_event: EventCallback,
    cancel_event: asyncio.Event,
    *,
    http_address: str,
    runner_kind: str,
    runner_token: str,
    resolve_http_url,
    OpaqueCheckpointClient,
    adapter_supports_checkpoints,
) -> str:
    """Load/save opaque checkpoints around adapter.execute when opted in."""
    use_cp = adapter_supports_checkpoints(adapter)
    http_base = resolve_http_url(http_address) if use_cp else None
    client = None
    last_values: dict = {}

    async def capturing_send(event: dict):
        if event.get("method") == "values" and isinstance(event.get("data"), dict):
            last_values.clear()
            last_values.update(event["data"])
        await send_event(event)

    if use_cp and http_base:
        framework = str(adapter.checkpoint_framework).strip()
        client = OpaqueCheckpointClient(
            http_base_url=http_base,
            framework=framework,
            runner_kind=runner_kind,
            runner_token=runner_token or None,
        )
        thread_id = assignment.get("thread_id") or ""
        # Work on a shallow copy so callers still see the raw client input
        # on the original assignment dict after this returns.
        assignment = {**assignment}
        prior = None
        try:
            # Load the fixed adapter-state id — never GET .../latest
            # (checkpoint_id DESC can return a LangGraph UUID blob).
            prior = await client.get(thread_id) if thread_id else None
            prepare = getattr(adapter, "prepare_checkpoint_input", None)
            if callable(prepare):
                assignment["input"] = prepare(prior, assignment.get("input") or {})
        except Exception:
            logger.exception("opaque checkpoint load/prepare failed; continuing without prior state")
            # Leave assignment["input"] as the raw client payload.

    status = "error"
    try:
        status = await adapter.execute(assignment, capturing_send if client else send_event, cancel_event)
    finally:
        if client is not None:
            try:
                if status in ("success", "interrupted"):
                    serialize = getattr(adapter, "serialize_checkpoint", None)
                    if callable(serialize):
                        blob = serialize(assignment, last_values, status)
                        if blob:
                            await client.put(assignment.get("thread_id") or "", blob)
            except Exception:
                logger.exception("opaque checkpoint save failed")
            await client.aclose()

    return status


async def _poll_loop(
    stub,
    adapter: FrameworkAdapter,
    runner_kind: str,
    auth_metadata: list,
    pending_cancels: dict,
    pre_cancelled: set,
    pending_cancels_lock: asyncio.Lock,
    concurrency: int = 1,
    in_flight: set[asyncio.Task] | None = None,
    http_address: str = "http://localhost:2026",
    runner_token: str = "",
) -> None:
    """Semaphore-bounded dispatcher: long-polls for jobs and hands each
    one to its own _handle_job task, up to `concurrency` running at
    once. See worker.py's _poll_loop for the full rationale (identical
    structure). concurrency=1 (the default) preserves the exact
    original one-job-at-a-time behavior."""
    if concurrency < 1:
        raise ValueError(f"concurrency must be >= 1, got {concurrency}")
    if in_flight is None:
        in_flight = set()
    sem = asyncio.Semaphore(concurrency)

    def _on_job_done(task: asyncio.Task):
        in_flight.discard(task)
        sem.release()

    while True:
        await sem.acquire()
        try:
            response = await stub.GetJob(
                runner_pb2.GetJobRequest(
                    runner_kind=runner_kind,
                    timeout_seconds=30,
                ),
                metadata=auth_metadata,
            )
        except asyncio.CancelledError:
            sem.release()
            raise
        except grpc.aio.AioRpcError as e:
            logger.error(f"gRPC error: {e.code()} {e.details()}")
            sem.release()
            await asyncio.sleep(1)
            continue
        except Exception as e:
            logger.exception(f"Worker error: {e}")
            sem.release()
            await asyncio.sleep(1)
            continue

        if not response.has_job:
            sem.release()
            continue

        task = asyncio.create_task(
            _handle_job(
                stub,
                adapter,
                response,
                auth_metadata,
                pending_cancels,
                pre_cancelled,
                pending_cancels_lock,
                http_address=http_address,
                runner_kind=runner_kind,
                runner_token=runner_token,
            )
        )
        in_flight.add(task)
        task.add_done_callback(_on_job_done)
