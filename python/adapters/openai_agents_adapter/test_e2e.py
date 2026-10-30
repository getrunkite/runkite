"""Opt-in installed-wheel OpenAI Agents lifecycle against the real control plane.

Set RUNKITE_TEST_BINARY, RUNKITE_TEST_SEED_BINARY and RUNKITE_TEST_RUNNER_WHEEL.
Optional paired RUNKITE_TEST_POSTGRES_DSN and RUNKITE_TEST_REDIS_URL select the
HA storage profile. The fixture makes no external model or tracing requests.
The configured USD prices are fictional test rates, not provider pricing.
"""

from __future__ import annotations

import importlib.util
import json
import sys
import unittest
from pathlib import Path

# Reuse the exact five installed-wheel scenarios and transport/proxy harness.
# This source-only test module is never imported by the installed worker.
_path = Path(__file__).resolve().parents[1] / "agent_framework_adapter" / "test_e2e.py"
_spec = importlib.util.spec_from_file_location("runkite_managed_runner_e2e", _path)
assert _spec is not None and _spec.loader is not None
_base = importlib.util.module_from_spec(_spec)
sys.modules[_spec.name] = _base
_spec.loader.exec_module(_base)


class ManagedOpenAIAgentsE2E(_base.ManagedAgentFrameworkE2E):
    adapter_package = "openai_agents_adapter"
    sdk_module = "agents"
    sdk_distribution = "openai-agents"
    sdk_version = "0.24.0"
    install_extra = "openai-agents"
    runner_command_name = "runkite-runner-openai-agents"
    runner_description = "Runkite managed OpenAI Agents SDK runner"
    example_directory = "managed_openai_agents"
    fixture = Path(__file__).resolve().parent / "e2e_agent.py"
    runner_kind_env = "PYTHON_OPENAI_AGENTS"
    counters_env = "RUNKITE_OPENAI_E2E_COUNTERS"
    metadata_key = "openai_agents"
    fixture_label = "oa"
    extra_graph_factories = ("error_agent", "unsupported_agent", "blocked_agent", "deny_all_agent", "unicode_agent")
    extra_runner_args = ("--concurrency", "2")
    extra_worker_environment = {"OPENAI_AGENTS_DISABLE_TRACING": "1"}

    def additional_configuration(self) -> dict:
        return {
            "allowed_tools": {
                self.agent: ["record_effect"],
                self.extra_graphs["blocked_agent"]: ["a_different_tool"],
                self.extra_graphs["deny_all_agent"]: [],
            },
            # Fictional prices make the real control-plane estimator observable.
            "finops": {"pricebook": {"offline-managed-e2e": {"input_per_1k": 1, "output_per_1k": 2}}},
        }

    def test_managed_lifecycle(self) -> None:
        super().test_managed_lifecycle()
        scenario = "model failure retains completed request usage"
        try:
            error_agent = self.extra_graphs["error_agent"]
            thread = self.new_thread()
            failed = self.new_run(thread, "error-after-first-tool", agent=error_agent)["run_id"]
            final = self.completed(thread, failed)
            self.assertEqual(final["status"], "error", json.dumps(final))
            self.assertEqual(len(self.records(failed, "factory")), 1)
            self.assertEqual(len(self.records(failed, "model")), 2)
            self.assertEqual(len(self.records(failed, "tool")), 1)
            self.assertEqual(len(self.records(failed, "failed")), 1)
            usage = final["output"]["usage"]
            self.assertEqual((usage["prompt_tokens"], usage["completion_tokens"]), (10, 4))
            self.expect_usage(error_agent, (1, 10, 4))
            self.request(f"/threads/{thread}/runs/{failed}/wait")
            self.expect_usage(error_agent, (1, 10, 4))
            print(f"PASS {self.profile}: model error retains observed usage once", flush=True)

            scenario = "concurrent nondefault tenants and cancellation isolation"
            first_thread = self.new_thread()
            second_thread = self.new_thread(key="e2e-b")
            first = self.new_run(first_thread, "concurrent-a-only", agent=self.slow)["run_id"]
            second = self.new_run(second_thread, "concurrent-b-only", agent=self.slow, key="e2e-b")["run_id"]
            _base.wait_for(
                "both native models stalled", lambda: self.records(first, "stalled") and self.records(second, "stalled")
            )
            for run_id, text, tenant in (
                (first, "concurrent-a-only", self.tenant_a),
                (second, "concurrent-b-only", self.tenant_b),
            ):
                self.assertEqual(self.records(run_id, "factory")[0]["tenant_id"], tenant)
                self.assertEqual(self.records(run_id, "model")[0]["users"], [text])
                self.assertEqual(self.records(run_id, "tool")[0]["text"], text)
                self.assertEqual(len(self.records(run_id, "factory")), 1)
                self.assertEqual(len(self.records(run_id, "tool")), 1)
            self.request(f"/threads/{first_thread}/runs/{first}", key="e2e-b", status=404)
            self.request(f"/threads/{second_thread}/runs/{second}", status=404)
            self.wait_provisional_usage(first_thread, first)
            self.request(f"/threads/{first_thread}/runs/{first}/cancel", {})
            first_canceled = self.completed(first_thread, first)
            self.assertEqual(first_canceled["status"], "interrupted")
            self.assertEqual(first_canceled["output"]["usage"]["model"], "offline-managed-e2e")
            _base.wait_for("only first native model canceled", lambda: self.records(first, "canceled"))
            self.assertEqual(self.request(f"/threads/{second_thread}/runs/{second}", key="e2e-b")["status"], "running")
            self.assertFalse(self.records(second, "canceled"), "one run's cancel reached another tenant's model")
            self.wait_provisional_usage(second_thread, second, key="e2e-b")
            self.request(f"/threads/{second_thread}/runs/{second}/cancel", {}, key="e2e-b")
            second_canceled = self.completed(second_thread, second, key="e2e-b")
            self.assertEqual(second_canceled["status"], "interrupted")
            self.assertEqual(second_canceled["output"]["usage"]["model"], "offline-managed-e2e")
            _base.wait_for("second native model canceled independently", lambda: self.records(second, "canceled"))
            self.expect_usage(self.slow, (3, 30, 12))
            print(f"PASS {self.profile}: simultaneous tenants keep context, tools and cancel isolated", flush=True)

            scenario = "unsupported native handoff is an explicit managed error"
            unsupported_thread = self.new_thread()
            unsupported = self.new_run(
                unsupported_thread, "do-not-execute-unsupported-handoff", agent=self.extra_graphs["unsupported_agent"]
            )["run_id"]
            final = self.completed(unsupported_thread, unsupported)
            self.assertEqual(final["status"], "error", json.dumps(final))
            self.assertEqual(len(self.records(unsupported, "factory")), 1)
            self.assertFalse(self.records(unsupported, "model"), "unsupported agents reached the model")
            self.assertFalse(self.records(unsupported, "tool"), "unsupported agents performed a tool effect")
            replay = self.http.get(
                f"{self.base}/threads/{unsupported_thread}/runs/{unsupported}/stream",
                headers={"Authorization": "Bearer e2e-a"},
            )
            self.assertEqual(replay.status_code, 200)
            self.assertIn("event: error", replay.text)
            self.assertIn("handoff", replay.text.lower())
            print(f"PASS {self.profile}: unsupported handoff fails before model or tool execution", flush=True)

            scenario = "tool allowlists deny before local function effects"
            for name in ("blocked_agent", "deny_all_agent"):
                graph = self.extra_graphs[name]
                blocked_thread = self.new_thread()
                blocked = self.new_run(blocked_thread, f"do-not-execute-{name}", agent=graph)["run_id"]
                final = self.completed(blocked_thread, blocked)
                self.assertEqual(final["status"], "error", json.dumps(final))
                self.assertEqual(len(self.records(blocked, "factory")), 1)
                self.assertEqual(len(self.records(blocked, "model")), 1)
                self.assertFalse(self.records(blocked, "tool"), f"{name} allowed a local function effect")
                usage = final["output"]["usage"]
                self.assertEqual((usage["prompt_tokens"], usage["completion_tokens"]), (10, 2))
                replay = self.http.get(
                    f"{self.base}/threads/{blocked_thread}/runs/{blocked}/stream",
                    headers={"Authorization": "Bearer e2e-a"},
                )
                self.assertEqual(replay.status_code, 200)
                self.assertIn("event: tool_auth", replay.text)
                self.assertIn("tool_not_allowed", replay.text)
                self.expect_usage(graph, (1, 10, 2))
            print(f"PASS {self.profile}: configured allowlist and explicit deny-all block local effects", flush=True)

            scenario = "multibyte output survives gRPC and committed receipt recovery"
            unicode_graph = self.extra_graphs["unicode_agent"]
            unicode_thread = self.new_thread()
            self.runner.stop()
            proxy, committed, outcome = self.crash_after_commit_proxy(unicode_thread)
            self.start_runner(proxy)
            unicode_run = self.new_run(unicode_thread, "unicode-receipt-recovery", agent=unicode_graph)["run_id"]
            self.assertTrue(committed.wait(25), "Unicode checkpoint never committed before the worker crash")
            self.assertTrue("error" not in outcome, outcome.get("error", ""))
            self.assertEqual(outcome.get("run_id"), unicode_run)
            self.assertEqual(outcome.get("receipt", {}).get("completed", {}).get("run_id"), unicode_run)
            self.assertNotEqual(self.request(f"/threads/{unicode_thread}/runs/{unicode_run}")["status"], "success")
            self.start_runner()
            final = self.completed(unicode_thread, unicode_run)
            self.assertEqual(final["status"], "success", f"Unicode run {unicode_run} did not complete")
            assistant = final["output"]["messages"][-1]
            self.assertEqual(assistant["role"], "assistant")
            content = assistant["content"]
            self.assertEqual(len(content), 900_000)
            self.assertEqual(len(content.encode("utf-8")), 2_700_000)
            self.assertTrue(set(content) == {"界"}, "Unicode output content was changed")
            usage = final["output"]["usage"]
            self.assertEqual((usage["prompt_tokens"], usage["completion_tokens"]), (20, 4))
            self.assertGreaterEqual(final["metadata"]["_runkite_completion_generation"], 2)
            self.assertEqual(
                final["output"]["openai_agents"]["generation"], final["metadata"]["_runkite_completion_generation"]
            )
            self.assertEqual(len(self.records(unicode_run, "factory")), 1, "Unicode receipt repeated the factory")
            self.assertEqual(len(self.records(unicode_run, "model")), 2, "Unicode receipt repeated the model")
            self.assertEqual(len(self.records(unicode_run, "tool")), 1, "Unicode receipt repeated the tool")
            thread_values = self.request(f"/threads/{unicode_thread}")["values"]
            self.assertTrue(thread_values == final["output"], "Unicode run and durable thread output differ")
            self.expect_usage(unicode_graph, (1, 20, 4))
            self.request(f"/threads/{unicode_thread}/runs/{unicode_run}/wait")
            self.expect_usage(unicode_graph, (1, 20, 4))
            print(
                f"PASS {self.profile}: 2.7MB UTF8 output and new-generation receipt replay without repeat work",
                flush=True,
            )

            scenario = "fictional pricebook settles actual model usage once"
            for graph, expected in (
                (self.agent, 0.112),
                (self.slow, 0.054),
                (error_agent, 0.018),
                (self.extra_graphs["blocked_agent"], 0.014),
                (self.extra_graphs["deny_all_agent"], 0.014),
                (unicode_graph, 0.028),
            ):
                rows = self.request(f"/admin-api/usage/summary?agent_id={graph}", key="e2e-admin")
                actual = sum(row["usd_estimate"] for row in rows)
                self.assertAlmostEqual(actual, expected, places=9, msg=f"Wrong fictional cost for {graph}")
            print(
                f"PASS {self.profile}: fictional pricebook settles success, cancel, error and receipt usage once",
                flush=True,
            )
        except Exception:
            print(f"FAILED scenario: {scenario}", file=sys.stderr)
            for process in self.processes:
                print(process.logs(), file=sys.stderr)
            raise


if __name__ == "__main__":
    unittest.main()
