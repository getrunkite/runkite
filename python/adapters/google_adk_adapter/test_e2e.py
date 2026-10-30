"""Opt-in installed Google ADK worker against the real authenticated plane.

Actual public BaseLlm streaming, Runner, FunctionTool and scoped Session APIs run
with synthetic offline data. All tokens and USD prices are fictional fixtures.
"""

from __future__ import annotations

import importlib.util
import json
import subprocess
import sys
import unittest
from pathlib import Path

_path = Path(__file__).resolve().parents[1] / "agent_framework_adapter" / "test_e2e.py"
_spec = importlib.util.spec_from_file_location("runkite_google_adk_shared_e2e", _path)
assert _spec is not None and _spec.loader is not None
_base = importlib.util.module_from_spec(_spec)
sys.modules[_spec.name] = _base
_spec.loader.exec_module(_base)
MODEL = "offline-google-adk-e2e"


class ManagedGoogleAdkE2E(_base.ManagedAgentFrameworkE2E):
    adapter_package = "google_adk_adapter"
    sdk_module = "google.adk"
    sdk_distribution = "google-adk"
    sdk_version = "2.13.0"
    install_extra = "google-adk"
    runner_command_name = "runkite-runner-google-adk"
    runner_description = "Runkite managed Google ADK runner"
    example_directory = "managed_google_adk"
    fixture = Path(__file__).resolve().parent / "e2e_agent.py"
    runner_kind_env = "PYTHON_GOOGLE_ADK"
    counters_env = "RUNKITE_GOOGLE_ADK_E2E_COUNTERS"
    metadata_key = "google_adk"
    fixture_label = "adk"
    extra_graph_factories = (
        "error_agent",
        "blocked_agent",
        "deny_all_agent",
        "budget_agent",
        "workflow_agent",
        "confirmation_agent",
        "app_state_agent",
        "user_state_agent",
    )

    def check_core_install(self) -> None:
        # All runtime imports must resolve from the actual installed artifact,
        # including the scoped session implementation, without checkout paths.
        subprocess.run(
            [
                str(self.consumer_python),
                "-I",
                "-c",
                "from pathlib import Path; import sys; "
                "from google_adk_adapter import adapter, state, sessions; "
                "assert all(Path(m.__file__).resolve().is_relative_to(Path(sys.prefix).resolve()) "
                "for m in (adapter, state, sessions))",
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
        assistants = [message["content"] for message in run["output"]["messages"] if message["role"] == "assistant"]
        self.assertTrue(assistants)
        for text in assistants:
            self.assertEqual(text.count("users="), 1, "partial and aggregate ADK text were duplicated")
            self.assertEqual(text.count("tool_results="), 1)

    def test_managed_lifecycle(self) -> None:
        super().test_managed_lifecycle()
        scenario = "native session state and partial/final text consistency"
        try:
            records = [json.loads(line) for line in self.counters.read_text().splitlines()]
            effects = {row["text"]: row for row in records if row["kind"] == "tool"}
            self.assertEqual(effects["tenant-a-first-turn"]["prior_effects"], 0)
            self.assertEqual(effects["tenant-a-second-turn"]["prior_effects"], 1)
            self.assertEqual(effects["tenant-b-only-turn"]["prior_effects"], 0)
            self.assertEqual(effects["crash-after-committed-tool"]["prior_effects"], 0)
            self.expect_cost(self.agent, 0.112)
            self.expect_cost(self.slow, 0.018)
            print(
                f"PASS {self.profile}: scoped native state survives restart and aggregate text is deduplicated",
                flush=True,
            )

            scenario = "provider error and cancel retain known model usage and spend"
            stalled = next(row for row in records if row["kind"] == "stalled")
            canceled = self.request(f"/threads/{stalled['thread_id']}/runs/{stalled['run_id']}")
            self.assertEqual(canceled["output"]["usage"]["model"], MODEL)
            error_graph = self.extra_graphs["error_agent"]
            error_thread = self.new_thread()
            error_run = self.new_run(error_thread, "error-after-first-tool", agent=error_graph)["run_id"]
            failed = self.completed(error_thread, error_run)
            self.assertEqual(failed["status"], "error", json.dumps(failed))
            usage = failed["output"]["usage"]
            self.assertEqual((usage["prompt_tokens"], usage["completion_tokens"], usage["model"]), (10, 4, MODEL))
            self.assertEqual(len(self.records(error_run, "factory")), 1)
            self.assertEqual(len(self.records(error_run, "model")), 2)
            self.assertEqual(len(self.records(error_run, "tool")), 1)
            self.assertEqual(len(self.records(error_run, "failed")), 1)
            self.expect_usage(error_graph, (1, 10, 4))
            self.expect_cost(error_graph, 0.018)
            self.request(f"/threads/{error_thread}/runs/{error_run}/wait")
            self.expect_usage(error_graph, (1, 10, 4))
            self.expect_cost(error_graph, 0.018)
            print(f"PASS {self.profile}: ADK error/cancel preserve observed usage and fictional USD once", flush=True)

            scenario = "unsupported workflow and native confirmation fail closed"
            for name in ("workflow_agent", "confirmation_agent"):
                graph = self.extra_graphs[name]
                thread = self.new_thread()
                run_id = self.new_run(thread, f"unsupported-{name}", agent=graph)["run_id"]
                result = self.completed(thread, run_id)
                self.assertEqual(result["status"], "error", json.dumps(result))
                self.assertEqual(len(self.records(run_id, "factory")), 1)
                self.assertEqual(len(self.records(run_id, "model")), 0 if name == "workflow_agent" else 1)
                self.assertFalse(self.records(run_id, "tool"), f"unsupported {name} performed a local effect")
                events = self.http.get(
                    f"{self.base}/threads/{thread}/runs/{run_id}/stream", headers={"Authorization": "Bearer e2e-a"}
                )
                self.assertEqual(events.status_code, 200)
                self.assertIn("event: error", events.text)
            print(
                f"PASS {self.profile}: workflow rejects before model and confirmation rejects before local body",
                flush=True,
            )

            scenario = "shared app and user session state fail closed"
            for name in ("app_state_agent", "user_state_agent"):
                graph = self.extra_graphs[name]
                thread = self.new_thread()
                run_id = self.new_run(thread, f"unsupported-{name}", agent=graph)["run_id"]
                result = self.completed(thread, run_id)
                self.assertEqual(result["status"], "error", json.dumps(result))
                self.assertEqual(len(self.records(run_id, "factory")), 1)
                self.assertEqual(len(self.records(run_id, "model")), 1)
                self.assertEqual(len(self.records(run_id, "unsupported_state_attempt")), 1)
                self.assertFalse(self.records(run_id, "tool"))
                self.expect_usage(graph, (1, 10, 2))
                self.expect_cost(graph, 0.014)
            print(
                f"PASS {self.profile}: cross-session app/user state cannot become a successful checkpoint", flush=True
            )

            scenario = "configured local allowlists deny effects"
            for name in ("blocked_agent", "deny_all_agent"):
                graph = self.extra_graphs[name]
                thread = self.new_thread()
                run_id = self.new_run(thread, f"do-not-execute-{name}", agent=graph)["run_id"]
                denied = self.completed(thread, run_id)
                self.assertEqual(denied["status"], "error", json.dumps(denied))
                self.assertEqual(len(self.records(run_id, "factory")), 1)
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
            print(f"PASS {self.profile}: configured allowlist and explicit deny-all block local effects", flush=True)

            scenario = "configured USD hard cap observes settled ADK spend"
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
            self.assertEqual(self.counters.read_bytes(), before, "denied admission reached the SDK")
            self.expect_usage(budget_graph, (1, 20, 4))
            self.expect_cost(budget_graph, 0.028)
            print(f"PASS {self.profile}: accounted ADK USD blocks later admission at its configured cap", flush=True)
        except Exception:
            print(f"FAILED scenario: {scenario}", file=sys.stderr)
            for process in self.processes:
                print(process.logs(), file=sys.stderr)
            raise


if __name__ == "__main__":
    unittest.main()
