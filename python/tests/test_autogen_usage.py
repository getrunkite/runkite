"""Unit check: AutoGen TaskResult.messages[].models_usage → Output.usage.

Usage:
    python/adapters/autogen_adapter/.venv/bin/python \\
      python/tests/test_autogen_usage.py
"""

from __future__ import annotations

import os
import sys
from types import SimpleNamespace

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "adapters", "autogen_adapter"))

from adapter import _usage_from_autogen_result  # noqa: E402


def check(name: str, cond: bool) -> None:
    print(f"[{'PASS' if cond else 'FAIL'}] {name}")
    if not cond:
        raise SystemExit(1)


def main() -> None:
    # Top-level usage_from_metrics(TaskResult) historically saw nothing;
    # tokens live on each message's models_usage (RequestUsage).
    result = SimpleNamespace(
        messages=[
            SimpleNamespace(models_usage=SimpleNamespace(prompt_tokens=10, completion_tokens=3)),
            SimpleNamespace(models_usage=SimpleNamespace(prompt_tokens=2, completion_tokens=5)),
            SimpleNamespace(models_usage=None),
        ]
    )
    u = _usage_from_autogen_result(result)
    check(
        "sums RequestUsage across messages",
        u
        == {
            "prompt_tokens": 12,
            "completion_tokens": 8,
            "total_tokens": 20,
        },
    )
    check("empty TaskResult → None", _usage_from_autogen_result(SimpleNamespace(messages=[])) is None)
    print("\nAll checks passed.")


if __name__ == "__main__":
    main()
