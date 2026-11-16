#!/usr/bin/env python3
"""Run the provider-free native Langflow qualification with bounded cleanup."""

from __future__ import annotations

import argparse
import importlib.util
import json
import os
from pathlib import Path


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--python", type=Path, required=True, help="Interpreter with this example's pinned native client requirements")
    parser.add_argument("--runner-python", type=Path, required=True, help="Separate interpreter with existing Runkite LangGraph runner dependencies")
    parser.add_argument("--output", type=Path, required=True, help="New JSON qualification receipt path")
    parser.add_argument("--scoped-source", action="store_true", help="Require the unreleased scoped MCP grant feature in the tested checkout")
    args = parser.parse_args()
    repository = Path(__file__).resolve().parents[2]
    interpreters = [Path(os.path.abspath(p)) for p in (args.python, args.runner_python)]
    if any(not p.is_file() or not os.access(p, os.X_OK) for p in interpreters):
        parser.error("both Python paths must be existing executables")
    output = args.output.resolve()
    log = output.with_suffix(output.suffix + ".test.log")
    if output.exists() or log.exists():
        parser.error("choose new receipt and log paths; existing evidence is never replaced")
    output.parent.mkdir(parents=True, exist_ok=True)
    # The checkout's existing supervisor is already regression-tested for
    # process-group cleanup and strips ambient model/deployment credentials.
    source = repository / "examples/trinity_specialist_mcp/qualify.py"
    spec = importlib.util.spec_from_file_location("runkite_qualification_supervisor", source)
    supervisor = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(supervisor)
    env = supervisor.qualification_environment(repository)
    env.update(RUNKITE_TEST_LANGFLOW_MCP="1", RUNKITE_TEST_LANGFLOW_PYTHON=str(interpreters[0]),
               RUNKITE_TEST_LANGFLOW_RUNNER_PYTHON=str(interpreters[1]), RUNKITE_TEST_LANGFLOW_OUTPUT=str(output))
    if args.scoped_source:
        env["RUNKITE_TEST_LANGFLOW_SCOPED"] = "1"
    with log.open("x") as stream:
        result = supervisor.run_owned(
            ["go", "test", "./internal/api", "-run", "^TestLangflowMCPQualification$", "-count=1", "-v", "-timeout=2m"],
            cwd=repository, env=env, stdout=stream, timeout=145,
        )
    if result.returncode:
        print(f"Qualification failed; see {log}")
        return result.returncode
    if not output.is_file() or json.loads(output.read_text()).get("passed") is not True:
        raise RuntimeError("qualification did not produce a passing receipt")
    print(f"Native headless Langflow qualification passed: {output}")
    print("Scope: installed native lfx client and tool conversion, real local plane and runner. No UI or model loop.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
