"""Shared helpers must use the authenticated kind of the current worker."""

import asyncio
import unittest
from unittest.mock import patch

import httpx
from langgraph.store.base import GetOp
from runkite_runner.a2a import call_agent
from runkite_runner.connectors import get_connector_session
from runkite_runner.store import RunkiteStore
from runkite_runner.tenant_ctx import bind_runner, reset_runner
from runkite_runner.vectorstore import RunkiteVectorStore


class IdentityTests(unittest.IsolatedAsyncioTestCase):
    async def test_all_helpers_follow_concurrent_kind_bindings(self):
        original_client = httpx.AsyncClient
        seen = []

        def respond(request):
            seen.append(request)
            kind = request.headers["X-Runner-Kind"]
            self.assertEqual(request.headers["X-Runner-Token"], kind + "-token")
            return httpx.Response(404 if request.url.path == "/internal/store/items" else 200, json={})

        transport = httpx.MockTransport(respond)

        def client(*args, **kwargs):
            return original_client(*args, transport=transport, **kwargs)

        async def job(kind):
            binding = bind_runner(kind, kind + "-token")
            try:
                await asyncio.sleep(0)
                config = {"configurable": {"run_id": "run", "generation": 2}}
                await get_connector_session(config, "workspace", control_plane_url="http://plane")
                await call_agent(config, "worker", {}, wait=False, control_plane_url="http://plane")
                store = RunkiteStore(http_base_url="http://plane")
                await store.abatch([GetOp(namespace=("n",), key="k")])
                vectors = RunkiteVectorStore(None, namespace="n", http_base_url="http://plane")
                await vectors._upsert_proxy("id", "content", {}, [1.0])
            finally:
                reset_runner(binding)

        with patch("httpx.AsyncClient", client):
            await asyncio.gather(job("python-crewai"), job("python-autogen"))
        self.assertEqual(len(seen), 8)
        self.assertEqual({request.headers["X-Runner-Kind"] for request in seen}, {"python-crewai", "python-autogen"})


if __name__ == "__main__":
    unittest.main()
