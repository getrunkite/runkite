"""Exercise CLI configuration without replacing its credential selection logic."""

from __future__ import annotations

import json
import os
import sys
import unittest
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "python"))
sys.path.insert(0, str(ROOT / "python" / "adapters"))

from agent_framework_adapter import __main__ as cli  # noqa: E402


class AgentFrameworkCLITests(unittest.TestCase):
    def test_canonical_runner_token_configures_http_and_shared_worker(self) -> None:
        adapter = MagicMock()
        worker = AsyncMock()
        with (
            patch.dict(
                os.environ, {"RUNNER_TOKEN": "test-canonical", "RUNKITE_RUNNER_TOKEN": "wrong-alias"}, clear=True
            ),
            patch.object(sys, "argv", ["runner", "--config", "agent.json", "--http-address", "http://plane"]),
            patch.object(cli, "setup_logging"),
            patch.object(cli, "AgentFrameworkAdapter", return_value=adapter),
            patch.object(cli, "run_worker", worker),
        ):
            cli.main()
        adapter.configure.assert_called_once_with(
            http_address="http://plane", runner_kind="python-agent-framework", runner_token="test-canonical"
        )
        worker.assert_awaited_once_with(
            adapter, "agent.json", "localhost:50051", "python-agent-framework", 1, http_address="http://plane"
        )

    def test_example_config_routes_to_the_cli_default_kind(self) -> None:
        config = json.loads((ROOT / "examples" / "managed_agent_framework" / "langgraph.json").read_text())
        self.assertEqual(config["runner_kind"], "python-agent-framework")


if __name__ == "__main__":
    unittest.main()
