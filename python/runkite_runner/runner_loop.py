"""Event loop that owns the runner's AsyncConnectionPools.

store.warm() / vectorstore.warm() bind this so sync APIs called from
asyncio.to_thread (deepagents, LangChain tools) can schedule onto the
pool's owning loop instead of asyncio.run() on a fresh one.
"""

from __future__ import annotations

import asyncio

_loop: asyncio.AbstractEventLoop | None = None


def bind(loop: asyncio.AbstractEventLoop | None) -> None:
    global _loop
    _loop = loop


def get() -> asyncio.AbstractEventLoop | None:
    return _loop
