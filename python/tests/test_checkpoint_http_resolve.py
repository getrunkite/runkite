"""Self-check: blank / explicit-empty RUNKITE_HTTP_URL → MemorySaver path."""

from __future__ import annotations

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner.checkpoint import resolve_checkpoint_http_url  # noqa: E402


def check(name: str, cond: bool) -> None:
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


def main() -> None:
    saved = os.environ.pop("RUNKITE_HTTP_URL", None)
    try:
        check(
            "unset env falls back to http_address",
            resolve_checkpoint_http_url("http://cp:2026") == "http://cp:2026",
        )
        check(
            "unset env + blank address → None (memory)",
            resolve_checkpoint_http_url("") is None,
        )
        check(
            "unset env + whitespace address → None",
            resolve_checkpoint_http_url("   ") is None,
        )

        os.environ["RUNKITE_HTTP_URL"] = "http://from-env:2026"
        check(
            "env wins over http_address",
            resolve_checkpoint_http_url("http://cli:9") == "http://from-env:2026",
        )

        os.environ["RUNKITE_HTTP_URL"] = ""
        check(
            "explicit empty env → None even if CLI default present",
            resolve_checkpoint_http_url("http://localhost:2026") is None,
        )

        os.environ["RUNKITE_HTTP_URL"] = "  "
        check(
            "whitespace-only env → None",
            resolve_checkpoint_http_url("http://localhost:2026") is None,
        )
    finally:
        if saved is None:
            os.environ.pop("RUNKITE_HTTP_URL", None)
        else:
            os.environ["RUNKITE_HTTP_URL"] = saved

    print("all checkpoint http resolve checks passed")


if __name__ == "__main__":
    main()
