"""Offline guard tests. Subprocesses and backend HTTP are mocked."""

import argparse
import base64
import io
import json
import os
import subprocess
import sys
import tempfile
import time
import unittest
from contextlib import redirect_stderr, redirect_stdout
from copy import deepcopy
from pathlib import Path
from unittest.mock import Mock, patch

import qualify

TRACE_ID = "11" * 16
PROJECT = "synthetic-qualification-project"
AUTH = "Basic " + base64.b64encode(b"pk-synthetic:sk-private-fixture").decode("ascii")
HEADERS = "Authorization=" + qualify.quote(AUTH, safe="") + ",x-langfuse-ingestion-version=4"


def receipts():
    return {
        language: {
            "schema_version": 1,
            "trace_id": TRACE_ID,
            "span_id": identifier * 8,
            "traceparent": f"00-{TRACE_ID}-{identifier * 8}-01",
            "service": qualify.SERVICES[language],
            "span_name": "run" if language == "go" else "runkite.run",
            "protocol": "http/json" if language == "typescript" else "http/protobuf",
        }
        for language, identifier in (("go", "22"), ("python", "33"), ("typescript", "44"))
    }


def observation_payload():
    ids = receipts()

    def row(language, name, identifier, parent, attributes):
        return {
            "id": identifier,
            "traceId": TRACE_ID,
            "projectId": PROJECT,
            "name": name,
            "type": "SPAN",
            "parentObservationId": parent,
            "startTime": "2026-11-17T00:00:00Z",
            "endTime": "2026-11-17T00:00:01Z",
            "metadata": {
                "scope.name": "runkite",
                "resourceAttributes.service.name": qualify.SERVICES[language],
                **{"attributes." + key: value for key, value in attributes.items()},
            },
            "model": "",
            "input": None,
            "output": None,
            "usageDetails": {},
            "costDetails": {},
            "totalCost": None,
        }

    rows = [
        row(
            "go",
            "run",
            ids["go"]["span_id"],
            None,
            {
                "run.id": "synthetic-run",
                "thread.id": "synthetic-thread",
                "graph.id": "synthetic-agent",
                "run.status": "success",
            },
        )
    ]
    for language, llm, tool in (("python", "55" * 8, "66" * 8), ("typescript", "77" * 8, "88" * 8)):
        run_id = f"synthetic-langfuse-{language}-run"
        attributes = {
            "run.id": run_id,
            "thread.id": f"synthetic-langfuse-{language}-thread",
            "graph.id": "synthetic-langfuse-agent",
            "tenant.id": "synthetic-langfuse-tenant",
            "correlation.id": "synthetic-langfuse-correlation",
            "run.status": "success",
        }
        rows.extend(
            [
                row(language, "runkite.run", ids[language]["span_id"], ids["go"]["span_id"], attributes),
                row(
                    language,
                    "runkite.llm",
                    llm,
                    ids[language]["span_id"],
                    {"run.id": run_id, "llm.name": "synthetic-langfuse-model"},
                ),
                row(language, "runkite.tool", tool, llm, {"run.id": run_id, "tool.name": "synthetic-langfuse-tool"}),
            ]
        )
    return {"data": rows, "meta": {}}


class Guards(unittest.TestCase):
    def test_only_literal_loopback_root_is_accepted(self):
        self.assertEqual(qualify.validate_url("http://127.0.0.1:3210/"), "http://127.0.0.1:3210")
        for value in (
            "http://localhost:3000",
            "https://127.0.0.1:3000",
            "http://127.0.0.1",
            "http://127.0.0.1:0",
            "http://127.0.0.1:65536",
            "http://127.0.0.1:3000/path",
            "http://127.0.0.1:3000/%2f",
            "http://user:password@127.0.0.1:3000",
            "http://127.0.0.1.example:3000",
            "http://[::1]:3000",
            "http://127.0.0.1:3000?",
            "http://127.0.0.1:3000#",
            " http://127.0.0.1:3000",
            "http://127.0.0.1:3000\n",
        ):
            with self.subTest(value=value), self.assertRaises(qualify.QualificationError):
                qualify.validate_url(value)

    def test_header_contract_rejects_ambiguity_and_credential_injection(self):
        self.assertEqual(qualify.parse_headers(HEADERS), {"authorization": AUTH, "x-langfuse-ingestion-version": "4"})
        self.assertEqual(
            qualify.parse_headers(HEADERS.replace("Authorization", "AUTHORIZATION"))["authorization"], AUTH
        )
        for raw in (
            "",
            HEADERS + ",AUTHORIZATION=duplicate",
            HEADERS + ",X-Other=secret",
            HEADERS.replace("version=4", "version=3"),
            HEADERS.replace("Basic%20", "Bearer%20"),
            "Authorization=Basic%20not-base64,x-langfuse-ingestion-version=4",
            HEADERS + "%zz",
            HEADERS + "%0a",
            "Authorization=Basic%20Og%3D%3D,x-langfuse-ingestion-version=4",
        ):
            with self.subTest(case=len(raw)), self.assertRaises(qualify.QualificationError):
                qualify.parse_headers(raw)

    def test_json_rejects_duplicates_nonfinite_and_invalid_encoding(self):
        for data in (b'{"a":1,"a":2}', b'{"a":NaN}', b'{"a":1e9999}', b"\xff", b"{"):
            with self.assertRaises(qualify.QualificationError):
                qualify.strict_json(data)

    def test_receipts_require_exact_identity_schema_and_private_file(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "receipt.json"
            original = receipts()["python"]
            malformed = []
            for key, value in (
                ("extra", "forbidden"),
                ("schema_version", True),
                ("span_id", "0" * 16),
                ("traceparent", receipts()["go"]["traceparent"]),
                ("service", "wrong"),
                ("protocol", "http/json"),
            ):
                malformed.append({**original, key: value})
            malformed.append({key: value for key, value in original.items() if key != "service"})
            for index, value in enumerate([original, *malformed]):
                path.write_text(json.dumps(value))
                path.chmod(0o600)
                if index == 0:
                    self.assertEqual(qualify.load_receipt(path, "python"), original)
                else:
                    with self.assertRaises(qualify.QualificationError):
                        qualify.load_receipt(path, "python")
            path.chmod(0o644)
            with self.assertRaises(qualify.QualificationError):
                qualify.load_receipt(path, "python")
            link = Path(directory) / "link.json"
            link.symlink_to(path)
            with self.assertRaises(qualify.QualificationError):
                qualify.load_receipt(link, "python")

    def test_exact_seven_span_tree_and_metadata(self):
        self.assertTrue(qualify.validate_observations(observation_payload(), receipts(), PROJECT))
        partial = observation_payload()
        partial["data"] = partial["data"][:3]
        self.assertFalse(qualify.validate_observations(partial, receipts(), PROJECT))
        mutations = (
            lambda p: p["data"][3].update(parentObservationId=receipts()["go"]["span_id"]),
            lambda p: p["data"][1].update(type="GENERATION"),
            lambda p: p["data"][0].update(projectId="another-project"),
            lambda p: p["data"][0].update(traceId="99" * 16),
            lambda p: p["data"][0].update(parentObservationId="99" * 8),
            lambda p: p["data"][1].update(id=receipts()["go"]["span_id"]),
            lambda p: p["data"][2].update(model="invented-model"),
            lambda p: p["data"][2].update(input="synthetic-prompt-not-exported"),
            lambda p: p["data"][2].update(usageDetails={"input": 1}),
            lambda p: p["data"][2].update(costDetails={"input": False}),
            lambda p: p["data"][2].update(totalCost=0.1),
            lambda p: p["data"][2].update(endTime=None),
            lambda p: p["data"][0]["metadata"].update({"attributes.gen_ai.operation.name": "chat"}),
            lambda p: p["data"][1]["metadata"].update({"attributes.thread.id": "wrong"}),
            lambda p: p["data"][0].update(
                metadata={
                    "resourceAttributes": {"service.name": qualify.SERVICES["go"]},
                    "attributes": {"run.id": "synthetic-run"},
                }
            ),
            lambda p: p["meta"].update(cursor="more-data"),
            lambda p: p["data"].append(deepcopy(p["data"][0])),
        )
        for index, mutate in enumerate(mutations):
            payload = observation_payload()
            mutate(payload)
            with self.subTest(case=index), self.assertRaises(qualify.QualificationError):
                qualify.validate_observations(payload, receipts(), PROJECT)


class HTTPBoundary(unittest.TestCase):
    def request(self, response=None, request_error=None):
        connection = Mock()
        if request_error:
            connection.request.side_effect = request_error
        connection.getresponse.return_value = response
        with patch.object(qualify.http.client, "HTTPConnection", return_value=connection) as factory:
            result = qualify.request_json("http://127.0.0.1:3210", "/api/public/health", {}, time.monotonic() + 20)
            factory.assert_called_once()
            self.assertEqual(factory.call_args.args[:2], ("127.0.0.1", 3210))
            return result

    def test_redirect_is_rejected_without_reading_or_following_location(self):
        response = Mock(status=302)
        with self.assertRaisesRegex(qualify.QualificationError, "redirects"):
            self.request(response)
        response.read1.assert_not_called()

    def test_oversized_response_is_rejected(self):
        response = Mock(status=200)
        response.getheader.return_value = str(qualify.MAX_RESPONSE + 1)
        with self.assertRaises(qualify.QualificationError):
            self.request(response)
        response.read1.assert_not_called()

    def test_successful_closed_body_is_decoded_and_chunked_cap_is_bounded(self):
        response = Mock(status=200, length=0)
        body = b'{"version":"4.81.0"}'
        response.getheader.return_value = str(len(body))
        response.read1.return_value = body
        response.isclosed.return_value = True
        self.assertEqual(self.request(response), (200, {"version": "4.81.0"}))
        response = Mock(status=200, length=None)
        response.getheader.return_value = None
        response.read1.return_value = b"x" * (qualify.MAX_RESPONSE + 1)
        response.isclosed.return_value = False
        with self.assertRaises(qualify.QualificationError):
            self.request(response)
        self.assertEqual(response.read1.call_count, 1)

    def test_timeout_does_not_expose_backend_error(self):
        with self.assertRaises(qualify.QualificationError) as caught:
            self.request(request_error=TimeoutError("private-backend-body"))
        self.assertNotIn("private-backend-body", str(caught.exception))

    def test_child_timeout_is_sanitized(self):
        supervisor = Mock()
        supervisor.run_owned.side_effect = subprocess.TimeoutExpired("sensitive", 1, output=b"private-child-output")
        with patch.object(qualify, "owned_supervisor", return_value=supervisor):
            with self.assertRaises(qualify.QualificationError) as caught:
                qualify.run_child(["not-executed"], Path.cwd(), {}, time.monotonic() + 20, 1, "synthetic emitter")
        self.assertEqual(supervisor.run_owned.call_args.kwargs["stdout"], subprocess.DEVNULL)
        self.assertGreater(supervisor.run_owned.call_args.kwargs["timeout"], 0)
        self.assertLessEqual(supervisor.run_owned.call_args.kwargs["timeout"], 1)
        self.assertNotIn("private-child-output", str(caught.exception))
        with self.assertRaises(qualify.QualificationError):
            qualify.remaining(time.monotonic() - 1, 3)


class EndToEndOffline(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.output = self.directory / "summary.json"
        self.args = argparse.Namespace(
            url="http://127.0.0.1:3210",
            python=sys.executable,
            project_id=PROJECT,
            output=str(self.output),
            expected_version=qualify.VERSION,
        )
        self.source = {
            "PATH": os.defpath,
            "OTEL_EXPORTER_OTLP_TRACES_HEADERS": HEADERS,
            "AWS_SECRET_ACCESS_KEY": "must-not-inherit",
            "OPENAI_API_KEY": "must-not-inherit",
            "HTTP_PROXY": "http://must-not-inherit",
            "OTEL_RESOURCE_ATTRIBUTES": "private=must-not-inherit",
            "NODE_OPTIONS": "must-not-inherit",
            "HOME": "/synthetic/private-home",
            "USERPROFILE": "/synthetic/private-profile",
            "NETRC": "/synthetic/private-netrc",
            "HOMEDRIVE": "synthetic-drive",
            "HOMEPATH": "/synthetic/private-path",
        }
        self.environments = []
        self.reads = 0

    def emit(self, command, cwd, environment, deadline, maximum, stage, capture=False):
        self.environments.append(dict(environment))
        language = next(key for key, value in qualify.SERVICES.items() if value == environment["OTEL_SERVICE_NAME"])
        self.assertEqual(maximum, 45 if language == "go" else 30)
        self.assertEqual(environment["OTEL_EXPORTER_OTLP_TRACES_TIMEOUT"], "3" if language == "python" else "3000")
        self.assertEqual(
            environment.get("RUNKITE_TEST_OTLP_PARENT"), None if language == "go" else receipts()["go"]["traceparent"]
        )
        path = Path(environment["RUNKITE_TEST_OTLP_RECEIPT"])
        path.write_text(json.dumps(receipts()[language]))
        path.chmod(0o600)
        return b""

    def http(self, url, path, headers, deadline):
        if path == "/api/public/health":
            return 200, {"status": "OK", "version": qualify.VERSION}
        self.assertIn("fields=basic%2Ctime%2Cmetadata%2Cio%2Cmodel%2Cusage", path)
        self.assertNotIn("expandMetadata=", path)
        if "authorization" not in headers:
            return 401, None
        if headers["authorization"] != AUTH:
            return 403, None
        self.reads += 1
        payload = observation_payload()
        if self.reads == 1:
            payload["data"] = payload["data"][:3]
        return 200, payload

    def run_fixture(self, http=None, emit=None):
        with (
            patch.object(qualify, "request_json", side_effect=http or self.http),
            patch.object(qualify, "run_child", side_effect=emit or self.emit),
            patch.object(qualify, "collect_versions", return_value={key: "1.2.3" for key in qualify.SDK_KEYS}),
            patch.object(qualify.shutil, "which", side_effect=lambda name, **_kwargs: "/synthetic/bin/" + name),
            patch.object(qualify.time, "sleep"),
        ):
            return qualify.qualify(self.args, self.source, self.directory)

    def test_partial_poll_then_exact_tree_produces_only_private_summary(self):
        summary = self.run_fixture()
        self.assertEqual(summary["observations"], 7)
        self.assertEqual(summary["auth_denial_probes"], 2)
        self.assertEqual(self.reads, 2)
        self.assertEqual(len(self.environments), 3)
        for environment in self.environments:
            for key in (
                "AWS_SECRET_ACCESS_KEY",
                "OPENAI_API_KEY",
                "HTTP_PROXY",
                "OTEL_RESOURCE_ATTRIBUTES",
                "NODE_OPTIONS",
                "OTEL_EXPORTER_OTLP_ENDPOINT",
                "HOMEDRIVE",
                "HOMEPATH",
            ):
                self.assertNotIn(key, environment)
            private_directory = str(Path(environment["RUNKITE_TEST_OTLP_RECEIPT"]).parent)
            self.assertEqual(environment["HOME"], private_directory)
            self.assertEqual(environment["USERPROFILE"], private_directory)
            self.assertEqual(environment["NETRC"], os.devnull)
        data = self.output.read_text()
        for private in (AUTH, "sk-private-fixture", TRACE_ID, PROJECT, "synthetic-prompt-not-exported"):
            self.assertNotIn(private, data)
        self.assertEqual(
            set(json.loads(data)),
            {
                "schema_version",
                "passed",
                "backend_version",
                "sdk_versions",
                "services",
                "observations",
                "span_observations",
                "generation_observations",
                "auth_denial_probes",
                "model_calls",
            },
        )
        self.assertEqual(self.output.stat().st_mode & 0o077, 0)

    def test_rejected_auth_probe_cannot_be_skipped(self):
        def public_read(url, path, headers, deadline):
            return (200, {"version": qualify.VERSION}) if path.endswith("health") else (200, observation_payload())

        with self.assertRaises(qualify.QualificationError):
            self.run_fixture(http=public_read)
        self.assertFalse(self.output.exists())

    def test_wrong_backend_version_and_child_failure_leave_no_summary(self):
        with self.assertRaises(qualify.QualificationError):
            self.run_fixture(http=lambda *_args: (200, {"version": "3.0.0"}))
        with self.assertRaises(qualify.QualificationError):
            self.run_fixture(
                emit=lambda *_args, **_kwargs: (_ for _ in ()).throw(
                    qualify.QualificationError("bounded emitter failure")
                )
            )
        self.assertFalse(self.output.exists())

    def test_partial_observations_exhaust_total_deadline_without_summary(self):
        def partial(url, path, headers, deadline):
            status, payload = self.http(url, path, headers, deadline)
            if status == 200 and path != "/api/public/health":
                payload["data"] = payload["data"][:3]
            return status, payload

        ticks = iter(range(0, 300, 20))
        with patch.object(qualify.time, "monotonic", side_effect=lambda: next(ticks)):
            with self.assertRaisesRegex(qualify.QualificationError, "deadline"):
                self.run_fixture(http=partial)
        self.assertFalse(self.output.exists())

    def test_existing_summary_is_not_overwritten_or_used_for_emission(self):
        self.output.write_text("preserve-existing")
        with patch.object(qualify, "request_json") as http:
            with self.assertRaises(qualify.QualificationError):
                qualify.qualify(self.args, self.source, self.directory)
            http.assert_not_called()
        self.assertEqual(self.output.read_text(), "preserve-existing")

    def test_main_sanitizes_unexpected_failures_and_defaults_version(self):
        stdout, stderr = io.StringIO(), io.StringIO()
        with (
            patch.object(qualify, "qualify", side_effect=RuntimeError("private-backend-body")) as run,
            redirect_stdout(stdout),
            redirect_stderr(stderr),
        ):
            code = qualify.main(
                [
                    "--url",
                    self.args.url,
                    "--python",
                    sys.executable,
                    "--project-id",
                    PROJECT,
                    "--output",
                    str(self.output),
                ]
            )
        self.assertEqual(code, 1)
        self.assertEqual(run.call_args.args[0].expected_version, qualify.VERSION)
        self.assertNotIn("private-backend-body", stderr.getvalue())
        self.assertNotIn("passed", stdout.getvalue())


if __name__ == "__main__":
    unittest.main()
