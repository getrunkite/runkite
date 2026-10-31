"""Exercise blocked transports without a socket server."""

import threading
import time
import unittest
from unittest.mock import patch

from runkite_governance import GovernanceClient, GovernanceError


class Response:
    status = 200

    def __enter__(self):
        return self

    def __exit__(self, *args):
        pass

    def read1(self, size):
        time.sleep(0.2)
        return b""


class DeadlineTests(unittest.TestCase):
    def test_non_success_cannot_become_allow_even_with_json_body(self):
        class Redirect(Response):
            status = 302

            def read1(self, _size):
                raise AssertionError("A redirected response must not be accepted as a decision")

        with patch("urllib.request.OpenerDirector.open", return_value=Redirect()):
            with self.assertRaises(GovernanceError) as caught:
                GovernanceClient("http://plane").decide(agent_id="a", connector="c", tool="t")
        self.assertEqual(caught.exception.status, 302)

    def test_transport_capacity_wait_is_bounded(self):
        workers = threading.BoundedSemaphore(1)
        workers.acquire()
        with patch("runkite_governance._HTTP_WORKERS", workers), patch("urllib.request.OpenerDirector.open") as opener:
            with self.assertRaises(GovernanceError) as caught:
                GovernanceClient("http://plane", timeout_ms=30).action("a")
            self.assertEqual(caught.exception.status, 408)
            opener.assert_not_called()
        workers.release()

    def test_headers_cannot_exceed_deadline(self):
        def blocked(*args, **kwargs):
            self.assertGreater(kwargs["timeout"], 0)
            time.sleep(0.2)
            return Response()

        started = time.monotonic()
        with patch("urllib.request.OpenerDirector.open", blocked):
            with self.assertRaises(GovernanceError) as caught:
                GovernanceClient("http://plane", timeout_ms=30).decide(agent_id="a", connector="c", tool="t")
        self.assertEqual(caught.exception.status, 408)
        self.assertLess(time.monotonic() - started, 0.15)

    def test_cancel_interrupts_blocked_body_wait(self):
        cancelled = threading.Event()
        timer = threading.Timer(0.03, cancelled.set)
        timer.start()
        started = time.monotonic()
        try:
            with patch("urllib.request.OpenerDirector.open", return_value=Response()):
                with self.assertRaises(GovernanceError) as caught:
                    GovernanceClient("http://plane", timeout_ms=1000).action("a", cancelled=cancelled)
            self.assertEqual(caught.exception.status, 499)
            self.assertLess(time.monotonic() - started, 0.15)
        finally:
            timer.cancel()

    def test_body_cannot_exceed_deadline(self):
        with patch("urllib.request.OpenerDirector.open", return_value=Response()):
            with self.assertRaises(GovernanceError) as caught:
                GovernanceClient("http://plane", timeout_ms=30).action("a")
        self.assertEqual(caught.exception.status, 408)


if __name__ == "__main__":
    unittest.main()
