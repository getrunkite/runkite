"""
SDK tests for cancel and HITL (Human-in-the-Loop) flows.

Prerequisites:
  1. pip install langgraph-sdk
  2. Start the control plane with all example agents:
       RUNKITE_MODE=test ./runkite
  3. Start the Python runner with all example agents:
       PYTHONPATH=python python -m runkite_runner --config examples/slow_agent/langgraph.json

Run:
    python tests/sdk_cancel_hitl_test.py

Note: The HITL test requires the runner to be started with the approval_agent
config as well. You can run two runners or use a combined config.
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
    # 1. Cancel: start slow_agent, cancel mid-execution
    # =========================================================================
    print("\n1. Cancel flow (slow_agent)")
    try:
        # Verify slow_agent is available
        agents = await client.assistants.search()
        agent_ids = [a["assistant_id"] for a in agents]
        has_slow = "slow_agent" in agent_ids
        report("slow_agent registered", has_slow, f"agents: {agent_ids}")

        if has_slow:
            thread = await client.threads.create(metadata={"test": "cancel"})
            t_id = thread["thread_id"]

            # Create a background run (slow_agent takes ~6s total)
            run = await client.runs.create(
                t_id,
                "slow_agent",
                input={"messages": [{"role": "human", "content": "go"}], "step": 0},
            )
            run_id = run["run_id"]
            report("background run created", True, f"run_id={run_id[:8]}...")

            # Wait a bit for the runner to pick it up and start step_1
            await asyncio.sleep(1.5)

            # Cancel mid-execution
            cancelled_run = await client.runs.cancel(t_id, run_id)
            report("cancel accepted", True)

            # Poll until terminal
            for _ in range(30):
                run = await client.runs.get(t_id, run_id)
                if run["status"] in ("success", "error", "interrupted"):
                    break
                await asyncio.sleep(0.3)

            report("run status is interrupted", run["status"] == "interrupted",
                   f"status={run['status']}")

            # Thread should be idle or interrupted (not busy)
            for _ in range(10):
                t = await client.threads.get(t_id)
                if t["status"] != "busy":
                    break
                await asyncio.sleep(0.3)
            report("thread not stuck busy after cancel",
                   t["status"] in ("idle", "interrupted"),
                   f"thread_status={t['status']}")
    except Exception as e:
        report("cancel flow", False, str(e))

    # =========================================================================
    # 2. HITL: start approval_agent, receive interrupt, resume
    # =========================================================================
    print("\n2. HITL flow (approval_agent)")
    try:
        has_approval = "approval_agent" in agent_ids
        report("approval_agent registered", has_approval, f"agents: {agent_ids}")

        if has_approval:
            thread2 = await client.threads.create(metadata={"test": "hitl"})
            t2_id = thread2["thread_id"]

            # Stream the approval_agent — it should hit an interrupt
            events = []
            async for chunk in client.runs.stream(
                t2_id,
                "approval_agent",
                input={"messages": [{"role": "human", "content": "send the email"}], "approved": False},
                stream_mode="values",
            ):
                events.append(chunk)

            event_types = [e.event for e in events]
            report("received events from initial run", len(events) > 0,
                   f"{len(events)} events, types={event_types}")

            # Check for lifecycle/interrupted or input.requested event
            has_interrupt = any(
                e.event in ("lifecycle", "input.requested") or
                (e.event == "end" and isinstance(e.data, dict) and e.data.get("status") == "interrupted")
                for e in events
            )
            report("interrupt detected", has_interrupt,
                   f"event_types={event_types}")

            # Thread should now be idle/interrupted (run completed with interrupt)
            for _ in range(10):
                t2 = await client.threads.get(t2_id)
                if t2["status"] != "busy":
                    break
                await asyncio.sleep(0.3)
            report("thread available after interrupt",
                   t2["status"] in ("idle", "interrupted"),
                   f"thread_status={t2['status']}")

            # Resume with approval (send command with resume value)
            result = await client.runs.wait(
                t2_id,
                "approval_agent",
                command={"resume": True},
            )

            report("resume returned result", isinstance(result, dict),
                   f"type={type(result).__name__}")

            # Check the result contains the completion message
            values = result.get("values", {})
            messages = values.get("messages", [])
            report("resume produced messages", len(messages) > 0,
                   f"{len(messages)} messages")

            if messages:
                last_msg = messages[-1]
                content = last_msg.get("content", "")
                report("final message indicates completion",
                       "sent" in content.lower() or "approved" in content.lower() or "complete" in content.lower(),
                       f"content='{content[:60]}'")

    except Exception as e:
        report("HITL flow", False, str(e))

    # =========================================================================
    # Summary
    # =========================================================================
    print(f"\n{'='*50}")
    print(f"Results: {passed}/{total_tests} passed")
    if failed:
        print(f"         {failed} FAILED")
        sys.exit(1)
    else:
        print("All cancel + HITL tests passed!")


if __name__ == "__main__":
    asyncio.run(main())
