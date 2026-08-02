"""Load local Gemini credentials for example agents.

Looks for a repo-root `.env.llm` (gitignored). Never hard-code keys.
Falls back to process environment if the file is absent (CI / deployed runners).
"""

from __future__ import annotations

import os
from pathlib import Path


def load_llm_env() -> None:
    root = Path(__file__).resolve().parents[2]
    path = root / ".env.llm"
    if not path.is_file():
        return
    for line in path.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, val = line.partition("=")
        key = key.strip()
        val = val.strip().strip('"').strip("'")
        # Don't override an already-exported value (shell / CI wins).
        if key and key not in os.environ:
            os.environ[key] = val


def require_google_api_key() -> str:
    load_llm_env()
    key = os.environ.get("GOOGLE_API_KEY", "").strip()
    if not key:
        raise RuntimeError(
            "GOOGLE_API_KEY is not set. Copy .env.llm.example to .env.llm "
            "(gitignored) and add your key, or export GOOGLE_API_KEY."
        )
    return key


def gemini_model() -> str:
    load_llm_env()
    return os.environ.get("GEMINI_MODEL", "gemini-2.0-flash").strip() or "gemini-2.0-flash"


def gemini_temperature() -> float:
    load_llm_env()
    raw = os.environ.get("GEMINI_TEMPERATURE", "0").strip() or "0"
    try:
        return float(raw)
    except ValueError:
        return 0.0
