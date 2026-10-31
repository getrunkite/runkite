#!/usr/bin/env python3
"""Bounded recovery checks for a disposable local multi-control-plane stack.

This supplements the steady create/wait soak. It uses the existing echo and
slow agents, durable HTTP records, retained SSE, and the webhook sink's JSONL.
Only the explicitly named Compose project is mutated. Services are restored;
the parent process remains responsible for removing its disposable stack.
Publish replica HTTP ports with explicit fixed host port numbers. Docker may
reassign an automatically allocated host port after a container stop/start.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import os
import re
import subprocess
import sys
import time
import traceback
import urllib.error
import urllib.parse
import urllib.request
import uuid
from collections.abc import Callable
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

ATTEMPT = "_runkite_attempt_generation"
COMPLETION = "_runkite_completion_generation"
TERMINAL = {"success", "error", "timeout", "interrupted"}


def timestamp() -> str:
    return datetime.now(timezone.utc).isoformat()


def segment(value: str) -> str:
    return urllib.parse.quote(value, safe="")


def require(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Recovery:
    def __init__(self, args: argparse.Namespace):
        self.args = args
        self.output = args.output.resolve()
        self.output.mkdir(parents=True, exist_ok=True)
        self.webhooks = args.webhook_log.resolve()
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
        self.key = os.environ.get(args.api_key_env, "")
        self.admin_key = os.environ.get(args.admin_key_env, "") or self.key
        self.prefix = "ha-recovery-" + uuid.uuid4().hex
        self.stopped: set[str] = set()
        self.container_ids: dict[str, str] = {}
        self.report: dict[str, Any] = {
            "schema_version": 1,
            "started_at": timestamp(),
            "status": "running",
            "configuration": {
                "url": args.url,
                "replica_urls": args.replica_url,
                "compose_project": args.project,
                "compose_file": [str(p.resolve()) for p in args.compose_file],
                "runner_services": args.runner_service,
                "victim_service": args.victim_service,
                "case_timeout_seconds": args.timeout,
                "settle_seconds": args.settle_seconds,
                "cache_agent": args.cache_agent,
                "webhook_log": str(self.webhooks),
            },
            "scope": {
                "model": "existing deterministic example agents",
                "stream": "full retained raw SSE replay, not managed UI or incremental cursor resume",
                "faults": "runner and one control-plane process loss; Postgres/Redis remain available",
                "non_claims": ["exactly-once external side effects", "live provider billing", "cloud or multi-day HA"],
            },
            "cases": [],
            "compose_commands": [],
            "docker_commands": [],
            "fault_checks": [],
        }

    def save(self) -> None:
        (self.output / "recovery-report.json").write_text(json.dumps(self.report, indent=2) + "\n")

    def request(self, base: str, path: str, body: Any = None, method: str | None = None) -> Any:
        data = None if body is None else json.dumps(body, separators=(",", ":"), ensure_ascii=False).encode()
        headers = {"Content-Type": "application/json"}
        credential = self.admin_key if path.startswith("/admin-api/") else self.key
        if credential:
            headers["Authorization"] = "Bearer " + credential
        req = urllib.request.Request(base.rstrip("/") + path, data=data, headers=headers, method=method)
        try:
            with self.opener.open(req, timeout=self.args.request_timeout) as response:
                raw = response.read((2 << 20) + 1)
                require(len(raw) <= 2 << 20, "HTTP response exceeded the recovery harness bound")
                return json.loads(raw) if raw else None
        except urllib.error.HTTPError as error:
            message = error.read(4096).decode("utf-8", "replace")
            raise RuntimeError(f"{req.get_method()} {path}: HTTP {error.code}: {message}") from error

    def poll(self, description: str, operation: Callable[[], Any], timeout: float | None = None) -> Any:
        deadline = time.monotonic() + (timeout or self.args.timeout)
        last_error = ""
        while time.monotonic() < deadline:
            try:
                result = operation()
                if result:
                    return result
            except (OSError, RuntimeError) as error:
                last_error = str(error)
            time.sleep(0.2)
        raise TimeoutError(f"Timed out waiting for {description}; last transport error: {last_error}")

    def run_path(self, thread: str, run: str) -> str:
        return f"/threads/{segment(thread)}/runs/{segment(run)}"

    def create_thread(self, base: str, name: str, values: dict[str, Any] | None = None) -> str:
        thread = self.prefix + "-" + name
        result = self.request(base, "/threads", {"thread_id": thread})
        require(result["thread_id"] == thread, "Thread creation changed the supplied identity")
        if values is not None:
            self.request(base, f"/threads/{segment(thread)}/state", {"values": values})
        return thread

    def create_run(self, base: str, thread: str, name: str, agent: str, content: str) -> tuple[str, dict]:
        run = self.prefix + "-" + name
        body = {
            "run_id": run,
            "agent_id": agent,
            "input": {"messages": [{"role": "human", "content": content}]},
            "stream_mode": ["values", "updates"],
        }
        result = self.request(base, f"/threads/{segment(thread)}/runs", body)
        require(result["run_id"] == run, "Run creation changed the supplied identity")
        return run, body

    def get_run(self, base: str, thread: str, run: str) -> dict:
        return self.request(base, self.run_path(thread, run))

    def terminal(self, base: str, thread: str, run: str, status: str) -> dict:
        def finished():
            current = self.get_run(base, thread, run)
            if current["status"] in TERMINAL:
                require(current["status"] == status, f"Run {run} ended as {current['status']}: {current.get('error')}")
                return current
            return None

        return self.poll(f"{run} durable {status}", finished)

    def running(self, base: str, thread: str, run: str) -> dict:
        def active():
            current = self.get_run(base, thread, run)
            require(current["status"] not in TERMINAL, f"Run finished before fault injection: {current}")
            metadata = current.get("metadata", {})
            return current if current["status"] == "running" and ATTEMPT in metadata else None

        return self.poll(f"{run} dispatched", active)

    def hook_events(self, run: str, kind: str) -> list[dict]:
        events = []
        if not self.webhooks.exists():
            return events
        for line in self.webhooks.read_text().splitlines():
            try:
                row = json.loads(line)
            except json.JSONDecodeError:
                continue  # The sink may be appending its last row right now.
            event = row.get("event", {})
            if event.get("run_id") == run and event.get("type") == kind:
                events.append(event)
        return events

    def wait_hook(self, run: str, kind: str) -> list[dict]:
        return self.poll(f"{kind} webhook for {run}", lambda: self.hook_events(run, kind))

    def compose(self, *arguments: str) -> str:
        command = ["docker", "compose", "--project-name", self.args.project]
        for path in self.args.compose_file:
            command.extend(["-f", str(path.resolve())])
        command.extend(arguments)
        started = time.monotonic()
        result = subprocess.run(command, text=True, capture_output=True, timeout=60)
        self.report["compose_commands"].append(
            {
                "arguments": list(arguments),
                "at": timestamp(),
                "returncode": result.returncode,
                "duration_seconds": round(time.monotonic() - started, 3),
                "stdout": result.stdout[-8192:],
                "stderr": result.stderr[-8192:],
            }
        )
        self.save()
        require(result.returncode == 0, f"Explicit project Compose command failed: {arguments}: {result.stderr}")
        return result.stdout

    def stop_hard(self, services: list[str]) -> None:
        containers = [self.service_container(service)[0] for service in services]
        self.stopped.update(services)  # Restoration also runs after a partial failure.
        self.docker("kill", "--signal", "SIGKILL", *containers)

    def restore(self, services: list[str]) -> None:
        containers = [self.service_container(service)[0] for service in services]
        # Compose start may expand dependencies and silently restore the CP
        # whose outage this scenario must preserve. Start only verified IDs.
        self.docker("start", *containers)
        self.stopped.difference_update(services)

    def restart(self, services: list[str]) -> None:
        containers = [self.service_container(service)[0] for service in services]
        self.stopped.update(services)
        self.docker("restart", "--time", "5", *containers)
        self.stopped.difference_update(services)

    def docker(self, *arguments: str) -> str:
        started = time.monotonic()
        result = subprocess.run(["docker", *arguments], text=True, capture_output=True, timeout=60)
        self.report["docker_commands"].append(
            {
                "arguments": list(arguments),
                "at": timestamp(),
                "returncode": result.returncode,
                "duration_seconds": round(time.monotonic() - started, 3),
                "stdout": result.stdout[-8192:],
                "stderr": result.stderr[-8192:],
            }
        )
        self.save()
        require(result.returncode == 0, f"Exact-container Docker command failed: {arguments}: {result.stderr}")
        return result.stdout

    def service_container(self, service: str) -> tuple[str, dict]:
        ids = self.compose("ps", "--all", "-q", service).splitlines()
        require(len(ids) == 1, f"Expected one explicit project container for {service}")
        container = ids[0]
        require(
            self.container_ids.get(service, container) == container,
            f"Recovery container for {service} was unexpectedly recreated",
        )
        # Never record full inspect output: Config.Env can hold credentials.
        template = (
            '{"project":{{json (index .Config.Labels "com.docker.compose.project")}},'
            '"service":{{json (index .Config.Labels "com.docker.compose.service")}},'
            '"running":{{json .State.Running}},'
            '"port_bindings":{{json .HostConfig.PortBindings}}}'
        )
        state = json.loads(self.docker("inspect", "--format", template, container))
        require(
            state["project"] == self.args.project and state["service"] == service,
            "Container does not match the explicit recovery project and service",
        )
        self.container_ids[service] = container
        return container, state

    def assert_victim_stopped(self, phase: str) -> dict:
        container, state = self.service_container(self.args.victim_service)
        check = {
            "at": timestamp(),
            "phase": phase,
            "service": self.args.victim_service,
            "container": container,
            "running": state["running"],
        }
        self.report["fault_checks"].append(check)
        self.save()
        require(state["running"] is False, f"Victim CP was restored during {phase}")
        return check

    def healthy(self, base: str) -> Any:
        return self.request(base, "/readyz")

    def wait_ready(self) -> None:
        for base in [self.args.url, *self.args.replica_url]:
            self.poll(f"{base} ready", lambda base=base: self.healthy(base))

    def verify_project(self) -> None:
        services = set(self.compose("config", "--services").splitlines())
        require(
            set(self.args.runner_service + [self.args.victim_service]) <= services, "Required recovery services absent"
        )
        for service in self.args.runner_service + [self.args.victim_service]:
            _, state = self.service_container(service)
            require(state["running"] is True, f"Recovery service {service} must initially be running")
            if service == self.args.victim_service:
                bindings = (state.get("port_bindings") or {}).get("2026/tcp") or []
                require(
                    bool(bindings) and all(str(binding.get("HostPort", "")) not in {"", "0"} for binding in bindings),
                    "Victim CP HTTP port must use an explicit fixed published host port, not automatic allocation",
                )
        self.wait_ready()

    def assert_stable(
        self, base: str, thread: str, run: str, expected: dict, values: dict, thread_status: str = "idle"
    ) -> dict:
        deadline = time.monotonic() + self.args.settle_seconds
        while time.monotonic() < deadline:
            current = self.get_run(base, thread, run)
            require(current["status"] == expected["status"], "Terminal status changed after recovery")
            require(current.get("output") == expected.get("output"), "Durable output changed after recovery")
            require(
                current.get("metadata", {}).get(ATTEMPT) == expected.get("metadata", {}).get(ATTEMPT),
                "Terminal attempt resurrected",
            )
            time.sleep(0.5)
        state = self.request(base, f"/threads/{segment(thread)}/state")
        saved = self.request(base, f"/threads/{segment(thread)}")
        require(saved["status"] == thread_status, f"Thread was not released as {thread_status}: {saved['status']}")
        require(saved.get("values", {}) == values, "Unexpected persisted thread values")
        require(state.get("values", {}) == values, "Latest checkpoint does not match durable thread state")
        return {"thread": saved, "latest_state": state}

    def cache_case(self) -> dict:
        first, second, third = self.args.replica_url
        before_usage = self.usage_totals(third, self.args.cache_agent)
        content = self.prefix + " constant cache input"
        warm_thread = self.create_thread(first, "cache-warm")
        warm_run, _ = self.create_run(first, warm_thread, "cache-source", self.args.cache_agent, content)
        warm = self.terminal(second, warm_thread, warm_run, "success")
        require(warm.get("metadata", {}).get("cache_hit") is not True, "Unique warm request unexpectedly hit cache")
        self.wait_hook(warm_run, "run_complete")  # Finalization saves cache before scheduling this hook.
        after_warm_usage = self.usage_totals(third, self.args.cache_agent)
        expected_usage = {"run_count": 1, "tokens_in": 20, "tokens_out": 4}
        require(
            {key: after_warm_usage[key] - before_usage[key] for key in expected_usage} == expected_usage,
            "Synthetic cache source did not settle exactly one 20/4 usage entry",
        )
        marker = {"recovery_marker": self.prefix}
        target = self.create_thread(third, "cache-target", marker)
        run = self.prefix + "-cache-hit"
        body = {
            "run_id": run,
            "agent_id": self.args.cache_agent,
            "input": {"messages": [{"role": "human", "content": content}]},
        }
        path = f"/threads/{segment(target)}/runs"
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as executor:
            futures = [executor.submit(self.request, base, path, body) for base in (first, second)]
            responses = [future.result(timeout=self.args.request_timeout + 2) for future in futures]
        require(
            all(row["run_id"] == run and row["status"] == "success" for row in responses),
            "Concurrent cache retries disagreed",
        )
        self.request(third, path, body)  # Lost-response reconciliation can hit a third replica.
        saved = self.terminal(third, target, run, "success")
        require(saved.get("metadata", {}).get("cache_hit") is True, "Probe did not exercise an actual cache hit")
        require(
            saved.get("metadata", {}).get(COMPLETION) == 0 and saved.get("metadata", {}).get(ATTEMPT, 0) == 0,
            "Cached completion unexpectedly entered a runner attempt",
        )
        require(saved.get("output") == warm.get("output"), "Cache replay changed the source output")
        rows = self.request(third, f"/threads/{segment(target)}/runs")
        require(len(rows) == 1 and rows[0]["run_id"] == run, "Concurrent cached create produced duplicate runs")
        values = {**marker, **warm["output"]}
        state = self.assert_stable(third, target, run, saved, values)
        history = self.request(third, f"/threads/{segment(target)}/history")
        require(history and history[0]["values"] == values, "Cached completion history lost preexisting state")
        hooks = self.wait_hook(run, "run_complete")
        require(
            len(hooks) == 1 and hooks[0].get("data", {}).get("cache_hit") is True,
            "Cached completion hook duplicated or lost marker",
        )
        after_cached_usage = self.usage_totals(third, self.args.cache_agent)
        require(
            after_cached_usage == after_warm_usage, "Cache replay billed provider usage or another usage-ledger run"
        )
        return {
            "source_run": warm,
            "cached_run": saved,
            "concurrent_responses": responses,
            "state": state,
            "history_count": len(history),
            "terminal_hook_count": len(hooks),
            "usage_before_source": before_usage,
            "usage_after_source": after_warm_usage,
            "usage_after_cached_retries": after_cached_usage,
            "billing_scope": "Explicitly synthetic 20/4 metadata, never provider billing.",
        }

    def usage_totals(self, base: str, agent: str) -> dict:
        rows = self.request(base, "/admin-api/usage/summary?" + urllib.parse.urlencode({"agent_id": agent}))
        return {key: sum(row.get(key, 0) for row in rows) for key in ("run_count", "tokens_in", "tokens_out")}

    def cancel_case(self) -> dict:
        first, second, third = self.args.replica_url
        marker = {"recovery_marker": self.prefix + "-cancel"}
        thread = self.create_thread(first, "cancel", marker)
        run, _ = self.create_run(first, thread, "cancel", "slow_agent", "Cancel from another control plane")
        before = self.running(third, thread, run)
        self.request(second, self.run_path(thread, run) + "/cancel", {})
        cancelled = self.terminal(third, thread, run, "interrupted")
        self.restart(self.args.runner_service)
        self.wait_ready()
        state = self.assert_stable(third, thread, run, cancelled, marker, thread_status="interrupted")
        hooks = self.wait_hook(run, "interrupt")
        require(len(hooks) == 1, "Cancellation terminal hook was duplicated")
        return {"before_cancel": before, "after_cancel": cancelled, "state": state, "terminal_hook_count": len(hooks)}

    def reclaim_case(self) -> dict:
        first, second, third = self.args.replica_url
        thread = self.create_thread(first, "reclaim")
        run, _ = self.create_run(first, thread, "reclaim", "slow_agent", "Recover this same run after process loss")
        before = self.running(second, thread, run)
        initial = before["metadata"][ATTEMPT]
        services = [*self.args.runner_service, self.args.victim_service]
        self.stop_hard(services)
        # Keep a CP down while surviving CPs reclaim. Workers restart on their
        # existing containers, so the load balancer's resolved addresses stay valid.
        self.restore(self.args.runner_service)
        after_runner_start = self.assert_victim_stopped("after exact runner start")
        self.poll("surviving CP readiness", lambda: self.healthy(second))
        final = self.terminal(second, thread, run, "success")
        metadata = final.get("metadata", {})
        require(metadata.get(ATTEMPT, -1) > initial, "Run succeeded without exercising reclaim")
        require(metadata.get(COMPLETION) == metadata.get(ATTEMPT), "Completion belongs to a superseded attempt")
        require(final.get("output", {}).get("step") == 3, "Recovered slow agent did not reach its last step")
        after_success = self.assert_victim_stopped("after durable reclaimed success")
        self.restore([self.args.victim_service])
        self.wait_ready()
        state = self.assert_stable(third, thread, run, final, final["output"])
        hooks = self.wait_hook(run, "run_complete")
        require(len(hooks) == 1, "Reclaimed completion hook was duplicated")
        return {
            "before_process_loss": before,
            "after_reclaim": final,
            "state": state,
            "terminal_hook_count": len(hooks),
            "victim_stopped_checks": [after_runner_start, after_success],
        }

    def stream(self, base: str, thread: str, run: str, first_only: bool = False) -> list[dict]:
        headers = {"Accept": "text/event-stream"}
        if self.key:
            headers["Authorization"] = "Bearer " + self.key
        request = urllib.request.Request(base.rstrip("/") + self.run_path(thread, run) + "/stream", headers=headers)
        frames: list[dict] = []
        with self.opener.open(request, timeout=self.args.request_timeout) as response:
            require(response.headers.get_content_type() == "text/event-stream", "Response is not SSE")
            fields: dict[str, str] = {}
            data: list[str] = []
            size = 0
            for raw in response:
                size += len(raw)
                require(size <= 2 << 20, "Retained SSE exceeded the bounded recovery fixture budget")
                line = raw.decode("utf-8").rstrip("\r\n")
                if not line:
                    if data:
                        value = "\n".join(data)
                        frame = {**fields, "data": json.loads(value)}
                        frames.append(frame)
                        if first_only:
                            return frames
                    fields, data = {}, []
                    continue
                field, _, value = line.partition(":")
                if value.startswith(" "):
                    value = value[1:]
                if field == "data":
                    data.append(value)
                elif field in {"id", "event"}:
                    fields[field] = value
        return frames

    def replay_case(self) -> dict:
        first, second, third = self.args.replica_url
        thread = self.create_thread(first, "sse")
        run, _ = self.create_run(first, thread, "sse", "slow_agent", "Replay this run on a different replica")
        before = self.running(second, thread, run)
        partial = self.stream(first, thread, run, first_only=True)
        require(partial, "Did not observe a frame before disconnect")
        final = self.terminal(second, thread, run, "success")
        require(
            final["metadata"][ATTEMPT] == before["metadata"][ATTEMPT], "SSE disconnect unexpectedly reclaimed the run"
        )
        replay = self.stream(third, thread, run)
        require(replay and replay[-1].get("event") == "end", "Cross-replica replay has no terminal event")
        require(replay[-1]["data"].get("status") == "success", "Replay did not confirm durable success")
        require(not any(frame.get("event") == "error" for frame in replay), "Replay included an error")
        generation = final["metadata"][COMPLETION]
        require(generation == final["metadata"][ATTEMPT], "Replay completion belongs to another attempt")
        # worker.py emits run_evt_N. bridge.StreamEvents adds :gN only for
        # generations above 1, so initial and legacy attempts keep plain IDs.
        suffix = f":g{generation}" if generation > 1 else ""
        runner_event_id = re.compile(re.escape(run) + r"_evt_[1-9][0-9]*" + re.escape(suffix))
        for frame in replay:
            if runner_event_id.fullmatch(frame.get("id", "")):
                continue
            require(
                frame.get("id") == run + "_end" and frame.get("event") == "end",
                "Replay included another attempt's event identity",
            )
        values = [frame["data"] for frame in replay if frame.get("event") == "values"]
        require(values and values[-1] == final["output"], "Replay values differ from durable output")
        rows = self.request(third, f"/threads/{segment(thread)}/runs")
        require(len(rows) == 1 and rows[0]["run_id"] == run, "Reattaching created another run")
        (self.output / "replay-frames.json").write_text(json.dumps(replay, indent=2) + "\n")
        return {
            "initial_frames": partial,
            "replay_frame_count": len(replay),
            "durable_run": final,
            "scope": "Disconnected one raw GET stream and replayed retained same-attempt events through another CP.",
        }

    def execute(self) -> int:
        code = 1
        try:
            self.verify_project()
            for name, operation in [
                ("atomic_cached_completion", self.cache_case),
                ("cross_replica_cancel", self.cancel_case),
                ("runner_and_cp_loss_reclaim", self.reclaim_case),
                ("cross_replica_retained_sse", self.replay_case),
            ]:
                case: dict[str, Any] = {"name": name, "started_at": timestamp(), "status": "running"}
                self.report["cases"].append(case)
                self.save()
                start = time.monotonic()
                try:
                    case["evidence"] = operation()
                    case["status"] = "pass"
                except Exception as error:
                    case["status"] = "fail"
                    case["error"] = str(error)
                    raise
                finally:
                    case["duration_seconds"] = round(time.monotonic() - start, 3)
                    case["finished_at"] = timestamp()
                    self.save()
                print(
                    json.dumps({"case": name, "status": "pass", "duration_seconds": case["duration_seconds"]}),
                    flush=True,
                )
            self.report["status"] = "pass"
            code = 0
        except Exception as error:
            self.report["status"] = "fail"
            self.report["error"] = str(error)
            self.report["traceback"] = traceback.format_exc()
            print(str(error), file=sys.stderr, flush=True)
            code = 1
        finally:
            if self.stopped:
                try:
                    self.restore(sorted(self.stopped))
                    self.wait_ready()
                except Exception as error:
                    self.report["status"] = "fail"
                    self.report["restoration_error"] = str(error)
                    code = 1
            self.report["finished_at"] = timestamp()
            self.save()
        return code


def arguments() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", required=True, help="Load-balancer HTTP URL")
    parser.add_argument(
        "--replica-url",
        action="append",
        required=True,
        help="Direct HTTP URL with fixed host port, exactly three replicas",
    )
    parser.add_argument("--project", required=True, help="Explicit disposable Compose project, prefixed runkite-ha-")
    parser.add_argument("--compose-file", type=Path, action="append", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument(
        "--webhook-log", type=Path, required=True, help="Existing webhook sink JSONL from this disposable run"
    )
    parser.add_argument("--runner-service", action="append", default=None)
    parser.add_argument("--victim-service", default="runkite-3")
    parser.add_argument("--timeout", type=float, default=120, help="Per-condition deadline, seconds")
    parser.add_argument("--request-timeout", type=float, default=8)
    parser.add_argument("--settle-seconds", type=float, default=10)
    parser.add_argument(
        "--cache-agent", default="metered_agent", help="Synthetic example agent with 20/4 usage and cache TTL"
    )
    parser.add_argument(
        "--api-key-env", default="SOAK_API_KEY", help="Optional credential environment variable name, never recorded"
    )
    parser.add_argument(
        "--admin-key-env",
        default="SOAK_ADMIN_API_KEY",
        help="Optional Admin key environment name; defaults to the client key",
    )
    args = parser.parse_args()
    args.url = args.url.rstrip("/")
    args.replica_url = [url.rstrip("/") for url in args.replica_url]
    require(
        len(args.replica_url) == 3 and len(set(args.replica_url)) == 3,
        "Exactly three distinct --replica-url values are required",
    )
    require(bool(re.fullmatch(r"runkite-ha-[a-z0-9_-]+", args.project)), "Use a disposable runkite-ha- project name")
    require(all(path.is_file() for path in args.compose_file), "Every Compose file must exist")
    require(
        0 < args.request_timeout <= 30 and 0 < args.timeout <= 300 and 0 < args.settle_seconds <= 60,
        "Timeouts must be positive and bounded",
    )
    args.runner_service = args.runner_service or ["runner-1", "runner-2"]
    require(
        len(set(args.runner_service)) == len(args.runner_service) and args.victim_service not in args.runner_service,
        "Distinct runner/victim services are required",
    )
    for url in [args.url, *args.replica_url]:
        parsed = urllib.parse.urlsplit(url)
        require(
            parsed.scheme == "http"
            and parsed.hostname in {"localhost", "127.0.0.1", "::1"}
            and not parsed.username
            and not parsed.password,
            "Recovery endpoints must be explicit local HTTP URLs",
        )
    return args


if __name__ == "__main__":
    raise SystemExit(Recovery(arguments()).execute())
