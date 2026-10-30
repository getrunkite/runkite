"""Opt-in installed Pydantic AI worker against the real authenticated plane.

The public provider SDK consumes in-process synthetic SSE. No provider network
or credentials are used. Token counts and USD prices are fictional test data.
"""

from __future__ import annotations

import importlib.util
import json
import subprocess
import sys
import unittest
from pathlib import Path

_path = Path(__file__).resolve().parents[1] / "agent_framework_adapter" / "test_e2e.py"
_spec = importlib.util.spec_from_file_location("runkite_pydantic_shared_e2e", _path)
assert _spec is not None and _spec.loader is not None
_base = importlib.util.module_from_spec(_spec)
sys.modules[_spec.name] = _base
_spec.loader.exec_module(_base)
MODEL = "offline-pydantic-e2e"


class ManagedPydanticAIE2E(_base.ManagedAgentFrameworkE2E):
    adapter_package = "pydantic_ai_adapter"
    sdk_module = "pydantic_ai"
    sdk_distribution = "pydantic-ai-slim"
    sdk_version = "2.63.0"
    install_extra = "pydantic-ai"
    runner_command_name = "runkite-runner-pydantic-ai"
    runner_description = "Runkite managed Pydantic AI runner"
    example_directory = "managed_pydantic_ai"
    fixture = Path(__file__).resolve().parent / "e2e_agent.py"
    runner_kind_env = "PYTHON_PYDANTIC_AI"
    counters_env = "RUNKITE_PYDANTIC_E2E_COUNTERS"
    metadata_key = "pydantic_ai"
    fixture_label = "pa"
    extra_graph_factories = (
        "typed_agent",
        "typed_output_only",
        "wide_integer",
        "error_agent",
        "blocked_agent",
        "deny_all_agent",
        "budget_agent",
    )
    extra_worker_environment = {"PYDANTIC_AI_NO_BANNER": "1"}
    extra_test_requirements = ("pydantic-ai-slim[openai]==2.63.0",)

    def check_core_install(self) -> None:
        subprocess.run(
            [
                str(self.consumer_python),
                "-I",
                "-c",
                "import importlib.util; "
                "from pydantic_ai.models.function import FunctionModel; "
                "assert importlib.util.find_spec('openai') is None, 'core extra installed a provider SDK'",
            ],
            env=self.worker_environment,
            cwd=self.directory,
            check=True,
            text=True,
            timeout=20,
        )

    def additional_configuration(self) -> dict:
        return {
            "allowed_tools": {
                self.agent: ["record_effect"],
                self.extra_graphs["typed_agent"]: ["record_effect"],
                self.extra_graphs["typed_output_only"]: [],
                self.extra_graphs["wide_integer"]: [],
                self.extra_graphs["blocked_agent"]: ["a_different_tool"],
                self.extra_graphs["deny_all_agent"]: [],
            },
            "finops": {
                "pricebook": {MODEL: {"input_per_1k": 1, "output_per_1k": 2}},
                "budgets": {
                    "agents": {f"{self.tenant_a}/{self.extra_graphs['budget_agent']}": {"max_usd_per_day": 0.02}}
                },
            },
        }

    def final_usage(self, run: dict, prompt: int, completion: int) -> None:
        super().final_usage(run, prompt, completion)
        self.assertEqual(run["output"]["usage"]["model"], MODEL)

    def assert_typed_result(self, run: dict, prompt: int, completion: int) -> None:
        self.final_usage(run, prompt, completion)
        self.assertEqual(run["output"]["result"], {"count": 7, "note": "validated", "tenant": self.tenant_a})
        self.assertIs(type(run["output"]["result"]["count"]), int)
        self.assertTrue(all(message["role"] != "assistant" for message in run["output"]["messages"]))

    def test_managed_lifecycle(self) -> None:
        super().test_managed_lifecycle()
        scenario = "provider error and cancel preserve model-aware spend"
        try:
            self.expect_cost(self.agent, 0.112)
            self.expect_cost(self.slow, 0.018)
            stalled = next(
                json.loads(line)
                for line in self.counters.read_text().splitlines()
                if json.loads(line)["kind"] == "stalled"
            )
            canceled = self.request(f"/threads/{stalled['thread_id']}/runs/{stalled['run_id']}")
            self.assertEqual(canceled["output"]["usage"]["model"], MODEL)
            error_graph = self.extra_graphs["error_agent"]
            error_thread = self.new_thread()
            error_run = self.new_run(error_thread, "error-after-first-tool", agent=error_graph)["run_id"]
            error = self.completed(error_thread, error_run)
            self.assertEqual(error["status"], "error", json.dumps(error))
            self.assertEqual(error["output"]["usage"]["model"], MODEL)
            self.assertEqual(
                (error["output"]["usage"]["prompt_tokens"], error["output"]["usage"]["completion_tokens"]), (10, 4)
            )
            self.assertEqual(len(self.records(error_run, "factory")), 1)
            self.assertEqual(len(self.records(error_run, "model")), 2)
            self.assertEqual(len(self.records(error_run, "tool")), 1)
            self.assertEqual(len(self.records(error_run, "failed")), 1)
            self.expect_usage(error_graph, (1, 10, 4))
            self.expect_cost(error_graph, 0.018)
            self.request(f"/threads/{error_thread}/runs/{error_run}/wait")
            self.expect_usage(error_graph, (1, 10, 4))
            self.expect_cost(error_graph, 0.018)
            print(f"PASS {self.profile}: Pydantic cancel/error usage and fictional USD settle once", flush=True)

            scenario = "validated typed result survives crash after state commit"
            typed_graph = self.extra_graphs["typed_agent"]
            typed_thread = self.new_thread()
            self.runner.stop()
            proxy, committed, outcome = self.crash_after_commit_proxy(typed_thread)
            self.start_runner(proxy)
            typed_run = self.new_run(typed_thread, "typed-first-turn", agent=typed_graph)["run_id"]
            self.assertTrue(committed.wait(25), "typed state never committed before the worker crash")
            self.assertNotIn("error", outcome)
            self.assertEqual(outcome["run_id"], typed_run)
            self.assertEqual(outcome["receipt"]["completed"]["run_id"], typed_run)
            self.assertEqual(outcome["receipt"]["completed"]["output"]["result"]["count"], 7)
            self.assertNotEqual(self.request(f"/threads/{typed_thread}/runs/{typed_run}")["status"], "success")
            self.start_runner()
            final = self.completed(typed_thread, typed_run)
            self.assert_typed_result(final, 30, 6)
            self.assertGreaterEqual(final["metadata"]["_runkite_completion_generation"], 2)
            self.assertEqual(
                final["output"]["pydantic_ai"]["generation"], final["metadata"]["_runkite_completion_generation"]
            )
            self.assertEqual(
                len(self.records(typed_run, "factory")), 1, "typed receipt replay constructed another agent"
            )
            self.assertEqual(len(self.records(typed_run, "model")), 3, "typed receipt replay repeated model work")
            self.assertEqual(len(self.records(typed_run, "tool")), 1, "typed receipt replay repeated a local effect")
            self.assertTrue(
                self.records(typed_run, "model")[-1]["validation_feedback"],
                "native schema retry feedback did not reach the provider",
            )
            self.assertEqual(self.request(f"/threads/{typed_thread}")["values"], final["output"])
            self.expect_usage(typed_graph, (1, 30, 6))
            self.expect_cost(typed_graph, 0.042)
            self.request(f"/threads/{typed_thread}/runs/{typed_run}/wait")
            self.expect_usage(typed_graph, (1, 30, 6))
            self.expect_cost(typed_graph, 0.042)
            replay = self.http.get(
                f"{self.base}/threads/{typed_thread}/runs/{typed_run}/stream", headers={"Authorization": "Bearer e2e-a"}
            )
            self.assertEqual(replay.status_code, 200)
            self.assertIn("event: values", replay.text)
            self.assertNotIn("event: messages", replay.text, "typed JSON must not become fabricated assistant text")
            print(
                f"PASS {self.profile}: native schema retry and typed receipt replay without repeated work or billing",
                flush=True,
            )

            scenario = "typed native conversation and tool history survive restart"
            self.runner.stop()
            self.start_runner()
            next_run = self.new_run(typed_thread, "typed-second-turn", agent=typed_graph)["run_id"]
            self.assert_typed_result(self.completed(typed_thread, next_run), 30, 6)
            restored = self.records(next_run, "model")[0]
            self.assertEqual(restored["users"], ["typed-first-turn", "typed-second-turn"])
            self.assertGreaterEqual(restored["tool_results"], 2)
            self.assertEqual(len(self.records(next_run, "factory")), 1)
            self.assertEqual(len(self.records(next_run, "model")), 3)
            self.assertEqual(len(self.records(next_run, "tool")), 1)
            self.expect_usage(typed_graph, (2, 60, 12))
            self.expect_cost(typed_graph, 0.084)
            print(f"PASS {self.profile}: typed native conversation and local tool history survive restart", flush=True)

            scenario = "deny-all applies to local effects while synthetic output tools remain valid"
            for name in ("blocked_agent", "deny_all_agent"):
                graph = self.extra_graphs[name]
                thread = self.new_thread()
                run_id = self.new_run(thread, f"do-not-execute-{name}", agent=graph)["run_id"]
                denied = self.completed(thread, run_id)
                self.assertEqual(denied["status"], "error", json.dumps(denied))
                self.assertEqual(len(self.records(run_id, "model")), 1)
                self.assertFalse(self.records(run_id, "tool"), "denied local function performed its effect")
                self.expect_usage(graph, (1, 10, 2))
                self.expect_cost(graph, 0.014)
                events = self.http.get(
                    f"{self.base}/threads/{thread}/runs/{run_id}/stream", headers={"Authorization": "Bearer e2e-a"}
                )
                self.assertEqual(events.status_code, 200)
                self.assertIn("event: tool_auth", events.text)
                self.assertIn("tool_not_allowed", events.text)
            output_graph = self.extra_graphs["typed_output_only"]
            output_thread = self.new_thread()
            output_run = self.new_run(output_thread, "typed-with-no-local-tools", agent=output_graph)["run_id"]
            self.assert_typed_result(self.completed(output_thread, output_run), 20, 4)
            self.assertEqual(len(self.records(output_run, "model")), 2)
            self.assertFalse(self.records(output_run, "tool"))
            self.expect_usage(output_graph, (1, 20, 4))
            self.expect_cost(output_graph, 0.028)
            print(f"PASS {self.profile}: local deny-all blocks effects while typed output validation works", flush=True)

            scenario = "configured USD hard cap sees settled Pydantic usage"
            budget_graph = self.extra_graphs["budget_agent"]
            configured = self.request("/admin-api/finops", key="e2e-admin")["effective"]
            self.assertEqual(
                configured["budgets"]["agents"][f"{self.tenant_a}/{budget_graph}"]["max_usd_per_day"], 0.02
            )
            budget_thread = self.new_thread()
            paid = self.new_run(budget_thread, "spend-before-hard-cap", agent=budget_graph)["run_id"]
            self.final_usage(self.completed(budget_thread, paid), 20, 4)
            self.expect_usage(budget_graph, (1, 20, 4))
            self.expect_cost(budget_graph, 0.028)
            denied_thread = self.new_thread()
            before = self.counters.read_bytes()
            denied = self.request(
                f"/threads/{denied_thread}/runs",
                {"agent_id": budget_graph, "input": {"messages": [{"role": "user", "content": "must-not-execute"}]}},
                status=403,
            )
            self.assertIn("usd budget exceeded", json.dumps(denied).lower())
            self.assertEqual(self.request(f"/threads/{denied_thread}/runs"), [])
            self.assertEqual(self.counters.read_bytes(), before)
            self.expect_usage(budget_graph, (1, 20, 4))
            self.expect_cost(budget_graph, 0.028)
            print(
                f"PASS {self.profile}: accounted Pydantic USD blocks a later create at the configured hard cap",
                flush=True,
            )

            scenario = "wide native integers survive committed receipt recovery and durable API reads"
            wide_graph = self.extra_graphs["wide_integer"]
            wide_thread = self.new_thread()
            self.runner.stop()
            proxy, committed, outcome = self.crash_after_commit_proxy(wide_thread)
            self.start_runner(proxy)
            wide_run = self.new_run(wide_thread, "wide-integer-first-turn", agent=wide_graph)["run_id"]
            self.assertTrue(committed.wait(25), "wide integer state never committed before the worker crash")
            self.assertNotIn("error", outcome)
            expected = {
                "count": 9007199254740993,
                "note": "validated",
                "tenant": self.tenant_a,
                "nested": {"ids": [18446744073709551615, -9223372036854775808]},
            }
            self.assertEqual(outcome["receipt"]["completed"]["run_id"], wide_run)
            self.assertEqual(outcome["receipt"]["completed"]["output"]["result"], expected)
            self.assertNotEqual(self.request(f"/threads/{wide_thread}/runs/{wide_run}")["status"], "success")
            self.start_runner()
            wide = self.completed(wide_thread, wide_run)
            self.final_usage(wide, 20, 4)
            self.assertGreaterEqual(wide["metadata"]["_runkite_completion_generation"], 2)
            for output in (
                wide["output"],
                self.request(f"/threads/{wide_thread}/runs/{wide_run}/wait")["values"],
                self.request(f"/admin-api/runs/{wide_run}", key="e2e-admin")["output"],
                self.request(f"/threads/{wide_thread}")["values"],
                self.request(f"/threads/{wide_thread}/state")["values"],
                self.request(f"/threads/{wide_thread}/history")[0]["values"],
            ):
                self.assertEqual(output["result"], expected)
                self.assertIs(type(output["result"]["count"]), int)
                self.assertTrue(all(type(value) is int for value in output["result"]["nested"]["ids"]))
                self.assertTrue(all(message["role"] != "assistant" for message in output["messages"]))
            self.assertEqual(len(self.records(wide_run, "factory")), 1)
            self.assertEqual(len(self.records(wide_run, "model")), 2)
            self.assertFalse(self.records(wide_run, "tool"))
            self.expect_usage(wide_graph, (1, 20, 4))
            self.expect_cost(wide_graph, 0.028)
            self.request(f"/threads/{wide_thread}/runs/{wide_run}/wait")
            self.expect_usage(wide_graph, (1, 20, 4))
            self.expect_cost(wide_graph, 0.028)
            print(
                f"PASS {self.profile}: native wide integer output survives receipt recovery and API state without rebilling",
                flush=True,
            )
        except Exception:
            print(f"FAILED scenario: {scenario}", file=sys.stderr)
            for process in self.processes:
                print(process.logs(), file=sys.stderr)
            raise


if __name__ == "__main__":
    unittest.main()
