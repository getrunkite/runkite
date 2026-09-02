"""Unit tests for FinOps usage accumulation from LangGraph chunks.

Invoked as a plain script by ``make test-python`` (unittest), not pytest.
"""

from __future__ import annotations

import unittest

from runkite_runner.usage import accumulate_usage, usage_from_metrics, usage_payload, values_with_usage


class TestUsageAccumulate(unittest.TestCase):
    def test_accumulate_from_aimessage_dict(self):
        totals: dict = {}
        accumulate_usage(
            totals,
            {
                "messages": [
                    {
                        "type": "ai",
                        "usage_metadata": {
                            "input_tokens": 10,
                            "output_tokens": 5,
                            "total_tokens": 15,
                        },
                        "response_metadata": {"model_name": "gemini-2.0-flash"},
                    }
                ]
            },
        )
        self.assertEqual(
            usage_payload(totals),
            {
                "prompt_tokens": 10,
                "completion_tokens": 5,
                "total_tokens": 15,
                "model": "gemini-2.0-flash",
            },
        )

    def test_accumulate_sums_multiple_turns(self):
        totals: dict = {}
        accumulate_usage(
            totals,
            {
                "messages": [
                    {"type": "ai", "usage_metadata": {"input_tokens": 3, "output_tokens": 1}}
                ]
            },
        )
        accumulate_usage(
            totals,
            {
                "messages": [
                    {"type": "ai", "usage_metadata": {"input_tokens": 7, "output_tokens": 2}}
                ]
            },
        )
        u = usage_payload(totals)
        self.assertEqual(u["prompt_tokens"], 10)
        self.assertEqual(u["completion_tokens"], 3)
        self.assertEqual(u["total_tokens"], 13)

    def test_growing_values_snapshots_must_not_be_summed_each_step(self):
        """Regression: default stream_mode=['values'] re-sends full history.

        Worker must meter once from the final snapshot (or equivalent), not
        accumulate_usage on every cumulative values chunk.
        """
        msg1 = {
            "type": "ai",
            "usage_metadata": {"input_tokens": 10, "output_tokens": 5},
            "response_metadata": {"model_name": "gemini-2.0-flash"},
        }
        msg2 = {
            "type": "ai",
            "usage_metadata": {"input_tokens": 7, "output_tokens": 2},
        }
        step1 = {"messages": [msg1]}
        step2 = {"messages": [msg1, msg2]}

        # Wrong: sum every cumulative snapshot (27/12 style overcount).
        wrong: dict = {}
        accumulate_usage(wrong, step1)
        accumulate_usage(wrong, step2)
        self.assertEqual(usage_payload(wrong)["prompt_tokens"], 27)
        self.assertEqual(usage_payload(wrong)["completion_tokens"], 12)

        # Right: once from final values (matches worker success-path).
        right: dict = {}
        accumulate_usage(right, step2)
        self.assertEqual(
            usage_payload(right),
            {
                "prompt_tokens": 17,
                "completion_tokens": 7,
                "total_tokens": 24,
                "model": "gemini-2.0-flash",
            },
        )

    def test_empty_usage_returns_none(self):
        self.assertIsNone(usage_payload({}))


class TestUsageFromMetrics(unittest.TestCase):
    def test_crewai_style_dict(self):
        u = usage_from_metrics({"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18})
        self.assertEqual(u["prompt_tokens"], 11)
        self.assertEqual(u["completion_tokens"], 7)
        self.assertEqual(u["total_tokens"], 18)

    def test_object_attrs(self):
        class M:
            prompt_tokens = 3
            completion_tokens = 2
        u = usage_from_metrics(M())
        self.assertEqual(u["total_tokens"], 5)

    def test_values_with_usage(self):
        base = {"messages": []}
        self.assertEqual(values_with_usage(base, None), base)
        out = values_with_usage(base, {"prompt_tokens": 1, "completion_tokens": 0, "total_tokens": 1})
        self.assertIn("usage", out)


# Must stay at EOF: unittest.main() discovers classes already defined in
# module globals. Placing it between TestUsageAccumulate and
# TestUsageFromMetrics silently drops the metrics tests when CI runs
# ``python python/tests/test_usage_accumulate.py``.
if __name__ == "__main__":
    unittest.main()
