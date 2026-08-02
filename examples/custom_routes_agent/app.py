"""Custom routes validation app (in-runner mode).

Proves a real ASGI app is reachable through the control plane reverse
proxy with the mount prefix stripped, and that authenticated identity
is available via X-Runkite-* headers (see runkite_runner.custom_auth).
"""

from __future__ import annotations

import os
import sys
from pathlib import Path

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse

# Allow `python -m` / runner load without installing the package editable.
_repo_python = Path(__file__).resolve().parents[2] / "python"
if str(_repo_python) not in sys.path:
    sys.path.insert(0, str(_repo_python))

from runkite_runner.custom_auth import user_from_request  # noqa: E402

app = FastAPI()


@app.get("/ping")
def ping():
    return {"pong": True}


@app.get("/echo/{value}")
def echo(value: str):
    return {"echo": value}


@app.get("/whoami")
def whoami(request: Request):
    """Returns the identity the control plane injected after auth.

    Call via the configured mount, e.g. GET /custom/whoami with a
    valid Authorization header when auth is enabled.
    """
    user = user_from_request(request)
    if user is None:
        return JSONResponse(
            {
                "authenticated": False,
                "hint": "missing X-Runkite-Identity (is the request going through the CP proxy with auth?)",
            },
            status_code=200,
        )
    return {
        "authenticated": True,
        "identity": user.identity,
        "tenant_id": user.tenant_id,
        "permissions": list(user.permissions),
        "display_name": user.display_name,
        "mount_hint": os.environ.get("CUSTOM_ROUTES_MOUNT", "/custom"),
    }
