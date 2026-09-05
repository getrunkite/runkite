"""
End-to-end SDK test: thread → run → wait/stream with a real runner.

Prerequisites:
  1. pip install langgraph-sdk
  2. Start the control plane with echo_agent bootstrapped:
       RUNKITE_MODE=test LANGGRAPH_CONFIG=examples/echo_agent/langgraph.json ./runkite
  3. Start the Python runner:
       PYTHONPATH=python python -m runkite_runner --config examples/echo_agent/langgraph.json

Run:
    python tests/sdk_e2e_test.py
"""

import asyncio
import os
import sys
import urllib.error
import urllib.request

try:
    from langgraph_sdk import get_client
except ImportError:
    print("ERROR: langgraph-sdk not installed. Run: pip install langgraph-sdk")
    sys.exit(1)

BASE_URL = os.environ.get("RUNKITE_URL", "http://localhost:2026")
AGENT_ID = os.environ.get("RUNKITE_AGENT", "echo_agent")


def _control_plane_reachable() -> bool:
    url = BASE_URL.rstrip("/") + "/health"
    try:
        urllib.request.urlopen(url, timeout=2)
        return True
    except urllib.error.HTTPError:
        return True
    except urllib.error.URLError:
        return False


async def main():
    if not _control_plane_reachable():
        print(f"SKIP: control plane not reachable at {BASE_URL} (set RUNKITE_URL to run)")
        return
    client = get_client(url=BASE_URL)
    passed = 0
    failed = 0
    total_tests = 0

    def report(name, ok, detail=""):
        nonlocal passed, failed, total_tests
        total_tests += 1
        if ok:
            passed += 1
            print(f"  ✓ {name}" + (f" — {detail}" if detail else ""))
        else:
            failed += 1
            print(f"  ✗ {name} — {detail}")

    # =========================================================================
    # 1. Verify agent is available
    # =========================================================================
    print("\n1. Agent discovery")
    try:
        agents = await client.assistants.search()
        agent_ids = [a["assistant_id"] for a in agents]
        report("search agents", AGENT_ID in agent_ids, f"found {len(agents)} agents")
    except Exception as e:
        report("search agents", False, str(e))
        print("FATAL: cannot proceed without agent. Exiting.")
        sys.exit(1)

    # =========================================================================
    # 2. Create thread → background run → poll until done
    # =========================================================================
    print("\n2. Background run (create + poll)")
    thread_id = None
    try:
        thread = await client.threads.create(metadata={"test": "e2e_bg"})
        thread_id = thread["thread_id"]
        report("create thread", True, f"thread_id={thread_id[:8]}...")

        run = await client.runs.create(
            thread_id,
            AGENT_ID,
            input={"messages": [{"role": "human", "content": "background test"}]},
        )
        run_id = run["run_id"]
        report("create background run", True, f"run_id={run_id[:8]}...")
        report("run has assistant_id", run.get("assistant_id") == AGENT_ID,
               f"got {run.get('assistant_id')}")

        # Poll until terminal
        for _ in range(30):
            run = await client.runs.get(thread_id, run_id)
            if run["status"] in ("success", "error", "interrupted"):
                break
            await asyncio.sleep(0.2)

        report("run reached terminal status", run["status"] == "success",
               f"status={run['status']}")
    except Exception as e:
        report("background run flow", False, str(e))

    # =========================================================================
    # 3. Create thread → run/wait (synchronous)
    # =========================================================================
    print("\n3. Synchronous run (create + wait)")
    try:
        thread2 = await client.threads.create(metadata={"test": "e2e_wait"})
        t2_id = thread2["thread_id"]

        result = await client.runs.wait(
            t2_id,
            AGENT_ID,
            input={"messages": [{"role": "human", "content": "wait test"}]},
        )

        report("wait returned result", isinstance(result, dict),
               f"type={type(result).__name__}")

        # Result shape: {"run": {...}, "values": {...}}
        values = result.get("values", {})
        messages = values.get("messages", [])
        report("echo response present", len(messages) >= 2,
               f"{len(messages)} messages")
        if len(messages) >= 2:
            ai_msg = messages[-1]
            content = ai_msg.get("content", "")
            report("echo content correct", content == "wait test",
                   f"got '{content}'")
    except Exception as e:
        report("wait flow", False, str(e))

    # =========================================================================
    # 4. Create thread → run/stream (SSE)
    # =========================================================================
    print("\n4. Streaming run (create + stream)")
    try:
        thread3 = await client.threads.create(metadata={"test": "e2e_stream"})
        t3_id = thread3["thread_id"]

        events = []
        async for chunk in client.runs.stream(
            t3_id,
            AGENT_ID,
            input={"messages": [{"role": "human", "content": "stream test"}]},
            stream_mode="values",
        ):
            events.append(chunk)

        event_types = [e.event for e in events]
        report("received events", len(events) > 0, f"{len(events)} events")
        report("has metadata event", "metadata" in event_types,
               f"types={event_types}")
        report("has values event", "values" in event_types,
               f"types={event_types}")
        report("has end event", "end" in event_types,
               f"types={event_types}")

        # Check the values content
        values_events = [e for e in events if e.event == "values"]
        if values_events:
            last_values = values_events[-1].data
            messages = last_values.get("messages", [])
            report("stream echo correct",
                   len(messages) >= 2 and messages[-1].get("content") == "stream test",
                   f"{len(messages)} messages")
        else:
            report("stream echo correct", False, "no values events")
    except Exception as e:
        report("stream flow", False, str(e))

    # =========================================================================
    # 5. Thread reuse: second run on same thread
    # =========================================================================
    print("\n5. Thread reuse (second run on same thread)")
    try:
        # Wait for thread to be idle (StatusCallback resets it)
        for _ in range(20):
            t3 = await client.threads.get(t3_id)
            if t3["status"] == "idle":
                break
            await asyncio.sleep(0.3)

        result2 = await client.runs.wait(
            t3_id,
            AGENT_ID,
            input={"messages": [{"role": "human", "content": "second run"}]},
        )
        report("second run on same thread", isinstance(result2, (dict, list)),
               f"type={type(result2).__name__}")
    except Exception as e:
        report("thread reuse", False, str(e))

    # =========================================================================
    # 6. Stateless run (no thread_id → auto-create)
    # =========================================================================
    print("\n6. Stateless run (auto-created thread)")
    try:
        result3 = await client.runs.wait(
            None,
            AGENT_ID,
            input={"messages": [{"role": "human", "content": "stateless"}]},
        )
        report("stateless run", isinstance(result3, (dict, list)),
               f"type={type(result3).__name__}")
    except Exception as e:
        report("stateless run", False, str(e))

    # =========================================================================
    # Summary
    # =========================================================================
    print(f"\n{'='*50}")
    print(f"Results: {passed}/{total_tests} passed")
    if failed:
        print(f"         {failed} FAILED")
        sys.exit(1)
    else:
        print("All SDK end-to-end tests passed!")


if __name__ == "__main__":
    asyncio.run(main())
