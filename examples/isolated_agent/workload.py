"""Offline workload and negative probes for the external bridge example.

This file has no runner client and receives no bootstrap credentials. Replace
its deterministic turn with an SDK loop in a separately qualified image.
"""

import json
import os
import stat
import sys
import time
from pathlib import Path
from urllib.error import HTTPError
from urllib.request import urlopen


def readable(path):
    try:
        Path(path).read_bytes()
        return True
    except (OSError, PermissionError):
        return False


def socket_path_status(path):
    try:
        metadata = Path(path).lstat()
    except FileNotFoundError:
        return "absent"
    except PermissionError:
        return "denied"
    except OSError:
        return "unknown_error"
    return "visible_socket" if stat.S_ISSOCK(metadata.st_mode) else "visible_other"


def network_reachable(url):
    try:
        with urlopen(url, timeout=3):
            return True
    except HTTPError:
        # Any HTTP response establishes reachability, even a denial or error
        # from the server. It cannot count as network-policy enforcement.
        return True
    except Exception:
        return False


def main():
    request = json.loads(sys.stdin.readline())
    payload = request.get("input") or {}
    if payload.get("tool"):
        print(
            json.dumps(
                {
                    "type": "tool_call",
                    "id": "fixture-call",
                    "connector": "fixture",
                    "tool": payload["tool"],
                    "arguments": {"value": "synthetic"},
                }
            ),
            flush=True,
        )
        tool_response = json.loads(sys.stdin.readline())["response"]
    else:
        tool_response = None
    if payload.get("sleep"):
        time.sleep(600)
    probes = [network_reachable(url) for url in payload.get("probe_urls", [])]
    turn = (request.get("state") or 0) + 1
    print(
        json.dumps({"type": "event", "method": "messages/partial", "data": {"content": "offline streamed reply"}}),
        flush=True,
    )
    output = {
        "turn": turn,
        "echo": payload.get("text"),
        "uid": os.getuid(),
        "bootstrap_environment_present": any(
            key in os.environ
            for key in (
                "RUNNER_TOKEN",
                "POSTGRES_DSN",
                "REDIS_URL",
                "VAULT_TOKEN",
                "RUNKITE_API_KEY",
                "RUNKITE_FIXTURE_API_KEY",
                "FIXTURE_DOWNSTREAM_SECRET",
            )
        ),
        "forbidden_fixture_readable": readable("/var/lib/forbidden-fixture/value"),
        "host_socket_status": socket_path_status("/var/run/docker.sock"),
        "network_probes": probes,
        "tool_response": tool_response,
    }
    print(json.dumps({"type": "result", "status": "success", "state": turn, "output": output}), flush=True)


if __name__ == "__main__":
    main()
