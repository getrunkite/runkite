"""Self-check for connectors.py run-bound header assembly.

Does not hit a live control plane -- only proves the helper refuses a
config without run_id and attaches run_id/generation headers when present.

Usage:
    python/.venv/bin/python python/tests/test_connectors.py
"""

from __future__ import annotations

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner.connectors import ConnectorError, _run_bound_headers  # noqa: E402
from runkite_runner.tenant_ctx import HEADER_GENERATION, HEADER_RUN_ID, HEADER_TENANT_ID  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


def test_requires_run_id():
    try:
        _run_bound_headers({"configurable": {}})
        check("missing run_id raises", False)
    except ValueError as e:
        check("missing run_id raises ValueError", "run_id" in str(e))


def test_headers_from_configurable():
    h = _run_bound_headers({"configurable": {"run_id": "run-1", "generation": 4}})
    check("run id header", h.get(HEADER_RUN_ID) == "run-1")
    check("generation header", h.get(HEADER_GENERATION) == "4")
    check("tenant header present", HEADER_TENANT_ID in h)


def test_accepts_bare_configurable_dict():
    h = _run_bound_headers({"run_id": "run-2", "generation": 1})
    check("bare configurable works", h.get(HEADER_RUN_ID) == "run-2")


def main():
    test_requires_run_id()
    test_headers_from_configurable()
    test_accepts_bare_configurable_dict()
    # ConnectorError is part of the public surface -- keep the import live.
    check("ConnectorError is an Exception", issubclass(ConnectorError, Exception))
    print("\nAll checks passed.")


if __name__ == "__main__":
    main()
