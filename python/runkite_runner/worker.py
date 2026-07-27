"""Runner worker that connects to the Runkite control plane via gRPC,
dequeues RunAssignments, executes LangGraph agents, and streams
RunEvents back.

Usage:
    python -m runkite_runner.worker --config langgraph.json --grpc-address localhost:50051
"""

import asyncio
import contextlib
import importlib
import importlib.util
import inspect
import json
import logging
import os
import sys
import time
from pathlib import Path
from typing import Any

import grpc

# Generated protobuf stubs
from . import runner_pb2
from . import runner_pb2_grpc
from .checkpoint import CheckpointerManager
from .custom_app import load_asgi_app, serve_custom_app
from .factory_graph import FactoryGraph, RunFactoryContext, RunnerUser, classify_graph_export
from .store import RunkiteStore

logger = logging.getLogger("runkite.runner")


class LangGraphAdapter:
    """Loads and executes a LangGraph graph from a config file.

    A loaded graph_id is one of two kinds (see factory_graph.py for the
    full rationale -- this implements LangGraph's own documented
    factory-graph/ServerRuntime convention, needed for real-world
    LangGraph agents that rely on it via LangGraph Platform or any
    LangGraph SDK-compatible server):

    - static (the original, only supported form): a compiled graph,
      shared across every concurrent run. Stored in `self.graphs`.
    - factory (`self.factories`): a callable built fresh PER RUN instead,
      for agents that need request-isolated state (e.g. per-user
      middleware instances) -- resolved in execute_run, not here.
    """

    def __init__(self):
        self.graphs: dict[str, Any] = {}
        self.factories: dict[str, FactoryGraph] = {}
        self._checkpointer_manager: CheckpointerManager | None = None
        self._store: RunkiteStore | None = None

    async def load_config(self, config_path: str):
        """Load graph definitions from langgraph.json."""
        self.config_path = Path(config_path)
        with open(self.config_path) as f:
            config = json.load(f)

        # Add dependency paths to sys.path
        config_dir = self.config_path.parent
        for dep in config.get("dependencies", []):
            dep_path = (config_dir / dep).resolve()
            if str(dep_path) not in sys.path:
                sys.path.insert(0, str(dep_path))
                logger.info(f"Added to sys.path: {dep_path}")

        # Load graph modules
        graphs_config = config.get("graphs", {})
        for graph_id, graph_path in graphs_config.items():
            file_path, export_name = graph_path.split(":", 1)
            abs_path = (config_dir / file_path).resolve()

            spec = importlib.util.spec_from_file_location(f"runkite_graph.{graph_id}", str(abs_path))
            if spec is None or spec.loader is None:
                raise ValueError(f"Cannot load graph module: {abs_path}")

            module = importlib.util.module_from_spec(spec)
            sys.modules[spec.name] = module
            spec.loader.exec_module(module)

            export = getattr(module, export_name)
            kind, resolved = await classify_graph_export(export)

            if kind == "factory":
                self.factories[graph_id] = resolved
                logger.info(f"Loaded factory graph: {graph_id} from {abs_path} (params: {resolved.param_names})")
            else:
                self.graphs[graph_id] = resolved
                logger.info(f"Loaded graph: {graph_id} from {abs_path}")

    def is_factory(self, graph_id: str) -> bool:
        return graph_id in self.factories

    def get_graph(self, graph_id: str):
        """Return the compiled graph for a STATIC graph_id. Raises for a
        factory graph_id -- those are only resolvable per-run, with a
        run's own config/runtime context, via build_factory_graph."""
        if graph_id in self.graphs:
            return self.graphs[graph_id]
        if graph_id in self.factories:
            raise ValueError(
                f"'{graph_id}' is a factory graph -- build it per-run via build_factory_graph, not get_graph."
            )
        raise ValueError(f"Graph not found: {graph_id}. Available: {self.all_graph_ids()}")

    def all_graph_ids(self) -> list[str]:
        return [*self.graphs.keys(), *self.factories.keys()]

    async def build_factory_graph(self, graph_id: str, config: dict, run_context: "RunFactoryContext"):
        """Resolve a factory graph_id for one run. Returns an async
        context manager yielding the compiled graph, with the runner's
        checkpointer/store already attached -- callers just do
        `async with adapter.build_factory_graph(...) as graph:`.
        """
        factory = self.factories[graph_id]
        return factory.build(
            config,
            run_context,
            checkpointer_manager=self._checkpointer_manager,
            store=self._store,
        )

    def attach_checkpointer(self, manager: CheckpointerManager):
        """Override every loaded STATIC graph's checkpointer with the
        runner's shared one, and remember it so factory graphs get the
        same treatment per-instance in build_factory_graph. Checkpoint
        mode is a runner concern (see checkpoint.py), not something
        agent authors configure in their own graph.py -- whatever a
        graph compiled with (MemorySaver, None, etc.) is replaced here."""
        self._checkpointer_manager = manager
        for graph_id, graph in self.graphs.items():
            manager.attach(graph)
            logger.info(f"Attached {manager.mode} checkpointer to graph: {graph_id}")

    def attach_store(self, store: RunkiteStore):
        """Attach the runner's Store Dual Mode client (see store.py) to
        every loaded STATIC graph, the same way attach_checkpointer
        works, and remember it for factory graphs. Any node whose
        signature requests a `store` param (LangGraph's standard
        BaseStore injection) gets this transparently -- no agent-code
        changes needed."""
        self._store = store
        for graph_id, graph in self.graphs.items():
            graph.store = store
            logger.info(f"Attached {store.mode} store to graph: {graph_id}")


def build_run_config(assignment: dict) -> dict:
    """Build the RunnableConfig passed to graph.astream(), including the
    keys LangGraph's own OSS code (langgraph/pregel/main.py's
    _build_server_info) reads to populate Runtime.server_info for node
    code -- distinct from the Factory Graph's ServerRuntime.user
    (factory_graph.py), which only the graph *factory* sees at build
    time. LangGraph documents this as "the server puts assistant_id/
    graph_id in config configurable and the authenticated user dict in
    configurable['langgraph_auth_user']" -- any hosting server (LangGraph
    Platform, this runner, or any other LangGraph SDK-compatible server)
    is expected to set these three keys for node-level code (e.g.
    deepagents' StoreBackend, which reads
    `get_runtime().server_info.user` to namespace per-user storage) to
    work at all. Without this, server_info is None and such code crashes
    with an AttributeError on first use -- same gap as the Factory Graph
    one, at a different injection point. A pure function (no I/O) so it's
    unit-testable without a live control plane or graph -- see
    test_factory_graph.py's build_run_config checks.
    """
    run_id = assignment["run_id"]
    graph_id = assignment["graph_id"]
    config = assignment.get("config") or {}

    config.setdefault("configurable", {})
    config["configurable"]["thread_id"] = assignment["thread_id"]
    config["configurable"]["run_id"] = run_id
    config["configurable"]["assistant_id"] = graph_id
    config["configurable"]["graph_id"] = graph_id
    if assignment.get("user"):
        user = RunnerUser(assignment["user"])
        config["configurable"]["langgraph_auth_user"] = user
        # LangGraph Platform hosting convenience shortcuts some agents
        # read instead of (or in addition to) server_info.user.
        config["configurable"]["user_id"] = user.identity
        config["configurable"]["user_display_name"] = user.display_name
    return config


def find_new_tool_calls(obj: Any, seen_ids: set) -> list[dict]:
    """Recursively scan a LangGraph stream chunk for AIMessage.tool_calls
    the control plane's on_tool_call hook watches for (master plan gap:
    "neither runner emits that method today"). `seen_ids` is mutated in
    place (dedup state) -- pass a fresh set per run.

    Checked in every stream_mode ("values"/"updates" give complete,
    already-materialized messages per graph step; "messages" streams
    per-token deltas, so an early chunk's tool_calls may have incomplete
    args from partially-parsed JSON -- deduping by id means we might
    emit on an early, args-incomplete chunk rather than the final one, a
    real but minor trade-off against the alternative of restricting
    detection to values/updates only and missing tool calls entirely for
    messages-only stream requests).
    """
    found: list[dict] = []
    tool_calls = getattr(obj, "tool_calls", None)
    if tool_calls:
        for tc in tool_calls:
            tc_id = tc.get("id") if isinstance(tc, dict) else getattr(tc, "id", None)
            if not tc_id or tc_id in seen_ids:
                continue
            seen_ids.add(tc_id)
            found.append(tc if isinstance(tc, dict) else dict(tc))
    elif isinstance(obj, dict):
        for v in obj.values():
            found.extend(find_new_tool_calls(v, seen_ids))
    elif isinstance(obj, (list, tuple)):
        for item in obj:
            found.extend(find_new_tool_calls(item, seen_ids))
    return found


async def execute_run(adapter: LangGraphAdapter, assignment: dict, event_callback, cancel_event: asyncio.Event | None = None) -> str:
    """Execute a single run and emit events via callback.

    Returns the final status: 'success', 'error', or 'interrupted'.
    """
    run_id = assignment["run_id"]
    graph_id = assignment["graph_id"]
    input_data = assignment.get("input")
    config = build_run_config(assignment)
    stream_modes = assignment.get("stream_modes", ["values"])
    checkpoint_ref = assignment.get("checkpoint_ref")
    resume_command = assignment.get("resume_command")

    seq = 0

    def _serialize(obj: Any) -> Any:
        """Convert LangChain objects to JSON-serializable dicts."""
        if obj is None:
            return None
        if isinstance(obj, (str, int, float, bool)):
            return obj
        if isinstance(obj, dict):
            return {k: _serialize(v) for k, v in obj.items()}
        if isinstance(obj, (list, tuple)):
            return [_serialize(item) for item in obj]
        # LangChain BaseMessage objects
        if hasattr(obj, "model_dump"):
            return obj.model_dump()
        if hasattr(obj, "dict"):
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

    seen_tool_call_ids: set = set()

    try:
        # Emitted BEFORE building the graph, not after: for a factory
        # graph (see factory_graph.py) this construction runs fresh, on
        # the request path, every single run -- it can genuinely take
        # seconds (LLM client setup, tool binding, MCP session
        # negotiation), and a static graph has zero equivalent cost since
        # it's built once at startup. A client waiting for ANY signal
        # that its run was even accepted, before the first real token,
        # needs this ordering -- previously this fired only after the
        # (possibly slow) factory call resolved, so the caller saw
        # nothing at all until construction finished.
        await event_callback(make_event("lifecycle", {"event": "running"}))

        async with contextlib.AsyncExitStack() as stack:
            if adapter.is_factory(graph_id):
                # Factory graph (master plan: LangGraph SDK/ServerRuntime
                # compatibility) --
                # built fresh for this run alone, with checkpointer/store
                # attached to THIS instance, not the shared one static
                # graphs use. See factory_graph.py for the full rationale.
                run_context = RunFactoryContext(
                    run_id=run_id,
                    thread_id=assignment["thread_id"],
                    user=assignment.get("user"),
                )
                graph_cm = await adapter.build_factory_graph(graph_id, config, run_context)
                graph = await stack.enter_async_context(graph_cm)
            else:
                graph = adapter.get_graph(graph_id)

            # Handle resume (HITL)
            if resume_command is not None:
                from langgraph.types import Command
                input_data = Command(resume=resume_command.get("response"))

            # Determine stream mode for LangGraph
            lg_stream_mode = []
            for mode in stream_modes:
                if mode in ("values", "updates", "messages"):
                    lg_stream_mode.append(mode)
            if not lg_stream_mode:
                lg_stream_mode = ["values"]

            # Stream the graph
            has_interrupt = False
            last_values = None

            async for chunk in graph.astream(input_data, config=config, stream_mode=lg_stream_mode):
                # LangGraph returns different shapes based on stream_mode
                if isinstance(chunk, tuple) and len(chunk) == 2:
                    mode, data = chunk
                else:
                    mode = lg_stream_mode[0] if len(lg_stream_mode) == 1 else "values"
                    data = chunk

                for tc in find_new_tool_calls(data, seen_tool_call_ids):
                    await event_callback(make_event("tool_call", {
                        "name": tc.get("name"),
                        "args": tc.get("args"),
                        "id": tc.get("id"),
                    }))

                # Check for interrupts
                if isinstance(data, dict) and "__interrupt__" in data:
                    has_interrupt = True
                    interrupts = data["__interrupt__"]
                    # Emit lifecycle interrupted
                    await event_callback(make_event("lifecycle", {"event": "interrupted"}))
                    # Emit input.requested for each interrupt
                    for interrupt in (interrupts if isinstance(interrupts, (list, tuple)) else [interrupts]):
                        interrupt_id = None
                        interrupt_value = None
                        if isinstance(interrupt, dict):
                            interrupt_id = interrupt.get("id", f"interrupt-{seq}")
                            interrupt_value = interrupt.get("value")
                        elif hasattr(interrupt, "id"):
                            interrupt_id = interrupt.id
                            interrupt_value = getattr(interrupt, "value", None)
                        else:
                            interrupt_id = f"interrupt-{seq}"
                            interrupt_value = interrupt

                        await event_callback(make_event("input.requested", {
                            "interrupt_id": interrupt_id,
                            "value": interrupt_value,
                        }))

                    # Clean data for the values event
                    clean_data = {k: v for k, v in data.items() if k != "__interrupt__"}
                    if clean_data:
                        await event_callback(make_event(mode, clean_data))
                    continue

                # Check for cancellation
                if cancel_event is not None and cancel_event.is_set():
                    await event_callback(make_event("end", {"status": "interrupted"}))
                    return "interrupted"

                # Normal event
                await event_callback(make_event(mode, data))

                if mode == "values":
                    last_values = data

            if has_interrupt:
                await event_callback(make_event("end", {"status": "interrupted"}))
                return "interrupted"
            else:
                await event_callback(make_event("end", {"status": "success"}))
                return "success"

    except asyncio.CancelledError:
        await event_callback(make_event("end", {"status": "interrupted"}))
        return "interrupted"
    except Exception as e:
        logger.exception(f"Run {run_id} failed: {e}")
        await event_callback(make_event("error", {
            "message": str(e),
            "type": type(e).__name__,
        }))
        return "error"


async def run_worker(
    config_path: str,
    grpc_address: str = "localhost:50051",
    http_address: str = "http://localhost:2026",
    runner_kind: str = "python-langgraph",
    concurrency: int = 1,
):
    """Main worker loop: poll for jobs, execute, stream events back.

    concurrency controls how many jobs this process handles at once (see
    _poll_loop's dispatcher below) -- default 1 preserves the original
    one-job-at-a-time behavior exactly. Also sizes the checkpointer/store
    connection pools (see checkpoint.py/store.py) so concurrent jobs' DB
    I/O doesn't serialize on a single shared connection.
    """
    adapter = LangGraphAdapter()
    await adapter.load_config(config_path)

    # Runner auth (master plan's "two-tier" model): if the control plane has
    # RUNNER_TOKEN_<KIND> configured (production mode), this runner must send
    # a matching runner-kind/runner-token pair as gRPC metadata on every
    # call. In local mode (no token set here or on the server), this is a
    # no-op -- runners are trusted implicitly.
    runner_token = os.environ.get("RUNNER_TOKEN", "")
    auth_metadata = [("runner-kind", runner_kind), ("runner-token", runner_token)] if runner_token else []

    # Checkpoint dual mode (master plan): direct-mode Postgres when the
    # runner has the same DATABASE_URL the control plane uses, so thread
    # state survives runner restarts; falls back to in-memory (explicitly
    # ephemeral) otherwise. See checkpoint.py for the full rationale.
    postgres_dsn = os.environ.get("POSTGRES_DSN")
    checkpointer_manager = CheckpointerManager()
    await checkpointer_manager.start(postgres_dsn, pool_size=concurrency)
    adapter.attach_checkpointer(checkpointer_manager)

    # Store dual mode (master plan: "Unified Store, Dual Mode, Same as
    # Checkpoints"): same direct/proxy split as checkpoints, but for the
    # BaseStore-backed KV store instead of thread checkpoints. Direct mode
    # queries the control plane's own store_items table straight over
    # Postgres; proxy mode calls its /store/* HTTP API. Either way it's the
    # exact same rows non-Python clients see via HTTP -- one store, not two.
    store = RunkiteStore(
        postgres_dsn=postgres_dsn,
        http_base_url=http_address,
        runner_token=runner_token,
        pool_size=concurrency,
    )
    adapter.attach_store(store)
    logger.info(f"Store mode: {store.mode}")

    logger.info(f"Connecting to control plane at {grpc_address}")
    # Keepalive so the control plane detects a dead/crashed runner quickly
    # instead of relying on TCP-level detection, which can leave a
    # crashed runner's in-flight GetJob long-poll "zombie" server-side for
    # a long time -- long enough to steal a job meant for its replacement
    # and then lose it (the response can never reach a dead client).
    # Matches the server's keepalive.ServerParameters in cmd/serve.go.
    channel = grpc.aio.insecure_channel(grpc_address, options=[
        ("grpc.keepalive_time_ms", 2000),
        ("grpc.keepalive_timeout_ms", 2000),
        ("grpc.keepalive_permit_without_calls", 1),
    ])
    stub = runner_pb2_grpc.RunnerServiceStub(channel)

    logger.info(f"Worker ready. Polling for jobs as runner_kind={runner_kind}")

    # Track cancel events by run_id. WatchCancels fires into these.
    #
    # pre_cancelled closes a real race (same one the TypeScript runner's
    # recordCancelSignal/registerRun already handles, and generic_worker's
    # pre_cancelled/register_run mirrors): a cancel signal can arrive over
    # this SEPARATE WatchCancels stream before _poll_loop below has
    # registered run_id in pending_cancels (GetJob -> parse -> register
    # takes real time). Without remembering that early signal, it was
    # silently dropped -- `ev is None` -> nothing happens -- and the run
    # proceeds to completion, ignoring a cancel request the client
    # believes was honored. Orphaned entries (a signal for a run_id this
    # runner never picks up) expire so this can't leak forever.
    pending_cancels: dict[str, asyncio.Event] = {}
    pre_cancelled: set[str] = set()
    pending_cancels_lock = asyncio.Lock()

    async def _expire_pre_cancel(run_id: str):
        await asyncio.sleep(60)
        async with pending_cancels_lock:
            pre_cancelled.discard(run_id)

    async def watch_cancels():
        """Subscribe to cancel signals via gRPC server-streaming RPC."""
        while True:
            try:
                stream = stub.WatchCancels(runner_pb2.WatchCancelsRequest(
                    runner_kind=runner_kind,
                ), metadata=auth_metadata)
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

    # Start the cancel watcher as a background task
    cancel_watcher_task = asyncio.create_task(watch_cancels())

    # Custom routes, in-runner mode (master plan: "Custom routes"). Same
    # process, same event loop as the poll loop above -- see custom_app.py's
    # docstring for the trade-off that implies. Sidecar mode needs nothing
    # here at all: it's just a separate process the control plane proxies
    # to directly, configured entirely on the Go side.
    custom_app_task = None
    custom_app_config = _load_custom_app_config(config_path)
    if custom_app_config is not None:
        app = load_asgi_app(Path(config_path).parent, custom_app_config["module"])
        custom_app_task = asyncio.create_task(serve_custom_app(
            app, custom_app_config.get("host", "127.0.0.1"), custom_app_config.get("port", 8100),
        ))

    # Tracks jobs currently being handled as spawned asyncio.Tasks (see
    # _poll_loop's dispatcher) -- populated/drained by _poll_loop itself,
    # but must survive _poll_loop's own cancellation below so the finally
    # block can await any still-running job before tearing down the
    # checkpointer/store connections those jobs depend on.
    in_flight_tasks: set[asyncio.Task] = set()

    try:
        await _poll_loop(
            stub, adapter, runner_kind, auth_metadata, pending_cancels, pre_cancelled, pending_cancels_lock,
            concurrency=concurrency, in_flight=in_flight_tasks,
        )
    finally:
        cancel_watcher_task.cancel()
        if custom_app_task is not None:
            custom_app_task.cancel()
        if in_flight_tasks:
            logger.info(f"Draining {len(in_flight_tasks)} in-flight job(s) before shutdown...")
            await asyncio.gather(*in_flight_tasks, return_exceptions=True)
        await checkpointer_manager.stop()
        await store.aclose()


def _load_custom_app_config(config_path: str) -> dict | None:
    """Reads the "custom_app" section straight from langgraph.json --
    kept separate from LangGraphAdapter's own config handling since it's an
    orthogonal, optional concern (the runner's own HTTP-hosting decision,
    not a graph definition)."""
    with open(config_path) as f:
        raw = json.load(f)
    return raw.get("custom_app")


async def register_run(
    pending_cancels: dict[str, asyncio.Event],
    pre_cancelled: set[str],
    lock: asyncio.Lock,
    run_id: str,
) -> asyncio.Event:
    """Claim (or create) the cancel Event for run_id. If a cancel signal
    already arrived (pre_cancelled), the Event is returned already set --
    see watch_cancels' pre_cancelled doc comment in run_worker above for
    the race this closes."""
    async with lock:
        ev = pending_cancels.get(run_id)
        if ev is None:
            ev = asyncio.Event()
            pending_cancels[run_id] = ev
        if run_id in pre_cancelled:
            pre_cancelled.discard(run_id)
            ev.set()
        return ev


async def _handle_job(
    stub,
    adapter: "LangGraphAdapter",
    response,
    auth_metadata: list,
    pending_cancels: dict,
    pre_cancelled: set,
    pending_cancels_lock: asyncio.Lock,
):
    """Execute one dispatched job end-to-end: register cancel, stream
    events, execute_run, report status. Split out of _poll_loop so the
    dispatcher can run one of these per concurrency slot as an
    independent asyncio.Task -- everything here is fresh per-call local
    state (run_id, event_queue, etc.), so there's no shared-closure risk
    running many of these concurrently.

    Errors are caught here, not left to propagate to the dispatcher --
    with concurrency > 1, an unhandled exception in one job's task must
    never take down the dispatcher (and every OTHER in-flight job with
    it). The old single-job loop caught these at the outer `while True`
    level; that's no longer where "one job" lives.
    """
    run_id = None
    stream_task = None
    status = "error"
    try:
        assignment = json.loads(response.assignment_json)
        run_id = assignment["run_id"]
        logger.info(f"Got job: run_id={run_id} graph_id={assignment['graph_id']}")

        # Open one persistent client-stream per run.
        event_queue: asyncio.Queue[runner_pb2.RunEventProto | None] = asyncio.Queue()

        async def event_generator():
            """Yield events into the gRPC client-stream until None sentinel."""
            while True:
                item = await event_queue.get()
                if item is None:
                    return
                yield item

        async def send_event(event: dict):
            """Queue an event for streaming to the control plane."""
            await event_queue.put(runner_pb2.RunEventProto(
                run_id=run_id,
                event_json=json.dumps(event),
            ))

        # Register a cancel event for this run, claiming any
        # pre-arrived cancel signal for it.
        cancel_event = await register_run(pending_cancels, pre_cancelled, pending_cancels_lock, run_id)

        try:
            # Start the gRPC stream call
            stream_call = stub.StreamEvents(event_generator(), metadata=auth_metadata)

            async def run_stream():
                return await stream_call

            stream_task = asyncio.ensure_future(run_stream())

            # Execute the agent with cancel support
            status = await execute_run(adapter, assignment, send_event, cancel_event=cancel_event)
        finally:
            # Always clear the cancel registration, even if execute_run
            # itself raised -- otherwise a failure here would leak an
            # entry in pending_cancels for every job that hits it.
            async with pending_cancels_lock:
                pending_cancels.pop(run_id, None)

        # Signal end of stream and wait for gRPC to finish
        await event_queue.put(None)
        if stream_task is not None:
            try:
                await stream_task
            except Exception as e:
                logger.error(f"Stream finalization error: {e}")

        # Report final status -- must always happen once run_id is known,
        # even if StreamEvents setup failed above (otherwise the run
        # stays "running" forever on the control plane).
        await stub.ReportStatus(runner_pb2.ReportStatusRequest(
            run_id=run_id,
            status=status,
            error_message="" if status != "error" else "see error event",
        ), metadata=auth_metadata)

        logger.info(f"Run completed: run_id={run_id} status={status}")

    except grpc.aio.AioRpcError as e:
        logger.error(f"gRPC error handling run_id={run_id}: {e.code()} {e.details()}")
    except Exception as e:
        logger.exception(f"Worker error handling run_id={run_id}: {e}")
        if run_id is not None:
            with contextlib.suppress(Exception):
                await stub.ReportStatus(runner_pb2.ReportStatusRequest(
                    run_id=run_id,
                    status="error",
                    error_message=str(e),
                ), metadata=auth_metadata)


async def _poll_loop(
    stub,
    adapter: "LangGraphAdapter",
    runner_kind: str,
    auth_metadata: list,
    pending_cancels: dict,
    pre_cancelled: set,
    pending_cancels_lock: asyncio.Lock,
    concurrency: int = 1,
    in_flight: set[asyncio.Task] | None = None,
):
    """Semaphore-bounded dispatcher: long-polls for jobs and hands each
    one to its own _handle_job task, up to `concurrency` running at
    once. concurrency=1 (the default) preserves the exact original
    one-job-at-a-time behavior -- GetJob is only called again once the
    single in-flight job's task has finished.

    The semaphore is acquired BEFORE GetJob, not after a job is
    received: at capacity, the dispatcher simply doesn't poll for more
    work, which is the backpressure this needs and nothing extra to
    build. `in_flight` (if provided) lets run_worker's shutdown drain
    tasks that outlive this loop's own cancellation -- see run_worker's
    finally block.
    """
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
            # Long-poll for a job
            response = await stub.GetJob(runner_pb2.GetJobRequest(
                runner_kind=runner_kind,
                timeout_seconds=30,
            ), metadata=auth_metadata)
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
            continue  # no job, poll again

        task = asyncio.create_task(_handle_job(
            stub, adapter, response, auth_metadata, pending_cancels, pre_cancelled, pending_cancels_lock,
        ))
        in_flight.add(task)
        task.add_done_callback(_on_job_done)


def main():
    import argparse
    parser = argparse.ArgumentParser(description="Runkite Python Runner")
    parser.add_argument("--config", default="langgraph.json", help="Path to langgraph.json")
    parser.add_argument(
        "--grpc-address",
        default=os.environ.get("RUNKITE_GRPC_URL", "localhost:50051"),
        help="Control plane gRPC address (env: RUNKITE_GRPC_URL)",
    )
    parser.add_argument(
        "--http-address",
        default=os.environ.get("RUNKITE_HTTP_URL", "http://localhost:2026"),
        help="Control plane HTTP address (env: RUNKITE_HTTP_URL)",
    )
    parser.add_argument("--runner-kind", default="python-langgraph", help="Runner kind identifier")
    parser.add_argument(
        "--concurrency",
        type=int,
        default=int(os.environ.get("RUNKITE_CONCURRENCY", "1")),
        help="Max concurrent in-flight jobs per runner process (env: RUNKITE_CONCURRENCY, default: 1)",
    )
    args = parser.parse_args()

    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(name)s %(levelname)s %(message)s",
    )

    asyncio.run(run_worker(args.config, args.grpc_address, args.http_address, args.runner_kind, args.concurrency))


if __name__ == "__main__":
    main()
