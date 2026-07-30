"""Self-check for logging_config.py's setup_logging: LOG_LEVEL and
LOG_FORMAT env vars, shared by worker.py and all 4 framework adapters'
__main__.py.

Proves:
1. LOG_LEVEL filters what gets emitted (default info; warn hides info).
2. LOG_FORMAT=json actually switches the handler to JSON output.
3. Unset env vars keep the original plain-text shape -- nothing
   regresses for anyone not setting them.
4. An unrecognized LOG_LEVEL falls back to info instead of raising.

Usage:
    python/.venv/bin/python python/tests/test_logging_config.py
"""

from __future__ import annotations

import io
import logging
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner.logging_config import setup_logging  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


def _capture(level=None, fmt=None):
    """Runs setup_logging() with the given env vars, then swaps the
    resulting handler's stream for a buffer so output can be asserted
    on -- setup_logging() itself always writes to a StreamHandler
    (defaulting to stderr), so redirecting AFTER setup is simpler than
    mocking os.environ.get at import time.
    """
    old_level, old_fmt = os.environ.get("LOG_LEVEL"), os.environ.get("LOG_FORMAT")
    try:
        if level is None:
            os.environ.pop("LOG_LEVEL", None)
        else:
            os.environ["LOG_LEVEL"] = level
        if fmt is None:
            os.environ.pop("LOG_FORMAT", None)
        else:
            os.environ["LOG_FORMAT"] = fmt

        setup_logging()
        buf = io.StringIO()
        root = logging.getLogger()
        root.handlers[0].stream = buf

        test_logger = logging.getLogger("runkite.runner")
        test_logger.debug("debug line")
        test_logger.info("info line")
        test_logger.warning("warn line")
        return buf.getvalue()
    finally:
        if old_level is None:
            os.environ.pop("LOG_LEVEL", None)
        else:
            os.environ["LOG_LEVEL"] = old_level
        if old_fmt is None:
            os.environ.pop("LOG_FORMAT", None)
        else:
            os.environ["LOG_FORMAT"] = old_fmt


def test_default_level_is_info_text_format():
    out = _capture(level=None, fmt=None)
    check("debug filtered out at default (info) level", "debug line" not in out)
    check("info line present at default level", "info line" in out)
    check("plain text shape, not JSON", "{" not in out)


def test_warn_level_filters_info_and_debug():
    out = _capture(level="warn", fmt=None)
    check("debug filtered out at warn level", "debug line" not in out)
    check("info filtered out at warn level", "info line" not in out)
    check("warn line still present", "warn line" in out)


def test_json_format_switches_handler():
    out = _capture(level="debug", fmt="json")
    check("JSON output contains braces", "{" in out)
    check("JSON output has a level field", '"level": "DEBUG"' in out)
    check("JSON output has a message field", '"message": "debug line"' in out)


def test_unrecognized_level_falls_back_to_info():
    out = _capture(level="not-a-real-level", fmt=None)
    check("unrecognized LOG_LEVEL doesn't crash setup_logging", True)
    check("falls back to info (debug still filtered)", "debug line" not in out)
    check("falls back to info (info still shown)", "info line" in out)


def main():
    test_default_level_is_info_text_format()
    test_warn_level_filters_info_and_debug()
    test_json_format_switches_handler()
    test_unrecognized_level_falls_back_to_info()
    print("\nAll checks passed.")


if __name__ == "__main__":
    main()
