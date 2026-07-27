"""Locust load test for resource-constrained (CPU/memory-limited container)
testing. Drives the same create-thread-and-run-and-wait cycle as
bench/loadgen (Go), just via Locust so the SAME tool/methodology used for
"how does this behave when squeezed" is the one already chosen for that
job, rather than reinventing it in Go.

Same server-compatibility support as bench/loadgen: set WAIT_PATH=join for
other LangGraph SDK-compatible servers (runkite defaults to "wait"), and
ASSISTANT_ID to the agent_id/assistant_id under test (see
bench/loadgen/main.go's comment for why "assistant_id", not "agent_id", is
the field name used for a request body that works unmodified against any
LangGraph SDK-compatible server).

Reports FOUR separate request names in Locust's stats table, not one
blended "Aggregated" row averaging three structurally different
operations together (a real methodology bug found in review: mixing a
fast create POST, a slow blocking wait/join GET, and a fast status GET
into one bucket makes "p50" represent none of them correctly -- it's
whichever operation happens to be more frequent in the sample, not an
end-to-end cycle time comparable to bench/loadgen's per-cycle p50/p99):
  - "create_run"   -- the POST alone
  - "block_wait"   -- the blocking wait/join GET alone
  - "get_status"   -- the final status-check GET alone
  - "full_cycle"   -- a synthetic request covering the whole three-step
                      cycle's wall time, fired via events.request.fire so
                      it shows up as its own row -- THIS is the number
                      directly comparable to bench/loadgen's p50/p99.
Also uses name="..." (not the literal per-thread URL) on every real
request, so Locust doesn't create one stats row per unique thread_id.

Usage:
    locust -f bench/locust/locustfile.py --host http://localhost:2026            # runkite

Headless, fixed-duration run (what the resource-constrained comparison
actually uses):
    locust -f bench/locust/locustfile.py --host http://localhost:2026 \\
        --headless --users 50 --spawn-rate 10 --run-time 30s --csv bench/locust/runkite
"""

import itertools
import os
import time
import uuid

from locust import HttpUser, between, events, task

ASSISTANT_ID = os.environ.get("ASSISTANT_ID", "echo_agent")
WAIT_PATH = os.environ.get("WAIT_PATH", "wait")  # "wait" (runkite) or "join" (some other LangGraph SDK-compatible servers)

_counter = itertools.count()


class AgentRunUser(HttpUser):
    """One simulated caller: create a thread, dispatch a run, block until
    it finishes, then check its persisted status via a plain GET -- the
    exact same three-step cycle as bench/loadgen/main.go's oneCycle,
    including checking status via a separate GET rather than trusting
    the blocking endpoint's own embedded field (see bench/REPORT.md
    finding #5 for why that distinction matters)."""

    wait_time = between(0, 0)  # fire the next cycle immediately, no think time

    @task
    def create_and_wait_run(self):
        thread_id = f"locust-{int(time.time() * 1000)}-{uuid.uuid4().hex[:8]}-{next(_counter)}"
        cycle_start = time.monotonic()
        cycle_ok = True
        cycle_error = None

        with self.client.post(
            f"/threads/{thread_id}/runs",
            json={"assistant_id": ASSISTANT_ID, "input": {"messages": [{"role": "user", "content": "ping"}]}},
            name="create_run",
            catch_response=True,
        ) as create_resp:
            if create_resp.status_code != 200:
                create_resp.failure(f"create run: status {create_resp.status_code}")
                self._fire_cycle(cycle_start, False, f"create run: status {create_resp.status_code}")
                return
            run_id = create_resp.json()["run_id"]

        with self.client.get(
            f"/threads/{thread_id}/runs/{run_id}/{WAIT_PATH}", name="block_wait", catch_response=True
        ) as block_resp:
            if block_resp.status_code != 200:
                block_resp.failure(f"{WAIT_PATH}: status {block_resp.status_code}")
                self._fire_cycle(cycle_start, False, f"{WAIT_PATH}: status {block_resp.status_code}")
                return

        with self.client.get(f"/threads/{thread_id}/runs/{run_id}", name="get_status", catch_response=True) as status_resp:
            if status_resp.status_code != 200:
                status_resp.failure(f"get run: status {status_resp.status_code}")
                cycle_ok, cycle_error = False, f"get run: status {status_resp.status_code}"
            else:
                status = status_resp.json().get("status")
                if status != "success":
                    status_resp.failure(f"run terminal status={status!r}")
                    cycle_ok, cycle_error = False, f"run terminal status={status!r}"

        self._fire_cycle(cycle_start, cycle_ok, cycle_error)

    def _fire_cycle(self, cycle_start: float, ok: bool, error: str | None):
        """Fires a synthetic "full_cycle" request covering the whole
        three-step cycle's wall time -- the number to actually compare
        against bench/loadgen's per-cycle p50/p99, not the per-request-
        type rows above (each of which times a single HTTP call only)."""
        events.request.fire(
            request_type="CYCLE",
            name="full_cycle",
            response_time=(time.monotonic() - cycle_start) * 1000,
            response_length=0,
            exception=None if ok else RuntimeError(error),
            context=self.context(),
        )


@events.test_start.add_listener
def _log_config(environment, **kwargs):
    print(f"locustfile config: assistant_id={ASSISTANT_ID!r} wait_path={WAIT_PATH!r}")
