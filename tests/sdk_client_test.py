"""
One real LangGraph SDK client test against the live Runkite server.

Prerequisites:
  1. pip install langgraph-sdk
  2. Start the control plane:  cd <project-root> && go run ./cmd/main.go
     (with at least one agent registered, e.g. LANGGRAPH_CONFIG=examples/echo_agent/langgraph.json)

Run:
  python tests/sdk_client_test.py

This validates GO-006: LangGraph SDK client connects and functions correctly.
"""

import asyncio
import os
import sys

# ponytail: stdlib only for imports; langgraph-sdk is the sole external dep.
try:
    from langgraph_sdk import get_client
except ImportError:
    print("ERROR: langgraph-sdk not installed. Run: pip install langgraph-sdk")
    sys.exit(1)


BASE_URL = os.environ.get("RUNKITE_URL", "http://localhost:2026")


async def main():
    client = get_client(url=BASE_URL)
    passed = 0
    failed = 0

    # --- Test 1: List/search agents ---
    print("Test 1: Search agents...", end=" ")
    try:
        agents = await client.assistants.search()
        assert isinstance(agents, list), f"expected list, got {type(agents)}"
        print(f"OK ({len(agents)} agents)")
        passed += 1
    except Exception as e:
        print(f"FAIL: {e}")
        failed += 1

    # --- Test 2: Create a thread ---
    print("Test 2: Create thread...", end=" ")
    try:
        thread = await client.threads.create(metadata={"test": "sdk"})
        assert thread["thread_id"], "missing thread_id"
        thread_id = thread["thread_id"]
        print(f"OK (thread_id={thread_id[:8]}...)")
        passed += 1
    except Exception as e:
        print(f"FAIL: {e}")
        failed += 1
        thread_id = None

    # --- Test 3: Get thread ---
    if thread_id:
        print("Test 3: Get thread...", end=" ")
        try:
            t = await client.threads.get(thread_id)
            assert t["thread_id"] == thread_id
            assert t["status"] == "idle"
            print("OK")
            passed += 1
        except Exception as e:
            print(f"FAIL: {e}")
            failed += 1

    # --- Test 4: Search threads ---
    print("Test 4: Search threads...", end=" ")
    try:
        threads = await client.threads.search()
        assert isinstance(threads, list)
        print(f"OK ({len(threads)} threads)")
        passed += 1
    except Exception as e:
        print(f"FAIL: {e}")
        failed += 1

    # --- Test 5: Store operations ---
    print("Test 5: Store put/get...", end=" ")
    try:
        # namespace is positional-only in the SDK (uses / separator)
        await client.store.put_item(
            ["sdk", "test"],
            key="hello",
            value={"message": "world"},
        )
        item = await client.store.get_item(
            ["sdk", "test"],
            key="hello",
        )
        assert item["value"]["message"] == "world", f"unexpected value: {item}"
        print("OK")
        passed += 1
    except Exception as e:
        print(f"FAIL: {e}")
        failed += 1

    # --- Test 6: Store search ---
    print("Test 6: Store search...", end=" ")
    try:
        results = await client.store.search_items(
            ["sdk"],
        )
        assert len(results["items"]) >= 1
        print(f"OK ({len(results['items'])} items)")
        passed += 1
    except Exception as e:
        print(f"FAIL: {e}")
        failed += 1

    # --- Test 7: Delete thread ---
    if thread_id:
        print("Test 7: Delete thread...", end=" ")
        try:
            await client.threads.delete(thread_id)
            print("OK")
            passed += 1
        except Exception as e:
            print(f"FAIL: {e}")
            failed += 1

    # --- Test 8: Store delete ---
    print("Test 8: Store delete...", end=" ")
    try:
        await client.store.delete_item(
            ["sdk", "test"],
            key="hello",
        )
        print("OK")
        passed += 1
    except Exception as e:
        print(f"FAIL: {e}")
        failed += 1

    # --- Summary ---
    total = passed + failed
    print(f"\n{'='*40}")
    print(f"Results: {passed}/{total} passed")
    if failed:
        print(f"         {failed} FAILED")
        sys.exit(1)
    else:
        print("All SDK client tests passed!")


if __name__ == "__main__":
    asyncio.run(main())
