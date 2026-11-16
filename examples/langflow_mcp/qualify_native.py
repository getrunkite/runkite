#!/usr/bin/env python3
"""Exercise the installed native Langflow MCP tool path against a local fixture.

Launched by TestLangflowMCPQualification, which owns the real plane/worker.
This is a headless qualification, not an agent loop or a production receipt store.
"""

from __future__ import annotations

import asyncio
import hashlib
import importlib.metadata
import ipaddress
import json
import os
import socket
from pathlib import Path

REVISION = "260cbf347cc5563cc48c79094c175ca31532ea2b"
UTIL_SHA256 = "fc967fe3a496a682d67119404a3d4280dfbe1bc831b21d65f9e4f8e0f39e4e1f"
COMPONENT_SHA256 = "e125822930136495277cddf45a2ea5a5588b8a5f8f3f2e00ead8f16f9f56273e"


def local_connections_only() -> None:
    """Keep the real HTTP transport, while refusing external network effects."""
    original = socket.socket.connect

    def connect(sock, address):
        if isinstance(address, tuple) and not ipaddress.ip_address(address[0]).is_loopback:
            raise AssertionError("qualification attempted a non-loopback connection")
        return original(sock, address)

    socket.socket.connect = connect


def persist_intent(path: Path, payload: dict) -> dict:
    with path.open("x", encoding="utf-8") as file:
        json.dump(payload, file, sort_keys=True)
        file.flush()
        os.fsync(file.fileno())
    return json.loads(path.read_text(encoding="utf-8"))


async def main() -> None:
    local_connections_only()
    from lfx.base.mcp import util

    versions = {name: importlib.metadata.version(name) for name in ("lfx", "mcp", "langchain-core", "pydantic")}
    assert versions["lfx"] == "1.13.2" and versions["mcp"] == "1.30.0", versions
    source = Path(util.__file__)
    assert hashlib.sha256(source.read_bytes()).hexdigest() == UTIL_SHA256
    component = source.parents[2] / "components/models_and_agents/mcp_component.py"
    assert hashlib.sha256(component.read_bytes()).hexdigest() == COMPONENT_SHA256
    endpoint = os.environ["RUNKITE_LANGFLOW_FIXTURE_URL"]
    key = os.environ["RUNKITE_LANGFLOW_FIXTURE_KEY"]
    state = Path(os.environ["LANGFLOW_CONFIG_DIR"])
    state.mkdir(parents=True, exist_ok=True)
    original = persist_intent(state / "intent.json", {
        "message": "Native Langflow provider-free qualification",
        "thread_id": "langflow-qualification-thread",
        "run_id": "langflow-qualification-operation",
    })
    protocols = []

    async def connect(credential: str):
        client = util.MCPStreamableHttpClient(tool_execution_timeout=15)
        try:
            mode, tools, cache = await util.update_tools(
                "runkite-local-qualification",
                {"url": endpoint + "/mcp", "mode": "Streamable_HTTP", "headers": {"Authorization": "Bearer " + credential}, "allow_sse_fallback": False},
                mcp_streamable_http_client=client,
            )
            assert mode == "Streamable_HTTP" and {tool.name for tool in tools} == {"echo_agent"}
            tool = cache["echo_agent"]
            assert "run_id" in tool.args_schema.model_fields
            # The native utility stores the actual initialize result for its
            # server-info display. Read it for evidence, never to modify it.
            initialized = getattr(client.session, "_lf_initialize_result")
            protocols.append(initialized.protocolVersion)
            return client, tool
        except BaseException:
            await close(client)
            raise

    async def close(client):
        await client.disconnect()
        manager = getattr(client, "_session_manager", None)
        if manager is not None:
            await manager.cleanup_all()

    def text(result) -> str:
        assert not result.isError
        return "".join(item.text for item in result.content if item.type == "text")

    client, tool = await connect(key)
    try:
        assert text(await tool.ainvoke(original)) == original["message"]
    finally:
        await close(client)
    client, tool = await connect(key)
    try:
        saved = json.loads((state / "intent.json").read_text())
        assert text(await tool.ainvoke(saved)) == original["message"]
        try:
            await tool.ainvoke({**saved, "message": "Conflicting retry"})
        except ValueError as error:
            assert "run_id reused with different input" in str(error), str(error)
        else:
            raise AssertionError("native tool accepted a conflicting run ID")
        failed = persist_intent(state / "failed-intent.json", {
            "message": "__langflow_qualification_error__", "thread_id": "langflow-error-thread", "run_id": "langflow-error-operation",
        })
        for _ in range(2):
            try:
                await tool.ainvoke(failed)
            except ValueError as error:
                assert "run errored:" in str(error), str(error)
            else:
                raise AssertionError("native tool converted a failed run into success")
    finally:
        await close(client)
    try:
        denied, _ = await connect("wrong-qualification-credential")
    except Exception:
        pass
    else:
        await close(denied)
        raise AssertionError("native client connected with an invalid credential")
    receipt = {
        "passed": True, "langflow_revision": REVISION, "versions": versions,
        "native_client_sha256": UTIL_SHA256, "native_component_sha256": COMPONENT_SHA256,
        "protocol_versions": sorted(set(protocols)), "transport": "streamable_http",
        "scope": "Native lfx update_tools, schema conversion and StructuredTool.ainvoke; real local Runkite and runner. No Langflow UI or model loop.",
        "checks": ["discovery", "bearer_auth", "native_schema_conversion", "native_tool_execution", "reconnect_same_run_id", "conflicting_retry", "terminal_error", "terminal_error_retry", "invalid_credential"],
    }
    with Path(os.environ["RUNKITE_LANGFLOW_FIXTURE_RECEIPT"]).open("x", encoding="utf-8") as file:
        json.dump(receipt, file, indent=2)


if __name__ == "__main__":
    asyncio.run(main())
