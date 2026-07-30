"""Shared logging setup for every runner entrypoint -- worker.py (the
LangGraph runner) and all 4 framework adapters' __main__.py (CrewAI,
LlamaIndex, AutoGen, plain LangChain) used to each hardcode their own
identical `logging.basicConfig(level=logging.INFO, format=...)` call.

LOG_LEVEL and LOG_FORMAT env vars mirror the Go control plane's
cmd/logging.go and the TypeScript runner's logger.ts, so the same two
env vars configure logging consistently across all three SDKs.
"""

import json
import logging
import os

_LEVELS = {
    "debug": logging.DEBUG,
    "info": logging.INFO,
    "warn": logging.WARNING,
    "warning": logging.WARNING,
    "error": logging.ERROR,
}


class _JSONFormatter(logging.Formatter):
    """Minimal stdlib-only JSON formatter. A handful of fields doesn't
    justify a new dependency (python-json-logger etc.) -- mirrors the Go
    side's slog.NewJSONHandler and the TS side's own JSON logger in
    shape (time/level/name-or-logger/message), not byte-for-byte format,
    since none of the three SDKs' log lines are meant to be diffed
    against each other, only independently machine-parseable.
    """

    def format(self, record: logging.LogRecord) -> str:
        payload = {
            "time": self.formatTime(record, "%Y-%m-%dT%H:%M:%S%z"),
            "level": record.levelname,
            "name": record.name,
            "message": record.getMessage(),
        }
        if record.exc_info:
            payload["exc_info"] = self.formatException(record.exc_info)
        return json.dumps(payload)


def setup_logging() -> None:
    """Configures the root logger from LOG_LEVEL (debug|info|warn|error,
    case-insensitive, default info) and LOG_FORMAT (text|json, default
    text). Unset envs behave exactly like the hardcoded
    logging.INFO/plain-text setup this replaces -- nothing regresses for
    anyone not setting them. An unrecognized LOG_LEVEL falls back to
    info rather than raising, so a typo doesn't crash runner startup.
    """
    level = _LEVELS.get(os.environ.get("LOG_LEVEL", "info").strip().lower(), logging.INFO)

    handler = logging.StreamHandler()
    if os.environ.get("LOG_FORMAT", "text").strip().lower() == "json":
        handler.setFormatter(_JSONFormatter())
    else:
        handler.setFormatter(logging.Formatter("%(asctime)s %(name)s %(levelname)s %(message)s"))

    root = logging.getLogger()
    root.handlers.clear()
    root.addHandler(handler)
    root.setLevel(level)
