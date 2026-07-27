"""Self-check for Custom Routes, in-runner mode (custom_app.py).

Proves:
1. load_asgi_app resolves a "path/to/module.py:app_symbol" reference the
   same way langgraph.json's "graphs" section already does for agents.
2. serve_custom_app actually serves that app over real HTTP via uvicorn,
   and can be cancelled cleanly (the same asyncio.create_task +
   task.cancel() lifecycle worker.py uses alongside the poll loop).

Usage:
    python/.venv/bin/python python/tests/test_custom_app.py
"""

import asyncio
import os
import sys
from pathlib import Path

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner.custom_app import load_asgi_app, serve_custom_app  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


def test_load_asgi_app():
    repo_root = Path(__file__).resolve().parents[2]
    app_dir = repo_root / "examples" / "custom_routes_agent"
    app = load_asgi_app(app_dir, "./app.py:app")
    check("load_asgi_app returns an ASGI-callable", callable(app))


async def test_serve_and_reach_over_http():
    repo_root = Path(__file__).resolve().parents[2]
    app_dir = repo_root / "examples" / "custom_routes_agent"
    app = load_asgi_app(app_dir, "./app.py:app")

    port = 8765
    task = asyncio.create_task(serve_custom_app(app, "127.0.0.1", port))
    try:
        # Give uvicorn a moment to bind before hitting it.
        import httpx

        deadline = asyncio.get_event_loop().time() + 5
        last_err = None
        async with httpx.AsyncClient() as client:
            while asyncio.get_event_loop().time() < deadline:
                try:
                    resp = await client.get(f"http://127.0.0.1:{port}/ping", timeout=1)
                    check("served app responds 200", resp.status_code == 200)
                    check("served app returns expected body", resp.json() == {"pong": True})

                    resp2 = await client.get(f"http://127.0.0.1:{port}/echo/hello", timeout=1)
                    check("path param route works", resp2.json() == {"echo": "hello"})
                    return
                except httpx.ConnectError as e:
                    last_err = e
                    await asyncio.sleep(0.1)
        raise RuntimeError(f"server never became reachable: {last_err}")
    finally:
        task.cancel()
        try:
            await task
        except asyncio.CancelledError:
            pass
        check("serve task actually stops on cancel", task.cancelled() or task.done())


def main():
    try:
        import fastapi  # noqa: F401
    except ImportError:
        print(
            "Skipping custom_app test (fastapi not installed -- it's the example\n"
            "app's framework choice, not a runkite_runner dependency; uvicorn\n"
            "alone is required at runtime, for any ASGI framework). Install with:\n"
            "  uv pip install fastapi (from the python/ directory)"
        )
        return
    test_load_asgi_app()
    asyncio.run(test_serve_and_reach_over_http())
    print("\nAll checks passed.")


if __name__ == "__main__":
    main()
