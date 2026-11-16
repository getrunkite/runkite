#!/usr/bin/env python3
"""Bounded local Langfuse proof of existing SDK/span helpers, without dispatch."""

from __future__ import annotations

import argparse
import base64
import binascii
import http.client
import importlib.util
import json
import math
import os
import re
import secrets
import shutil
import stat
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from urllib.parse import quote, unquote, urlencode, urlsplit

VERSION = "4.81.0"
MAX_RESPONSE = 2 * 1024 * 1024
RECEIPT_KEYS = {"schema_version", "trace_id", "span_id", "traceparent", "service", "span_name", "protocol"}
SENTINELS = (
    "synthetic-prompt-not-exported",
    "synthetic-completion-not-exported",
    "synthetic-tool-input-not-exported",
    "synthetic-tool-output-not-exported",
)
SERVICES = {language: f"synthetic-{language}-service" for language in ("go", "python", "typescript")}
SDK_KEYS = {
    "go",
    "go_otel",
    "go_otlp_http",
    "python",
    "python_otel_sdk",
    "python_otlp_http",
    "node",
    "typescript_otel_sdk",
    "typescript_otlp_http",
}


class QualificationError(Exception):
    """Messages are fixed diagnostics and never contain backend/child output."""


def strict_json(data: bytes):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise QualificationError("duplicate JSON field")
            result[key] = value
        return result

    def constant(_value):
        raise QualificationError("non-finite JSON value")

    def finite_float(value):
        number = float(value)
        if not math.isfinite(number):
            raise QualificationError("non-finite JSON value")
        return number

    try:
        return json.loads(
            data.decode("utf-8"), object_pairs_hook=pairs, parse_constant=constant, parse_float=finite_float
        )
    except (ValueError, UnicodeError, RecursionError):
        raise QualificationError("invalid JSON response or receipt") from None


def validate_url(raw: str) -> str:
    try:
        parsed = urlsplit(raw)
        if (
            len(raw) > 1024
            or any(character.isspace() or ord(character) < 32 for character in raw)
            or parsed.scheme != "http"
            or parsed.hostname != "127.0.0.1"
            or not re.fullmatch(r"127\.0\.0\.1:[0-9]{1,5}", parsed.netloc)
            or parsed.port is None
            or not 1 <= parsed.port <= 65535
            or parsed.path not in ("", "/")
            or "?" in raw
            or "#" in raw
            or parsed.username is not None
            or parsed.password is not None
        ):
            raise ValueError
    except ValueError:
        raise QualificationError(
            "URL must be an explicit HTTP port on literal 127.0.0.1 with a root path only"
        ) from None
    return raw.rstrip("/")


def parse_headers(raw: str) -> dict[str, str]:
    if not raw or len(raw) > 8192:
        raise QualificationError("exact trace authentication headers are required")
    headers = {}
    for item in raw.split(","):
        key, separator, value = item.partition("=")
        key = key.strip().lower()
        if not separator or key not in {"authorization", "x-langfuse-ingestion-version"} or key in headers:
            raise QualificationError("trace headers must contain exactly the two supported fields")
        if re.search(r"%(?![0-9a-fA-F]{2})", value):
            raise QualificationError("invalid percent encoding in trace headers")
        try:
            value = unquote(value.strip(), errors="strict")
        except UnicodeError:
            raise QualificationError("invalid percent encoding in trace headers") from None
        if not value or any(ord(character) < 32 or ord(character) > 126 for character in value):
            raise QualificationError("invalid trace header value")
        headers[key] = value
    if (
        set(headers) != {"authorization", "x-langfuse-ingestion-version"}
        or headers["x-langfuse-ingestion-version"] != "4"
    ):
        raise QualificationError("ingestion version 4 and Basic authentication are required")
    basic_credentials(headers["authorization"])
    return headers


def basic_credentials(authorization: str) -> tuple[str, str]:
    try:
        if not authorization.startswith("Basic "):
            raise ValueError
        decoded = base64.b64decode(authorization[6:], validate=True).decode("ascii")
        if decoded.count(":") != 1:
            raise ValueError
        public, secret = decoded.split(":")
        if not public or not secret or any(ord(character) <= 32 or ord(character) > 126 for character in decoded):
            raise ValueError
        return public, secret
    except (ValueError, UnicodeError, binascii.Error):
        raise QualificationError("valid Basic project authentication is required") from None


def remaining(deadline: float, maximum: float) -> float:
    duration = min(maximum, deadline - time.monotonic())
    if duration <= 0:
        raise QualificationError("qualification deadline exceeded")
    return duration


def request_json(url: str, path: str, headers: dict[str, str], deadline: float):
    """Direct loopback HTTP ignores proxies and never follows a redirect."""
    endpoint = urlsplit(validate_url(url))
    request_deadline = time.monotonic() + remaining(deadline, 3)
    connection = http.client.HTTPConnection("127.0.0.1", endpoint.port, timeout=remaining(request_deadline, 3))
    try:
        connection.request("GET", path, headers={**headers, "Accept": "application/json"})
        sock = connection.sock
        if sock is None:
            raise QualificationError("backend connection was not established")
        sock.settimeout(remaining(request_deadline, 3))
        response = connection.getresponse()
        if 300 <= response.status < 400:
            raise QualificationError("backend redirects are forbidden")
        if response.status != 200:
            return response.status, None
        size = response.getheader("Content-Length")
        if size is not None and (not size.isdecimal() or int(size) > MAX_RESPONSE):
            raise QualificationError("backend response exceeds the bounded JSON contract")
        body = bytearray()
        while True:
            sock.settimeout(remaining(request_deadline, 3))
            chunk = response.read1(min(65536, MAX_RESPONSE + 1 - len(body)))
            if not chunk:
                break
            body.extend(chunk)
            if len(body) > MAX_RESPONSE:
                raise QualificationError("backend response exceeds the bounded JSON contract")
            if response.isclosed():
                break
        if response.length not in (None, 0):
            raise QualificationError("backend response was incomplete")
        return response.status, strict_json(body)
    except (OSError, http.client.HTTPException, ValueError):
        raise QualificationError("backend HTTP request failed or timed out") from None
    finally:
        connection.close()


def owned_supervisor():
    source = Path(__file__).resolve().parents[2] / "examples/trinity_specialist_mcp/qualify.py"
    spec = importlib.util.spec_from_file_location("runkite_langfuse_owned_process", source)
    if spec is None or spec.loader is None:
        raise QualificationError("the checkout's process supervisor is unavailable")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def run_child(
    command: list[str],
    cwd: Path,
    environment: dict[str, str],
    deadline: float,
    maximum: float,
    stage: str,
    capture=False,
) -> bytes:
    try:
        if capture:
            result = subprocess.run(
                command,
                cwd=cwd,
                env=environment,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                timeout=remaining(deadline, maximum),
                check=False,
            )
        else:
            # Reuse the existing process-group cleanup for Go's test binary
            # and any descendants, including failed or timed-out emissions.
            result = owned_supervisor().run_owned(
                command,
                cwd=cwd,
                env=environment,
                stdout=subprocess.DEVNULL,
                timeout=remaining(deadline, maximum),
            )
    except subprocess.TimeoutExpired:
        raise QualificationError(f"{stage} exceeded its bounded timeout") from None
    except OSError:
        raise QualificationError(f"{stage} could not start; prepare local prerequisites") from None
    if result.returncode != 0:
        raise QualificationError(f"{stage} failed; inspect local prerequisites without sharing credentials")
    output = result.stdout or b""
    if len(output) > 65536:
        raise QualificationError(f"{stage} returned an oversized version response")
    return output


def base_environment(source: dict[str, str], directory: Path) -> dict[str, str]:
    result = {
        key: source[key]
        for key in ("PATH", "LANG", "LC_ALL", "GOCACHE", "GOMODCACHE", "GOPATH", "GOROOT")
        if key in source
    }
    result.update(
        {
            "TMPDIR": str(directory),
            "CGO_ENABLED": "0",
            "GOENV": "off",
            # requests falls back to the account database when HOME is absent.
            # Never let an ambient netrc replace the explicit local fixture auth.
            "HOME": str(directory),
            "USERPROFILE": str(directory),
            "NETRC": os.devnull,
            "GOPROXY": "off",
            "GOTOOLCHAIN": "local",
            "GOWORK": "off",
            "GOCACHE": result.get("GOCACHE", str(directory / "go-build")),
            "GOMODCACHE": result.get("GOMODCACHE", str(directory / "go-mod")),
        }
    )
    return result


def collect_versions(
    repository: Path, python: str, tools: dict[str, str], environment: dict[str, str], deadline: float
) -> dict[str, str]:
    go_version = (
        run_child([tools["go"], "version"], repository, environment, deadline, 5, "Go version probe", True)
        .decode("ascii")
        .split()
    )
    if len(go_version) != 4 or go_version[:2] != ["go", "version"]:
        raise QualificationError("unexpected Go version response")
    versions = {"go": go_version[2]}
    for module, key in (
        ("go.opentelemetry.io/otel", "go_otel"),
        ("go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp", "go_otlp_http"),
    ):
        versions[key] = (
            run_child(
                [tools["go"], "list", "-mod=readonly", "-m", "-f", "{{.Version}}", module],
                repository,
                environment,
                deadline,
                5,
                "Go SDK version probe",
                True,
            )
            .decode("ascii")
            .strip()
        )
    script = (
        "import json,platform,importlib.metadata as m;print(json.dumps({"
        "'python':platform.python_version(),'python_otel_sdk':m.version('opentelemetry-sdk'),"
        "'python_otlp_http':m.version('opentelemetry-exporter-otlp-proto-http')}))"
    )
    versions.update(
        strict_json(
            run_child(
                [python, "-I", "-B", "-c", script],
                repository,
                environment,
                deadline,
                5,
                "Python SDK version probe",
                True,
            )
        )
    )
    script = (
        "const p=require;console.log(JSON.stringify({node:process.version,"
        "typescript_otel_sdk:p('@opentelemetry/sdk-trace-base/package.json').version,"
        "typescript_otlp_http:p('@opentelemetry/exporter-trace-otlp-http/package.json').version}))"
    )
    versions.update(
        strict_json(
            run_child(
                [tools["node"], "-e", script],
                repository / "typescript/runkite-runner",
                environment,
                deadline,
                5,
                "TypeScript SDK version probe",
                True,
            )
        )
    )
    if set(versions) != SDK_KEYS or any(
        not isinstance(value, str) or not re.fullmatch(r"[A-Za-z0-9.+_/-]{1,80}", value) for value in versions.values()
    ):
        raise QualificationError("unexpected SDK version schema")
    return versions


def valid_id(value, length: int) -> bool:
    return (
        isinstance(value, str) and re.fullmatch(rf"[0-9a-f]{{{length}}}", value) is not None and value != "0" * length
    )


def load_receipt(path: Path, language: str) -> dict:
    try:
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(descriptor, "rb") as receipt_file:
            info = os.fstat(receipt_file.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_size > 4096:
                raise QualificationError("emitter receipt is not a bounded private regular file")
            receipt = strict_json(receipt_file.read(4097))
    except OSError:
        raise QualificationError("emitter did not create a private receipt") from None
    if (
        not isinstance(receipt, dict)
        or set(receipt) != RECEIPT_KEYS
        or type(receipt["schema_version"]) is not int
        or receipt["schema_version"] != 1
        or not valid_id(receipt["trace_id"], 32)
        or not valid_id(receipt["span_id"], 16)
        or receipt["traceparent"] != f"00-{receipt['trace_id']}-{receipt['span_id']}-01"
        or receipt["service"] != SERVICES[language]
        or receipt["span_name"] != ("run" if language == "go" else "runkite.run")
        or receipt["protocol"] != ("http/json" if language == "typescript" else "http/protobuf")
    ):
        raise QualificationError("emitter receipt schema or identity is invalid")
    return receipt


def no_private_content(value):
    if isinstance(value, dict):
        for key, item in value.items():
            if key.startswith("gen_ai.") or ".gen_ai." in key:
                raise QualificationError("unexpected GenAI attributes in ordinary metadata spans")
            no_private_content(item)
    elif isinstance(value, list):
        for item in value:
            no_private_content(item)
    elif isinstance(value, str) and any(sentinel in value for sentinel in SENTINELS):
        raise QualificationError("a synthetic content sentinel escaped into backend observations")


def validate_observations(payload, receipts: dict[str, dict], project: str) -> bool:
    if (
        not isinstance(payload, dict)
        or set(payload) != {"data", "meta"}
        or not isinstance(payload["data"], list)
        or not isinstance(payload["meta"], dict)
    ):
        raise QualificationError("unexpected observation response schema")
    rows = payload["data"]
    if len(rows) > 7 or payload["meta"].get("cursor"):
        raise QualificationError("unexpected extra or paginated observations")
    no_private_content(payload)
    if len(rows) < 7:
        return False
    indexed = {}
    seen = set()
    root = receipts["go"]
    for row in rows:
        if not isinstance(row, dict) or not valid_id(row.get("id"), 16) or row["id"] in seen:
            raise QualificationError("observation identity is missing or duplicated")
        seen.add(row["id"])
        if row.get("traceId") != root["trace_id"] or row.get("projectId") != project or row.get("type") != "SPAN":
            raise QualificationError("observation trace, project or ordinary span type differs")
        if not row.get("startTime") or not row.get("endTime"):
            raise QualificationError("persisted spans must be completed")
        metadata = row.get("metadata")
        if (
            not isinstance(metadata, dict)
            or not isinstance(metadata.get("resourceAttributes.service.name"), str)
            or not isinstance(metadata.get("attributes.run.id"), str)
        ):
            raise QualificationError("required resource or span metadata is absent")
        service = metadata["resourceAttributes.service.name"]
        key = (service, row.get("name"))
        if key in indexed or service not in SERVICES.values():
            raise QualificationError("unexpected or duplicate service/span pair")
        indexed[key] = row
        if "model" not in row or row["model"] not in (None, ""):
            raise QualificationError("ordinary spans must not invent a model")
        for field in ("input", "output"):
            if row.get(field) not in (None, "", "null", {}, []):
                raise QualificationError("ordinary spans unexpectedly contain input or output")
        for field in ("usageDetails", "costDetails"):
            values = row.get(field, {})
            if not isinstance(values, dict) or any(
                type(value) not in (int, float) or not math.isfinite(value) or value != 0 for value in values.values()
            ):
                raise QualificationError("ordinary spans contain nonzero or invalid usage/cost")
        if row.get("totalCost") is not None and (type(row["totalCost"]) not in (int, float) or row["totalCost"] != 0):
            raise QualificationError("ordinary spans contain nonzero or invalid total cost")
    expected = {(SERVICES["go"], "run")}
    expected.update(
        (SERVICES[language], name)
        for language in ("python", "typescript")
        for name in ("runkite.run", "runkite.llm", "runkite.tool")
    )
    if set(indexed) != expected:
        raise QualificationError("the exact seven-span helper tree was not persisted")
    go_row = indexed[(SERVICES["go"], "run")]
    if go_row["id"] != root["span_id"] or go_row.get("parentObservationId") is not None:
        raise QualificationError("Go receipt identity or root parent differs")
    required = {
        "run.id": "synthetic-run",
        "thread.id": "synthetic-thread",
        "graph.id": "synthetic-agent",
        "run.status": "success",
    }
    for key, value in required.items():
        if go_row["metadata"].get("attributes." + key) != value:
            raise QualificationError("Go operational metadata differs")
    for language in ("python", "typescript"):
        run, llm, tool = [
            indexed[(SERVICES[language], name)] for name in ("runkite.run", "runkite.llm", "runkite.tool")
        ]
        if (
            run["id"] != receipts[language]["span_id"]
            or run.get("parentObservationId") != go_row["id"]
            or llm.get("parentObservationId") != run["id"]
            or tool.get("parentObservationId") != llm["id"]
        ):
            raise QualificationError("runner or callback parentage differs from its receipts")
        run_id = f"synthetic-langfuse-{language}-run"
        expected_attributes = {
            "run.id": run_id,
            "thread.id": f"synthetic-langfuse-{language}-thread",
            "graph.id": "synthetic-langfuse-agent",
            "tenant.id": "synthetic-langfuse-tenant",
            "correlation.id": "synthetic-langfuse-correlation",
            "run.status": "success",
        }
        if any(run["metadata"].get("attributes." + key) != value for key, value in expected_attributes.items()):
            raise QualificationError("runner operational metadata differs")
        for row, key, value in (
            (llm, "llm.name", "synthetic-langfuse-model"),
            (tool, "tool.name", "synthetic-langfuse-tool"),
        ):
            metadata = row["metadata"]
            if metadata.get("attributes.run.id") != run_id or metadata.get("attributes." + key) != value:
                raise QualificationError("callback operational metadata differs")
    return True


def qualify(args, source=None, repository=None) -> dict:
    source = dict(os.environ if source is None else source)
    repository = Path(__file__).resolve().parents[2] if repository is None else repository
    url = validate_url(args.url)
    headers = parse_headers(source.get("OTEL_EXPORTER_OTLP_TRACES_HEADERS", ""))
    python = Path(args.python)
    output = Path(args.output)
    if not python.is_absolute() or not python.is_file() or not os.access(python, os.X_OK):
        raise QualificationError("Python must be an absolute existing executable")
    if not output.is_absolute() or not output.parent.is_dir() or os.path.lexists(output):
        raise QualificationError("output must be a new absolute file in an existing directory")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,127}", args.project_id) or args.expected_version != VERSION:
        raise QualificationError("a synthetic project ID and supported expected backend version are required")
    deadline = time.monotonic() + 120
    status, health = request_json(url, "/api/public/health", {}, deadline)
    if status != 200 or not isinstance(health, dict) or health.get("version") != args.expected_version:
        raise QualificationError("healthy backend version does not match the pinned qualification")
    with tempfile.TemporaryDirectory(prefix="langfuse-sdk-qualification-", dir=output.parent) as temporary:
        directory = Path(temporary)
        environment = base_environment(source, directory)
        tools = {name: shutil.which(name, path=environment.get("PATH", os.defpath)) for name in ("go", "node")}
        if not all(tools.values()):
            raise QualificationError("prepare installed Go and Node executables first")
        versions = collect_versions(repository, str(python), tools, environment, deadline)
        receipts = {}
        encoded_headers = ",".join(f"{key}={quote(value, safe='')}" for key, value in headers.items())
        for language in ("go", "python", "typescript"):
            receipt_path = directory / f"{language}.json"
            child_environment = {
                **environment,
                "RUNKITE_TEST_OTLP_EMIT": "1",
                "RUNKITE_TEST_OTLP_RECEIPT": str(receipt_path),
                "OTEL_SERVICE_NAME": SERVICES[language],
                "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": url + "/api/public/otel/v1/traces",
                "OTEL_EXPORTER_OTLP_TRACES_HEADERS": encoded_headers,
                "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "http/json" if language == "typescript" else "http/protobuf",
                "OTEL_EXPORTER_OTLP_TRACES_TIMEOUT": "3" if language == "python" else "3000",
                "OTEL_BSP_EXPORT_TIMEOUT": "5000",
                "OTEL_BSP_SCHEDULE_DELAY": "60000",
            }
            if language == "go":
                command = [
                    tools["go"],
                    "test",
                    "-mod=readonly",
                    "./internal/tracing",
                    "-run",
                    "^TestOTLPHTTPEmitHelper$",
                    "-count=1",
                    "-timeout=30s",
                ]
            else:
                child_environment["RUNKITE_TEST_OTLP_PARENT"] = receipts["go"]["traceparent"]
                command = (
                    [str(python), "-I", "-B", str(repository / "python/tests/fixtures/tracing_otlp_emit.py")]
                    if language == "python"
                    else [tools["node"], str(repository / "typescript/runkite-runner/src/fixtures/tracingOtlpEmit.mjs")]
                )
            run_child(
                command, repository, child_environment, deadline, 45 if language == "go" else 30, f"{language} emitter"
            )
            receipts[language] = load_receipt(receipt_path, language)
            if receipts[language]["trace_id"] != receipts["go"]["trace_id"] or len(
                {receipt["span_id"] for receipt in receipts.values()}
            ) != len(receipts):
                raise QualificationError("emitter receipts do not identify distinct spans in one trace")
        path = "/api/public/v2/observations?" + urlencode(
            {
                "traceId": receipts["go"]["trace_id"],
                "fields": "basic,time,metadata,io,model,usage",
                "limit": "100",
            }
        )
        public, secret = basic_credentials(headers["authorization"])
        invalid = secrets.token_urlsafe(24)
        while invalid == secret:
            invalid = secrets.token_urlsafe(24)
        invalid_headers = {
            **headers,
            "authorization": "Basic " + base64.b64encode(f"{public}:{invalid}".encode("ascii")).decode("ascii"),
        }
        for denied_headers in ({"x-langfuse-ingestion-version": "4"}, invalid_headers):
            status, _ = request_json(url, path, denied_headers, deadline)
            if status not in (401, 403):
                raise QualificationError("backend did not reject the missing/invalid authentication probe")
        while True:
            status, payload = request_json(url, path, headers, deadline)
            if status == 200 and validate_observations(payload, receipts, args.project_id):
                break
            if status != 200 and status not in (429, 500, 502, 503, 504):
                raise QualificationError("backend observation API rejected the configured read mode")
            time.sleep(remaining(deadline, 2))
    summary = {
        "schema_version": 1,
        "passed": True,
        "backend_version": health["version"],
        "sdk_versions": versions,
        "services": 3,
        "observations": 7,
        "span_observations": 7,
        "generation_observations": 0,
        "auth_denial_probes": 2,
        "model_calls": 0,
    }
    descriptor = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as receipt_file:
        json.dump(summary, receipt_file, sort_keys=True, separators=(",", ":"))
        receipt_file.write("\n")
    return summary


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", required=True)
    parser.add_argument("--python", required=True)
    parser.add_argument(
        "--project-id", required=True, help="fresh synthetic project provisioned by the local fixture owner"
    )
    parser.add_argument("--output", required=True)
    parser.add_argument("--expected-version", default=VERSION)
    args = parser.parse_args(argv)
    try:
        qualify(args)
    except QualificationError as error:
        print(f"Langfuse SDK/span qualification failed: {error}.", file=sys.stderr)
        return 1
    except Exception:
        print(
            "Langfuse SDK/span qualification failed. Check local prerequisites, pinned backend mode, private authentication and the bounded fixture contract.",
            file=sys.stderr,
        )
        return 1
    print(
        "Langfuse SDK/span qualification passed: seven ordinary spans across three services; two authentication denials."
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
