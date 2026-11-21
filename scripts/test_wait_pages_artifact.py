#!/usr/bin/env python3
"""Exercise Pages metadata timing and identity checks without GitHub requests."""

import copy
import io
import json
from pathlib import Path
import unittest
from unittest.mock import patch
import urllib.error

import wait_pages_artifact as pages


class Clock:
    def __init__(self):
        self.elapsed = 0
        self.sleeps = []

    def now(self):
        return self.elapsed

    def sleep(self, seconds):
        assert seconds >= 0
        self.sleeps.append(seconds)
        self.elapsed += seconds


class PagesArtifactTests(unittest.TestCase):
    repository = "getrunkite/runkite"
    run_id = "44866334794"
    artifact_id = "14710294801"
    name = "github-pages-44866334794-3"

    def setUp(self):
        self.clock = Clock()
        self.visible = {"total_count": 1, "artifacts": [{
            "id": int(self.artifact_id), "name": self.name,
            "expired": False, "size_in_bytes": 2678111,
        }]}
        self.calls = []

    def wait(self, responses=None, fetch=None, **kwargs):
        if fetch is None:
            responses = iter(responses or [self.visible])

            def fetch(*args):
                self.calls.append(args)
                result = next(responses)
                if isinstance(result, Exception):
                    raise result
                return copy.deepcopy(result)

        return pages.wait_for_artifact(
            kwargs.get("repository", self.repository), kwargs.get("run_id", self.run_id),
            kwargs.get("artifact_id", self.artifact_id), kwargs.get("artifact_name", self.name),
            kwargs.get("token", "test-token"), fetch=fetch,
            now=self.clock.now, sleep=self.clock.sleep)

    def test_exact_uploaded_artifact_is_ready_without_sleep(self):
        self.wait()
        self.assertEqual(self.clock.sleeps, [])
        url, token, timeout = self.calls[0]
        self.assertEqual(url, "https://api.github.com/repos/getrunkite/runkite/actions/runs/"
                         "44866334794/artifacts?name=github-pages-44866334794-3&per_page=100")
        self.assertEqual(token, "test-token")
        self.assertEqual(timeout, pages.REQUEST_SECONDS)

    def test_missing_metadata_is_polled_until_visible(self):
        self.wait([{"total_count": 0, "artifacts": []}, self.visible])
        self.assertEqual(self.clock.sleeps, [pages.POLL_SECONDS])

    def test_zero_sized_metadata_is_polled_until_complete(self):
        incomplete = copy.deepcopy(self.visible)
        incomplete["artifacts"][0]["size_in_bytes"] = 0
        self.wait([incomplete, self.visible])
        self.assertEqual(len(self.calls), 2)

    def test_temporary_metadata_read_failure_is_retried(self):
        self.wait([pages.RetryableRead("unavailable"), self.visible])
        self.assertEqual(len(self.calls), 2)

    def test_missing_metadata_times_out_without_deployment(self):
        def missing(*args):
            self.calls.append(args)
            return {"total_count": 0, "artifacts": []}

        with self.assertRaisesRegex(pages.ArtifactError, "Timed out"):
            self.wait(fetch=missing)
        self.assertEqual(self.clock.elapsed, pages.WAIT_SECONDS)
        self.assertLessEqual(len(self.calls), pages.WAIT_SECONDS // pages.POLL_SECONDS)
        self.assertTrue(all(call[2] <= pages.REQUEST_SECONDS for call in self.calls))

    def test_sleep_is_bounded_by_remaining_deadline(self):
        def delayed(*args):
            self.calls.append(args)
            self.clock.elapsed += 59
            return {"total_count": 0, "artifacts": []}

        with self.assertRaisesRegex(pages.ArtifactError, "Timed out"):
            self.wait(fetch=delayed)
        self.assertEqual(self.clock.elapsed, 60)
        self.assertEqual(self.clock.sleeps, [1])

    def test_request_timeout_shrinks_to_remaining_deadline(self):
        def delayed(*args):
            self.calls.append(args)
            if len(self.calls) == 1:
                self.clock.elapsed += 49
                return {"total_count": 0, "artifacts": []}
            return self.visible

        self.wait(fetch=delayed)
        self.assertEqual([call[2] for call in self.calls], [10, 9])

    def test_success_arriving_after_deadline_is_rejected(self):
        def delayed(*_args):
            self.clock.elapsed += pages.WAIT_SECONDS
            return self.visible

        with self.assertRaisesRegex(pages.ArtifactError, "Timed out"):
            self.wait(fetch=delayed)

    def test_authentication_error_is_not_retried(self):
        with self.assertRaisesRegex(pages.ArtifactError, "denied"):
            self.wait([pages.ArtifactError("denied")])
        self.assertEqual(self.clock.sleeps, [])

    def test_duplicate_names_fail_instead_of_selecting_latest(self):
        duplicate = copy.deepcopy(self.visible)
        duplicate["total_count"] = 2
        duplicate["artifacts"].append(dict(duplicate["artifacts"][0], id=123))
        with self.assertRaisesRegex(pages.ArtifactError, "Multiple artifacts"):
            self.wait([duplicate])
        self.assertEqual(self.clock.sleeps, [])

    def test_wrong_identity_and_expired_artifacts_fail_closed(self):
        for field, value in (("id", 123), ("id", True), ("name", "github-pages"),
                             ("expired", True), ("expired", None),
                             ("size_in_bytes", -1), ("size_in_bytes", True)):
            metadata = copy.deepcopy(self.visible)
            metadata["artifacts"][0][field] = value
            with self.subTest(field=field, value=value), self.assertRaises(pages.ArtifactError):
                self.wait([metadata])

    def test_invalid_or_incomplete_metadata_fails_closed(self):
        for metadata in (None, [], {}, {"total_count": True, "artifacts": []},
                         {"total_count": -1, "artifacts": []},
                         {"total_count": 1, "artifacts": []},
                         {"total_count": 0, "artifacts": [{}]},
                         {"total_count": 1, "artifacts": [None]}):
            with self.subTest(metadata=metadata), self.assertRaises(pages.ArtifactError):
                self.wait([metadata])

    def test_invalid_inputs_fail_before_metadata_request(self):
        cases = ({"repository": "getrunkite/runkite/other"}, {"repository": "x/y?secret=x"},
                 {"run_id": "0"}, {"run_id": "42\n"}, {"artifact_id": ""},
                 {"artifact_id": "01"}, {"artifact_name": "github-pages"},
                 {"artifact_name": "github-pages-123-1"},
                 {"artifact_name": self.name + "\n"}, {"token": ""})
        for kwargs in cases:
            with self.subTest(kwargs=kwargs), self.assertRaises(pages.ArtifactError):
                self.wait(**kwargs)
        self.assertEqual(self.calls, [])

    def test_http_reads_use_token_header_and_no_mutation(self):
        response = io.BytesIO(json.dumps(self.visible).encode())
        with patch.object(pages.urllib.request, "urlopen", return_value=response) as opened:
            self.assertEqual(pages.github_json("https://api.github.com/example", "test-token", 3),
                             self.visible)
        request = opened.call_args.args[0]
        self.assertEqual(request.get_method(), "GET")
        self.assertIsNone(request.data)
        self.assertEqual(request.get_header("Authorization"), "Bearer test-token")
        self.assertEqual(opened.call_args.kwargs["timeout"], 3)

    def test_transient_http_statuses_are_retryable_without_response_body(self):
        for status in (404, 408, 429, 500, 502, 503, 504):
            error = urllib.error.HTTPError("https://api.github.com", status,
                                           "sensitive body", {}, None)
            with self.subTest(status=status), patch.object(pages.urllib.request, "urlopen", side_effect=error):
                with self.assertRaises(pages.RetryableRead) as caught:
                    pages.github_json("https://api.github.com", "test-token", 3)
                self.assertNotIn("sensitive", str(caught.exception))

    def test_permanent_http_statuses_fail_without_exposing_response(self):
        for status in (400, 401, 403, 422):
            error = urllib.error.HTTPError("https://api.github.com", status,
                                           "test-token", {}, None)
            with self.subTest(status=status), patch.object(pages.urllib.request, "urlopen", side_effect=error):
                with self.assertRaises(pages.ArtifactError) as caught:
                    pages.github_json("https://api.github.com", "test-token", 3)
                self.assertNotIsInstance(caught.exception, pages.RetryableRead)
                self.assertNotIn("test-token", str(caught.exception))

    def test_transport_errors_are_retryable(self):
        for error in (urllib.error.URLError("test-token"), TimeoutError("test-token")):
            with self.subTest(error=error), patch.object(pages.urllib.request, "urlopen", side_effect=error):
                with self.assertRaises(pages.RetryableRead) as caught:
                    pages.github_json("https://api.github.com", "test-token", 3)
                self.assertNotIn("test-token", str(caught.exception))

    def test_malformed_and_oversized_metadata_fail(self):
        for data in (b"not json", b"\xff", b" " * (pages.MAX_METADATA_BYTES + 1)):
            with self.subTest(size=len(data)), patch.object(pages.urllib.request, "urlopen", return_value=io.BytesIO(data)):
                with self.assertRaises(pages.ArtifactError):
                    pages.github_json("https://api.github.com", "test-token", 3)

    def test_workflow_uses_one_attempt_name_for_upload_and_deploy(self):
        root = Path(__file__).resolve().parents[1]
        workflow = (root / ".github/workflows/pages.yml").read_text()
        self.assertEqual(workflow.count("PAGES_ARTIFACT_NAME: github-pages-${{ github.run_id }}-${{ github.run_attempt }}"), 1)
        self.assertIn("name: ${{ env.PAGES_ARTIFACT_NAME }}", workflow)
        self.assertIn("artifact_name: ${{ env.PAGES_ARTIFACT_NAME }}", workflow)
        self.assertIn("PAGES_ARTIFACT_ID: ${{ steps.upload.outputs.artifact_id }}", workflow)
        self.assertIn("actions: read", workflow)
        self.assertIn("cancel-in-progress: false", workflow)
        self.assertNotIn("continue-on-error", workflow)


if __name__ == "__main__":
    unittest.main()
