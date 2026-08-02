#!/usr/bin/env python3
"""Live Gemini N×N matrix: frameworks × backends × scenarios.

Requires:
  - repo-root .env.llm with GOOGLE_API_KEY (gitignored)
  - built `./runkite` binary (built automatically if missing)
  - python/.venv (+ adapter venvs for crewai/llamaindex/autogen)
  - for non-sqlite backends: `make infra-up` + matching DSNs

Usage:
  set -a && source .env.llm && set +a
  python3 bench/llm/run_matrix.py
  python3 bench/llm/run_matrix.py --backends sqlite_inprocess --frameworks python-langgraph
  python3 bench/llm/run_matrix.py --dry-run

Results land in bench/llm/out/<timestamp>/ (gitignored).
"""

from __future__ import annotations

import argparse
import json
import os
import signal
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
from dataclasses import asdict, dataclass
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "bench" / "llm"))
from budget import Budget  # noqa: E402


def load_dotenv_llm() -> None:
    path = ROOT / ".env.llm"
    if not path.is_file():
        return
    for line in path.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, _, v = line.partition("=")
        k, v = k.strip(), v.strip().strip('"').strip("'")
        if k and k not in os.environ:
            os.environ[k] = v


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


def http_json(method: str, url: str, body: dict | None = None, headers: dict | None = None, timeout: float = 120.0):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        raw = resp.read()
        if not raw:
            return resp.status, None
        return resp.status, json.loads(raw.decode())


def wait_http(url: str, timeout: float = 60.0) -> None:
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=2) as resp:
                if resp.status < 500:
                    return
        except Exception as e:  # noqa: BLE001
            last = e
            time.sleep(0.25)
    raise RuntimeError(f"timeout waiting for {url}: {last}")


@dataclass
class CellResult:
    backend: str
    framework: str
    scenario: str
    status: str  # pass|fail|skip
    detail: str = ""
    duration_s: float = 0.0
    run_status: str = ""


# Backend env builders — blank higher-priority selectors like the Go matrix.
BACKENDS = {
    "sqlite_inprocess": lambda: {
        "DATABASE_PATH": ":memory:",
        "POSTGRES_DSN": "",
        "MYSQL_DSN": "",
        "MONGO_URI": "",
        "REDIS_URL": "",
        "NATS_URL": "",
        "KAFKA_URL": "",
    },
    "postgres_redis": lambda: _require(
        {
            "POSTGRES_DSN": os.environ.get(
                "POSTGRES_DSN",
                "postgres://runkite:runkite@localhost:5433/runkite_test?sslmode=disable",
            ),
            "REDIS_URL": os.environ.get("REDIS_URL", "redis://localhost:6380"),
            "MYSQL_DSN": "",
            "MONGO_URI": "",
            "NATS_URL": "",
            "KAFKA_URL": "",
            "DATABASE_PATH": "",
        },
        ("POSTGRES_DSN", "REDIS_URL"),
    ),
    "mysql_inprocess": lambda: _require(
        {
            "MYSQL_DSN": os.environ.get(
                "MYSQL_DSN",
                "runkite:runkite@tcp(127.0.0.1:3307)/runkite_test?parseTime=true",
            ),
            "POSTGRES_DSN": "",
            "MONGO_URI": "",
            "REDIS_URL": "",
            "NATS_URL": "",
            "KAFKA_URL": "",
            "DATABASE_PATH": "",
        },
        ("MYSQL_DSN",),
    ),
    "mongo_redis": lambda: _require(
        {
            "MONGO_URI": os.environ.get(
                "MONGO_URI",
                "mongodb://localhost:27018/?replicaSet=rs0&directConnection=true",
            ),
            "REDIS_URL": os.environ.get("REDIS_URL", "redis://localhost:6380"),
            "POSTGRES_DSN": "",
            "MYSQL_DSN": "",
            "NATS_URL": "",
            "KAFKA_URL": "",
            "DATABASE_PATH": "",
        },
        ("MONGO_URI", "REDIS_URL"),
    ),
}


def _require(env: dict, names: tuple[str, ...]) -> dict | str:
    for n in names:
        if not env.get(n):
            return f"{n} not set"
    return env


FRAMEWORKS = {
    "python-langgraph": {
        "config": "examples/gemini/langgraph_agent/langgraph_full.json",
        "agent_id": "gemini_langgraph",
        "cancel_agent_id": "slow_agent",
        "approval_agent_id": "approval_agent",
        "scenarios": ["happy_path", "cancel", "hitl"],
        "module": "runkite_runner",
        "python": "python/.venv/bin/python",
        "extra_pythonpath": "python",
        "venv_check": "python/.venv/bin/python",
    },
    "python-langchain": {
        "config": "examples/gemini/langchain_agent/langgraph.json",
        "agent_id": "gemini_langchain",
        "scenarios": ["happy_path"],
        "module": "langchain_adapter",
        "python": "python/.venv/bin/python",
        "extra_pythonpath": "python:python/adapters",
        "venv_check": "python/.venv/bin/python",
    },
    "python-crewai": {
        "config": "examples/gemini/crewai_agent/langgraph.json",
        "agent_id": "gemini_crewai",
        "scenarios": ["happy_path"],
        "module": "crewai_adapter",
        "python": "python/adapters/crewai_adapter/.venv/bin/python",
        "extra_pythonpath": "python:python/adapters",
        "venv_check": "python/adapters/crewai_adapter/.venv/bin/python",
    },
    "python-llamaindex": {
        "config": "examples/gemini/llamaindex_agent/langgraph.json",
        "agent_id": "gemini_llamaindex",
        "scenarios": ["happy_path"],
        "module": "llamaindex_adapter",
        "python": "python/adapters/llamaindex_adapter/.venv/bin/python",
        "extra_pythonpath": "python:python/adapters",
        "venv_check": "python/adapters/llamaindex_adapter/.venv/bin/python",
    },
    "python-autogen": {
        "config": "examples/gemini/autogen_agent/langgraph.json",
        "agent_id": "gemini_autogen",
        "scenarios": ["happy_path"],
        "module": "autogen_adapter",
        "python": "python/adapters/autogen_adapter/.venv/bin/python",
        "extra_pythonpath": "python:python/adapters",
        "venv_check": "python/adapters/autogen_adapter/.venv/bin/python",
    },
    "typescript-langgraphjs": {
        "config": "examples/gemini/langgraphjs_agent/langgraph.json",
        "agent_id": "gemini_langgraphjs",
        "scenarios": ["happy_path"],
        "ts": True,
        "venv_check": "typescript/runkite-runner/node_modules",
    },
}


def ensure_binary() -> Path:
    bin_path = ROOT / "runkite"
    if bin_path.is_file():
        return bin_path
    print("building ./runkite ...")
    subprocess.check_call(["go", "build", "-o", str(bin_path), "./cmd"], cwd=ROOT)
    return bin_path


def start_cp(bin_path: Path, http_port: int, grpc_port: int, backend_env: dict, config: Path) -> subprocess.Popen:
    env = os.environ.copy()
    env.update({k: str(v) for k, v in backend_env.items()})
    # Same insecure-serve posture as test/e2e/matrix: isolate the
    # framework×backend×LLM seam without production admission gates.
    env["RUNKITE_ALLOW_INSECURE_SERVE"] = "1"
    cmd = [
        str(bin_path),
        "serve",
        "--config",
        str(config),
        "--port",
        str(http_port),
        "--grpc-port",
        str(grpc_port),
    ]
    return subprocess.Popen(
        cmd,
        cwd=ROOT,
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )


def start_runner(fw: dict, grpc_port: int, http_port: int, config: Path) -> subprocess.Popen:
    env = os.environ.copy()
    env["GOOGLE_API_KEY"] = os.environ["GOOGLE_API_KEY"]
    env["GEMINI_MODEL"] = os.environ.get("GEMINI_MODEL", "gemini-2.0-flash")
    env["GEMINI_TEMPERATURE"] = os.environ.get("GEMINI_TEMPERATURE", "0")
    if fw.get("ts"):
        runner_dir = ROOT / "typescript" / "runkite-runner"
        # Example-local node_modules (e.g. @langchain/google-genai) must
        # resolve when the graph is loaded from examples/gemini/... —
        # prepend that path without baking Gemini into the core runner.
        example_nm = str(config.parent / "node_modules")
        runner_nm = str(runner_dir / "node_modules")
        env["NODE_PATH"] = example_nm + os.pathsep + runner_nm + (
            (os.pathsep + env["NODE_PATH"]) if env.get("NODE_PATH") else ""
        )
        cmd = [
            "npx",
            "tsx",
            "src/cli.ts",
            "--config",
            str(config),
            "--grpc-address",
            f"127.0.0.1:{grpc_port}",
            "--http-address",
            f"http://127.0.0.1:{http_port}",
        ]
        return subprocess.Popen(cmd, cwd=runner_dir, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)

    py = ROOT / fw["python"]
    extra = fw["extra_pythonpath"]
    env["PYTHONPATH"] = ":".join(str(ROOT / p) for p in extra.split(":")) + (
        (":" + env["PYTHONPATH"]) if env.get("PYTHONPATH") else ""
    )
    env["PYTHONUNBUFFERED"] = "1"
    # LangGraph runner accepts --http-address; framework adapters only
    # take --config / --grpc-address / --runner-kind / --concurrency.
    cmd = [
        str(py),
        "-m",
        fw["module"],
        "--config",
        str(config),
        "--grpc-address",
        f"127.0.0.1:{grpc_port}",
    ]
    if fw["module"] == "runkite_runner":
        cmd += ["--http-address", f"http://127.0.0.1:{http_port}"]
    return subprocess.Popen(cmd, cwd=ROOT, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)


def kill_proc(proc: subprocess.Popen | None) -> str:
    if proc is None:
        return ""
    if proc.poll() is None:
        proc.send_signal(signal.SIGTERM)
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
    out = ""
    try:
        if proc.stdout:
            out = proc.stdout.read() or ""
    except Exception:
        pass
    return out[-8000:]


def auth_headers() -> dict:
    # Matrix cells run with RUNKITE_ALLOW_INSECURE_SERVE (no auth).
    return {}


def wait_for_agent(base: str, agent_id: str, timeout: float = 60.0) -> None:
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            _, body = http_json(
                "POST",
                f"{base}/agents/search",
                {"name": agent_id, "limit": 100},
                headers=auth_headers(),
            )
            agents = body if isinstance(body, list) else (body or {}).get("agents") or (body or {}).get("items") or []
            for a in agents:
                if isinstance(a, dict) and a.get("agent_id") == agent_id:
                    return
        except Exception as e:  # noqa: BLE001
            last = e
            time.sleep(0.5)
    raise RuntimeError(f"agent {agent_id} never registered: {last}")


def create_thread(base: str) -> str:
    _, body = http_json("POST", f"{base}/threads", {}, headers=auth_headers())
    return body["thread_id"]


def run_stream(base: str, thread_id: str, agent_id: str, input_obj: dict, timeout: float = 180.0) -> tuple[str, list]:
    """POST runs/stream; return (final_status, events)."""
    url = f"{base}/threads/{thread_id}/runs/stream"
    data = json.dumps({"agent_id": agent_id, "input": input_obj}).encode()
    req = urllib.request.Request(url, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    for k, v in auth_headers().items():
        req.add_header(k, v)
    events = []
    status = "unknown"
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        buf = ""
        while True:
            chunk = resp.read(4096)
            if not chunk:
                break
            buf += chunk.decode("utf-8", errors="replace")
            while "\n\n" in buf:
                block, buf = buf.split("\n\n", 1)
                event_name = None
                data_lines = []
                for line in block.splitlines():
                    if line.startswith("event:"):
                        event_name = line[6:].strip()
                    elif line.startswith("data:"):
                        data_lines.append(line[5:].strip())
                if not data_lines:
                    continue
                payload = json.loads("".join(data_lines))
                events.append({"event": event_name, "data": payload})
                if event_name in ("end", "error") and isinstance(payload, dict):
                    status = payload.get("status") or event_name
                elif isinstance(payload, dict) and payload.get("method") in ("end", "error"):
                    status = (payload.get("data") or {}).get("status") or payload.get("method")
    return status, events


def scenario_happy(base: str, agent_id: str) -> tuple[str, str]:
    tid = create_thread(base)
    status, events = run_stream(
        base,
        tid,
        agent_id,
        {"messages": [{"role": "user", "content": f"Say hello in one word. tag={time.time()}"}]},
    )
    if status != "success":
        return "fail", f"run status={status} events={len(events)}"
    return "pass", f"events={len(events)}"


def scenario_cancel(base: str, agent_id: str) -> tuple[str, str]:
    tid = create_thread(base)
    # Non-stream create then cancel
    _, body = http_json(
        "POST",
        f"{base}/threads/{tid}/runs",
        {"agent_id": agent_id, "input": {"messages": [{"role": "user", "content": "go"}], "step": 0}},
        headers=auth_headers(),
    )
    run_id = body["run_id"]
    time.sleep(2)
    try:
        http_json("POST", f"{base}/threads/{tid}/runs/{run_id}/cancel", {}, headers=auth_headers())
    except urllib.error.HTTPError as e:
        return "fail", f"cancel http {e.code}"
    # Poll
    deadline = time.time() + 60
    status = "unknown"
    while time.time() < deadline:
        _, run = http_json("GET", f"{base}/threads/{tid}/runs/{run_id}", headers=auth_headers())
        status = run.get("status", "unknown")
        if status in ("interrupted", "error", "success"):
            break
        time.sleep(0.5)
    if status != "interrupted":
        return "fail", f"expected interrupted got {status}"
    return "pass", f"run={run_id}"


def poll_thread_not_busy(base: str, thread_id: str, timeout: float = 15.0) -> None:
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        _, body = http_json("GET", f"{base}/threads/{thread_id}", headers=auth_headers())
        status = (body or {}).get("status")
        last = status
        if status and status != "busy":
            return
        time.sleep(0.3)
    raise RuntimeError(f"thread {thread_id} still busy after {timeout}s (last={last})")


def scenario_hitl(base: str, agent_id: str) -> tuple[str, str]:
    # Mirror test/e2e/matrix/scenarios.go scenarioHITL — including the
    # pollThreadNotBusy gate before resume (postgres/mongo can still show
    # busy briefly after the interrupt SSE ends; posting resume too early
    # returns 409 Conflict).
    tid = create_thread(base)
    status, events = run_stream(
        base,
        tid,
        agent_id,
        {
            "messages": [{"role": "user", "content": "send the email"}],
            "approved": False,
        },
        timeout=120,
    )
    types = [e.get("event") for e in events]
    if status != "interrupted" and "input.requested" not in types:
        return "fail", f"expected interrupt, status={status} types={types}"
    poll_thread_not_busy(base, tid, timeout=15.0)
    _, result = http_json(
        "POST",
        f"{base}/threads/{tid}/runs/wait",
        {"agent_id": agent_id, "command": {"resume": True}},
        headers=auth_headers(),
        timeout=120,
    )
    resumed = (result or {}).get("run") or {}
    final = resumed.get("status")
    if final != "success":
        return "fail", f"resume status={final} body_keys={list((result or {}).keys())}"
    return "pass", f"interrupted+resume events={len(events)}"


def run_scenario(name: str, base: str, fw: dict) -> tuple[str, str]:
    if name == "happy_path":
        return scenario_happy(base, fw["agent_id"])
    if name == "cancel":
        return scenario_cancel(base, fw["cancel_agent_id"])
    if name == "hitl":
        return scenario_hitl(base, fw["approval_agent_id"])
    return "fail", f"unknown scenario {name}"


def main() -> int:
    load_dotenv_llm()
    ap = argparse.ArgumentParser()
    ap.add_argument("--backends", nargs="*", default=list(BACKENDS))
    ap.add_argument("--frameworks", nargs="*", default=list(FRAMEWORKS))
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    if not os.environ.get("GOOGLE_API_KEY"):
        print("FATAL: GOOGLE_API_KEY missing. Create .env.llm from .env.llm.example", file=sys.stderr)
        return 2

    out_dir = ROOT / "bench" / "llm" / "out" / datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
    out_dir.mkdir(parents=True, exist_ok=True)
    notes = out_dir / "NOTES.md"
    results_path = out_dir / "results.json"
    budget = Budget.from_env()

    notes.write_text(
        f"# LLM matrix notes\n\n"
        f"- started: {datetime.now(timezone.utc).isoformat()}\n"
        f"- model: {os.environ.get('GEMINI_MODEL')}\n"
        f"- budget_usd: {budget.limit_usd}\n"
        f"- backends: {args.backends}\n"
        f"- frameworks: {args.frameworks}\n\n"
        f"Secrets are loaded from `.env.llm` (gitignored). This folder is gitignored.\n"
    )

    if args.dry_run:
        cells = [(b, f, s) for b in args.backends for f in args.frameworks for s in FRAMEWORKS[f]["scenarios"]]
        print(f"dry-run cells={len(cells)}")
        for c in cells:
            print(" ", c)
        return 0

    bin_path = ensure_binary()
    results: list[CellResult] = []

    for backend in args.backends:
        be = BACKENDS[backend]()
        if isinstance(be, str):
            for fw_name in args.frameworks:
                for sc in FRAMEWORKS[fw_name]["scenarios"]:
                    results.append(CellResult(backend, fw_name, sc, "skip", be))
            continue

        for fw_name in args.frameworks:
            fw = FRAMEWORKS[fw_name]
            check = ROOT / fw["venv_check"]
            if not check.exists():
                for sc in fw["scenarios"]:
                    results.append(CellResult(backend, fw_name, sc, "skip", f"missing {fw['venv_check']}"))
                continue

            for sc in fw["scenarios"]:
                if not budget.allow():
                    results.append(CellResult(backend, fw_name, sc, "skip", budget.stop_reason))
                    continue

                label = f"{backend}/{fw_name}/{sc}"
                print(f"==> {label}")
                t0 = time.time()
                http_port, grpc_port = free_port(), free_port()
                config = ROOT / fw["config"]
                cp = runner = None
                detail = ""
                status = "fail"
                run_status = ""
                try:
                    cp = start_cp(bin_path, http_port, grpc_port, be, config)
                    wait_http(f"http://127.0.0.1:{http_port}/health", timeout=90)
                    runner = start_runner(fw, grpc_port, http_port, config)
                    base = f"http://127.0.0.1:{http_port}"
                    # Agents may be pre-registered from langgraph.json; still
                    # wait so SearchAgents is warm, then ensure the runner
                    # process is alive before submitting work.
                    wait_for_agent(base, fw["agent_id"], timeout=fw.get("startup_timeout", 60))
                    time.sleep(2)
                    if runner.poll() is not None:
                        raise RuntimeError(f"runner exited early code={runner.returncode}")
                    print(f"    running {sc} ...", flush=True)
                    status, detail = run_scenario(sc, base, fw)
                    run_status = detail
                    # Charge budget: happy_path hits Gemini; cancel/hitl usually don't.
                    if sc == "happy_path":
                        budget.charge(800, 200, label)
                    else:
                        budget.charge(0, 0, label)
                except Exception as e:  # noqa: BLE001
                    status = "fail"
                    detail = f"{type(e).__name__}: {e}"
                finally:
                    rlog = kill_proc(runner)
                    clog = kill_proc(cp)
                    if status == "fail":
                        (out_dir / f"{backend}__{fw_name}__{sc}.log").write_text(
                            f"DETAIL: {detail}\n\n--- runner ---\n{rlog}\n\n--- cp ---\n{clog}\n"
                        )
                dur = time.time() - t0
                results.append(CellResult(backend, fw_name, sc, status, detail, dur, run_status))
                print(f"    {status} ({dur:.1f}s) {detail}")

    results_path.write_text(json.dumps([asdict(r) for r in results], indent=2) + "\n")
    budget.dump(out_dir / "budget.json")

    passed = sum(1 for r in results if r.status == "pass")
    failed = sum(1 for r in results if r.status == "fail")
    skipped = sum(1 for r in results if r.status == "skip")
    summary = (
        f"\n## Summary\n\n"
        f"- pass: {passed}\n- fail: {failed}\n- skip: {skipped}\n"
        f"- estimated_spend_usd: {budget.spent_usd:.4f} / {budget.limit_usd}\n"
        f"- ended: {datetime.now(timezone.utc).isoformat()}\n"
    )
    notes.write_text(notes.read_text() + summary)
    print(summary)
    print(f"artifacts: {out_dir}")
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
