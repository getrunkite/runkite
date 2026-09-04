"""Load local LLM credentials for example agents.

Looks for:
1. ``RUNKITE_LLM_ENV`` — absolute/relative path to an env file (preferred when
   keys live outside the repo, e.g. ``~/.config/runkite/llm.env``)
2. Else repo-root ``.env.llm`` (gitignored)

Never hard-code keys. Process environment already set wins over the file.
"""

from __future__ import annotations

import os
from pathlib import Path


def _llm_env_path() -> Path | None:
    override = os.environ.get("RUNKITE_LLM_ENV", "").strip()
    if override:
        p = Path(override).expanduser()
        return p if p.is_file() else None
    root = Path(__file__).resolve().parents[2]
    p = root / ".env.llm"
    return p if p.is_file() else None


def load_llm_env() -> None:
    path = _llm_env_path()
    if path is None:
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
            "(gitignored), or set RUNKITE_LLM_ENV to your secrets file, "
            "or export GOOGLE_API_KEY."
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
