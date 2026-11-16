#!/usr/bin/env python3
"""Qualify this example against explicitly supplied, pinned Trinity source.

Runs the real MCP validator, template renderer and Claude configuration writer
using temporary files. Does not launch Trinity, a native CLI or a model. Requires
Python 3.11 or newer and the standard library only. Makes no network requests.
"""

from __future__ import annotations

import argparse
import contextlib
import copy
import importlib.util
import io
import json
import os
import socket
import subprocess
import sys
import tempfile
import types
import unittest
from pathlib import Path
from unittest.mock import patch

TRINITY_REVISION = "f70ebf6f231a981b032a965e3cc89a97f7ea6635"
SOURCE_FILES = (
    "docker/base-image/agent_server/mcp_validator.py",
    "docker/base-image/agent_server/mcp_template.py",
    "docker/base-image/agent_server/services/trinity_mcp.py",
)
EXAMPLE_DIR = Path(__file__).resolve().parent
SERVER_NAME = "runkite-specialist"


def verify_checkout(checkout: Path) -> None:
    revision = subprocess.run(
        ["git", "-C", str(checkout), "rev-parse", "HEAD"],
        capture_output=True,
        text=True,
        check=True,
        timeout=10,
    ).stdout.strip()
    if revision != TRINITY_REVISION:
        raise ValueError(f"expected Trinity {TRINITY_REVISION}, got {revision}")
    clean = subprocess.run(
        ["git", "-C", str(checkout), "diff", "--quiet", "HEAD", "--", *SOURCE_FILES],
        capture_output=True,
        check=False,
        timeout=10,
    )
    if clean.returncode != 0:
        raise ValueError("the qualified Trinity source files must match the pinned commit")
    for relative in SOURCE_FILES:
        if not (checkout / relative).is_file():
            raise ValueError(f"missing Trinity source file: {relative}")


def load_source(name: str, source: Path) -> types.ModuleType:
    spec = importlib.util.spec_from_file_location(name, source)
    if spec is None or spec.loader is None:
        raise ValueError(f"cannot load {source.name}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def qualify(checkout: Path) -> tuple[unittest.TestResult, int]:
    verify_checkout(checkout)
    sys.dont_write_bytecode = True
    package_name = "runkite_trinity_config_qualification"
    source_dir = checkout / "docker/base-image/agent_server"
    package = types.ModuleType(package_name)
    package.__path__ = [str(source_dir)]
    sys.modules[package_name] = package
    validator = load_source(f"{package_name}.mcp_validator", source_dir / "mcp_validator.py")
    renderer = load_source(f"{package_name}.mcp_template", source_dir / "mcp_template.py")
    configuration = load_source(f"{package_name}.trinity_mcp", source_dir / "services/trinity_mcp.py")
    template = json.loads((EXAMPLE_DIR / ".mcp.json.template").read_text(encoding="utf-8"))

    class ConfigQualification(unittest.TestCase):
        def setUp(self) -> None:
            self.temp = tempfile.TemporaryDirectory(prefix="runkite-trinity-config-")
            self.addCleanup(self.temp.cleanup)
            self.home = Path(self.temp.name)
            self.enterContext(patch.dict(os.environ, {"HOME": str(self.home), "AGENT_RUNTIME": "claude-code"}))
            self.enterContext(patch.object(socket, "getaddrinfo", side_effect=AssertionError("unexpected DNS request")))
            self.enterContext(
                patch.object(configuration.subprocess, "run", side_effect=AssertionError("unexpected subprocess"))
            )

        def rendered(self) -> dict:
            entry, reason = renderer.render_server(
                template["mcpServers"][SERVER_NAME],
                {"RUNKITE_API_KEY": "configuration-test-value"},
                SERVER_NAME,
            )
            self.assertIsNone(reason)
            self.assertIsNotNone(entry)
            return entry

        def test_template_passes_actual_stdio_validator(self) -> None:
            validator.validate_mcp_config(json.dumps(template))
            entry = template["mcpServers"][SERVER_NAME]
            self.assertEqual(entry["command"], "python3")
            self.assertEqual(entry["args"], ["/home/developer/launch.py"])
            self.assertEqual(entry["env"]["RUNKITE_API_KEY"], "${RUNKITE_API_KEY}")
            self.assertEqual(
                set(entry["env"]),
                {
                    "RUNKITE_BRIDGE_URL",
                    "RUNKITE_BRIDGE_AGENT",
                    "RUNKITE_BRIDGE_PROFILE",
                    "RUNKITE_API_KEY",
                    "RUNKITE_BRIDGE_STATE",
                },
            )

        def test_actual_renderer_expands_only_credential_env(self) -> None:
            entry = self.rendered()
            expected = copy.deepcopy(template["mcpServers"][SERVER_NAME])
            expected["env"]["RUNKITE_API_KEY"] = "configuration-test-value"
            self.assertEqual(entry, expected)

        def test_missing_credential_withholds_server(self) -> None:
            entry, reason = renderer.render_server(template["mcpServers"][SERVER_NAME], {}, SERVER_NAME)
            self.assertIsNone(entry)
            self.assertIn("RUNKITE_API_KEY", reason)

        def test_startup_renderer_preserves_existing_server(self) -> None:
            template_path = self.home / ".mcp.json.template"
            config_path = self.home / ".mcp.json"
            env_path = self.home / ".env"
            template_path.write_text(json.dumps(template), encoding="utf-8")
            env_path.write_text("RUNKITE_API_KEY=configuration-test-value\n", encoding="utf-8")
            previous = {"command": "python3", "args": ["/home/developer/unrelated.py"]}
            config_path.write_text(json.dumps({"mcpServers": {"existing": previous}}), encoding="utf-8")
            with contextlib.redirect_stdout(io.StringIO()):
                result = renderer.render(template_path, config_path, env_path)
            self.assertEqual(result["status"], "ok")
            self.assertEqual(result["added"], [SERVER_NAME])
            self.assertFalse(result["skipped"])
            installed = json.loads(config_path.read_text(encoding="utf-8"))["mcpServers"]
            self.assertEqual(installed["existing"], previous)
            self.assertEqual(installed[SERVER_NAME], self.rendered())

        def test_actual_claude_configuration_keeps_env_and_existing_entry(self) -> None:
            config_path = self.home / ".mcp.json"
            previous = {"command": "python3", "args": ["/home/developer/unrelated.py"]}
            config_path.write_text(json.dumps({"mcpServers": {"existing": previous}}), encoding="utf-8")
            entry = self.rendered()

            def isolated_path(value: str) -> Path:
                if value != "/home/developer":
                    raise AssertionError("Claude configuration attempted an unexpected root")
                return self.home

            # Upstream uses a literal container home, not Path.home(). Redirect
            # that one constructor while executing its unchanged real function.
            with patch.object(configuration, "Path", side_effect=isolated_path):
                self.assertTrue(configuration.configure_mcp_servers({SERVER_NAME: entry}))
            installed = json.loads(config_path.read_text(encoding="utf-8"))["mcpServers"]
            self.assertEqual(installed["existing"], previous)
            self.assertEqual(installed[SERVER_NAME], entry)

        def test_actual_validator_rejects_inline_python_execution(self) -> None:
            invalid = copy.deepcopy(template)
            invalid["mcpServers"][SERVER_NAME]["args"] = ["-c", "print(1)"]
            with self.assertRaises(validator.McpValidationError):
                validator.validate_mcp_config(json.dumps(invalid))

        def test_actual_validator_rejects_http_and_private_https(self) -> None:
            for url in ("http://127.0.0.1:2026/mcp", "https://127.0.0.1/mcp"):
                with self.subTest(url=url):
                    loopback = [(socket.AF_INET, socket.SOCK_STREAM, 6, "", ("127.0.0.1", 0))]
                    with patch.object(socket, "getaddrinfo", return_value=loopback):
                        with self.assertRaises(validator.McpValidationError):
                            validator.validate_mcp_config(
                                json.dumps({"mcpServers": {SERVER_NAME: {"type": "http", "url": url}}})
                            )

    suite = unittest.defaultTestLoader.loadTestsFromTestCase(ConfigQualification)
    count = suite.countTestCases()
    return unittest.TextTestRunner(verbosity=2).run(suite), count


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--trinity-checkout", required=True, type=Path)
    args = parser.parse_args()
    if sys.version_info < (3, 11):
        parser.error("Python 3.11 or newer is required by the pinned Trinity source")
    try:
        result, count = qualify(args.trinity_checkout.resolve(strict=True))
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        parser.exit(2, f"Configuration qualification could not run: {error}\n")
    print(
        json.dumps(
            {
                "scope": "pinned Trinity source configuration only",
                "trinity_revision": TRINITY_REVISION,
                "checks": count,
                "passed": result.wasSuccessful(),
                "native_cli_execution": "not qualified",
                "model_calls": 0,
            },
            indent=2,
        )
    )
    return 0 if result.wasSuccessful() else 1


if __name__ == "__main__":
    raise SystemExit(main())
