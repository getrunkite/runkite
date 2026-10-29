import asyncio
import unittest
from types import SimpleNamespace

from runkite_runner.completion import CompletionDeliveryError, finish_run


class CompletionTests(unittest.IsolatedAsyncioTestCase):
    async def test_retry_preserves_execution_status(self):
        seen = []

        async def report(request, metadata):
            seen.append((request.status, request.generation))
            if len(seen) == 1:
                raise RuntimeError("storage unavailable")
            return SimpleNamespace(ok=True, superseded=False)

        stream = asyncio.sleep(0, result=SimpleNamespace(ok=True))
        self.assertEqual(
            await finish_run(SimpleNamespace(ReportStatus=report), "r", 4, "success", stream, [], retry_delay=0),
            "success",
        )
        self.assertEqual(seen, [("success", 4), ("success", 4)])

    async def test_failed_stream_preserves_outcome_without_reporting_replacement_status(self):
        seen = []

        async def report(request, metadata):
            seen.append(request)
            return SimpleNamespace(ok=True, superseded=False)

        for status in ("success", "error", "interrupted", "timeout"):
            with self.subTest(status=status):
                cause = RuntimeError("stream close failed")

                async def stream(error=cause):
                    raise error

                with self.assertRaises(CompletionDeliveryError) as caught:
                    await finish_run(SimpleNamespace(ReportStatus=report), "r", 2, status, stream(), [])
                self.assertIs(caught.exception.__cause__, cause)
        self.assertEqual(seen, [])

    async def test_missing_or_negative_ack_keeps_assignment_recoverable(self):
        async def report(request, metadata):
            self.fail("An unacknowledged stream must not report a terminal status")

        for response in (None, SimpleNamespace(ok=False)):
            with self.subTest(response=response):
                with self.assertRaises(CompletionDeliveryError):
                    await finish_run(
                        SimpleNamespace(ReportStatus=report),
                        "r",
                        2,
                        "success",
                        asyncio.sleep(0, result=response),
                        [],
                    )

    async def test_superseded_is_not_retried(self):
        async def report(request, metadata):
            return SimpleNamespace(ok=False, superseded=True)

        self.assertIsNone(
            await finish_run(
                SimpleNamespace(ReportStatus=report),
                "r",
                1,
                "success",
                asyncio.sleep(0, result=SimpleNamespace(ok=True)),
                [],
            )
        )

    async def test_exhaustion_has_distinct_error(self):
        async def report(request, metadata):
            raise RuntimeError("offline")

        with self.assertRaises(CompletionDeliveryError):
            await finish_run(
                SimpleNamespace(ReportStatus=report),
                "r",
                1,
                "success",
                asyncio.sleep(0, result=SimpleNamespace(ok=True)),
                [],
                attempts=2,
                retry_delay=0,
            )


if __name__ == "__main__":
    unittest.main()
