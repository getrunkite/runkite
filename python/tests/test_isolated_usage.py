"""Usage frame validation and real HTTP client behavior without model calls."""

import json
import unittest

import httpx
from runkite_runner.isolated_usage import RunUsageClient, UsageSession, output_usage, validate_report
from runkite_runner.tenant_ctx import bind_run, bind_tenant, reset_run, reset_tenant


class IsolatedUsageTests(unittest.IsolatedAsyncioTestCase):
    def test_unknown_and_reported_zero_are_different(self):
        unknown = validate_report({"schema_version": 1, "sequence": 1, "status": "unknown", "complete": False})
        zero = validate_report(
            {
                "schema_version": 1,
                "sequence": 1,
                "status": "reported",
                "complete": True,
                "prompt_tokens": 0,
                "completion_tokens": 0,
                "cost_usd": 0,
            }
        )
        self.assertTrue(output_usage(unknown, 1)["unmetered"])
        self.assertNotIn("prompt_tokens", output_usage(unknown, 1))
        self.assertFalse(output_usage(zero, 1)["unmetered"])
        self.assertEqual(output_usage(zero, 1)["total_tokens"], 0)
        self.assertTrue(output_usage(zero, 1)["cost_reported"])

    def test_malformed_contradictory_and_lossy_values_fail_closed(self):
        good = {
            "schema_version": 1,
            "sequence": 1,
            "status": "reported",
            "complete": False,
            "prompt_tokens": 1,
            "completion_tokens": 0,
        }
        for bad in [
            {**good, "sequence": True},
            {**good, "schema_version": 2},
            {**good, "complete": 1},
            {**good, "prompt_tokens": -1},
            {**good, "prompt_tokens": 1.5},
            {**good, "prompt_tokens": True},
            {**good, "prompt_tokens": 1 << 53},
            {**good, "cost_usd": float("nan")},
            {**good, "cost_usd": float("inf")},
            {**good, "status": "unknown"},
            {**good, "source": "trusted"},
            {**good, "total_tokens": 99},
            {**good, "model": "x\n"},
        ]:
            with self.subTest(value=str(bad)), self.assertRaises(ValueError):
                validate_report(bad)

    async def test_lost_http_ack_retries_exact_cumulative_report_with_bound_identity(self):
        received = []

        async def respond(request):
            self.assertEqual(request.url.raw_path, b"/internal/runs/custom%2Frun/usage")
            self.assertEqual(request.headers["x-runkite-run-id"], "custom/run")
            self.assertEqual(request.headers["x-runkite-generation"], "7")
            self.assertEqual(request.headers["x-runkite-tenant-id"], "acme")
            self.assertEqual(request.headers["x-runner-kind"], "pool-fixture")
            self.assertEqual(request.headers["x-runner-token"], "synthetic-token")
            value = json.loads(request.content)
            received.append(value)
            if len(received) == 1:
                raise httpx.ReadError("synthetic lost acknowledgement")
            return httpx.Response(
                200, json={"snapshot": {**value, "source": "isolated_workload", "reported_generation": 7}}
            )

        client = RunUsageClient(
            "https://plane.invalid", "pool-fixture", "synthetic-token", transport=httpx.MockTransport(respond)
        )
        tenant = bind_tenant("acme")
        run = bind_run("custom/run", 7, run_incarnation_id="record-fixture")
        try:
            report = {
                "schema_version": 1,
                "sequence": 1,
                "status": "reported",
                "complete": True,
                "prompt_tokens": 10,
                "completion_tokens": 4,
            }
            snapshot = await client.report("custom/run", report)
            self.assertEqual(received, [report, report])
            self.assertEqual(snapshot["prompt_tokens"], 10)
        finally:
            reset_run(run)
            reset_tenant(tenant)
            await client.aclose()

    async def test_session_rejects_changed_ack_and_reserved_output_usage(self):
        class Client:
            async def get(self, _run):
                return None

            async def report(self, _run, value):
                return {**value, "source": "isolated_workload", "reported_generation": 1}

        session = UsageSession(Client(), "run")
        await session.start()
        with self.assertRaises(ValueError):
            session.output({"usage": {"prompt_tokens": 999}})
        frame = {
            "type": "usage",
            "schema_version": 1,
            "sequence": 1,
            "status": "reported",
            "complete": True,
            "prompt_tokens": 0,
            "completion_tokens": 0,
        }
        self.assertEqual(await session.receive(frame), 1)
        self.assertEqual(await session.receive(frame), 1)
        with self.assertRaises(ValueError):
            await session.receive({**frame, "sequence": 2})
        self.assertFalse(session.output({})["usage"]["unmetered"])


if __name__ == "__main__":
    unittest.main()
