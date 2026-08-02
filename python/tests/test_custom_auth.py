"""Self-check for custom_auth.user_from_headers."""

from __future__ import annotations

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner.custom_auth import (  # noqa: E402
    HEADER_IDENTITY,
    HEADER_PERMISSIONS,
    HEADER_TENANT_ID,
    HEADER_USER_JSON,
    user_from_headers,
)


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


def main():
    check("missing headers → None", user_from_headers({}) is None)

    u = user_from_headers(
        {
            HEADER_IDENTITY: "alice",
            HEADER_TENANT_ID: "acme",
            HEADER_PERMISSIONS: "read,write",
        }
    )
    check("identity parsed", u is not None and u.identity == "alice")
    check("tenant parsed", u.tenant_id == "acme")
    check("permissions parsed", u.permissions == ("read", "write"))

    u2 = user_from_headers(
        {
            HEADER_USER_JSON: '{"identity":"bob","tenant_id":"t2","permissions":["admin"],"display_name":"Bob"}',
            HEADER_IDENTITY: "ignored",
        }
    )
    check("JSON header wins", u2 is not None and u2.identity == "bob" and u2.display_name == "Bob")
    print("\nAll custom_auth checks passed.")


if __name__ == "__main__":
    main()
