"""In-runner mode for the master plan's "Custom routes" platform extension.

Loads a user-defined ASGI app (FastAPI, Starlette, or any other ASGI
framework) from langgraph.json's "custom_app" section and serves it via
uvicorn, running as a concurrent asyncio task alongside the runner's own
gRPC worker loop (see run_worker in worker.py) -- same process, same event
loop, "similar to dropping a file in your project" per the master plan.

The control plane reverse-proxies /custom/* to wherever this ends up
listening (see internal/config.CustomRoutesEntry / cmd/serve.go's
initCustomRoutesProxy) -- from the control plane's side, in-runner mode and
a separately-run sidecar are the exact same mechanism, just different
processes hosting the target URL.

WSGI frameworks (e.g. Flask) aren't directly supported -- uvicorn only
serves ASGI. Wrap a WSGI app with an adapter (e.g. a2wsgi.WSGIMiddleware)
if that's the framework of choice; this loader doesn't care what kind of
object it gets back as long as it's ASGI-callable.
"""

import importlib.util
import logging
import sys
from pathlib import Path
from typing import Any

logger = logging.getLogger("runkite.runner")


def load_asgi_app(config_dir: Path, module_ref: str) -> Any:
    """Loads an ASGI app object from a "path/to/module.py:app_symbol" ref --
    the same "path:symbol" convention langgraph.json's "graphs" section
    already uses for agent graphs."""
    file_path, export_name = module_ref.split(":", 1)
    abs_path = (config_dir / file_path).resolve()

    spec = importlib.util.spec_from_file_location("runkite_custom_app", str(abs_path))
    if spec is None or spec.loader is None:
        raise ValueError(f"Cannot load custom app module: {abs_path}")

    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)

    return getattr(module, export_name)


async def serve_custom_app(app: Any, host: str, port: int) -> None:
    """Runs an ASGI app via uvicorn until cancelled. Meant to run as a
    concurrent asyncio task (asyncio.create_task) alongside the gRPC
    worker's own poll loop -- both share the process and event loop, so a
    slow/blocking route handler in the user's app can, in principle, delay
    the worker's own async work. That's an inherent trade-off of "in-runner,
    same process" mode; use sidecar mode instead for routes that need
    independent scaling or isolation.
    """
    import uvicorn

    config = uvicorn.Config(app, host=host, port=port, log_level="info", access_log=True)
    server = uvicorn.Server(config)
    logger.info(f"Custom routes (in-runner mode): serving on http://{host}:{port}")
    await server.serve()
