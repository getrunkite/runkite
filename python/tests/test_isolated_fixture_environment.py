"""Sterile fixture child environments and read-only cancellation evidence."""

from __future__ import annotations

import importlib.util
import json
import os
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location(
    "isolated_fixture_environment", ROOT / "scripts/isolated_fixture_environment.py"
)
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


class FixtureEnvironmentTests(unittest.TestCase):
    def test_only_path_is_inherited_and_owned_paths_are_private_stable(self):
        with tempfile.TemporaryDirectory() as directory:
            selected = Path(directory) / "environment"
            source = {
                "PATH": os.defpath,
                "HOME": "/synthetic-user",
                "XDG_CONFIG_HOME": "/synthetic-user/config",
                "OTEL_EXPORTER_OTLP_ENDPOINT": "https://private.invalid",
                "OTEL_EXPORTER_OTLP_HEADERS": "secret",
                "OPENAI_API_KEY": "secret",
                "AWS_ACCESS_KEY_ID": "secret",
                "AWS_SECRET_ACCESS_KEY": "secret",
                "VAULT_TOKEN": "secret",
                "RUNNER_TOKEN": "secret",
                "RUNNER_TOKEN_POOL_FIXTURE": "secret",
                "HTTP_PROXY": "http://credential:secret@proxy.invalid",
                "HTTPS_PROXY": "secret",
                "ALL_PROXY": "secret",
                "SSL_CERT_FILE": "/synthetic-ca",
                "PYTHONPATH": "/synthetic-package",
                "PYTHONSTARTUP": "/synthetic-startup",
                "DOCKER_HOST": "tcp://private.invalid:2375",
                "DOCKER_CONFIG": "/synthetic-user/docker",
                "OPENSHELL_GATEWAY_ENDPOINT": "https://private.invalid",
                "OPENSHELL_GATEWAY_INSECURE": "1",
                "UV_INDEX_URL": "https://credential:secret@index.invalid",
                "UV_CACHE_DIR": "/synthetic-user/cache",
                "PIP_CONFIG_FILE": "/synthetic-user/pip.conf",
                "RUNKITE_TEST_UNKNOWN_SECRET": "secret",
            }
            env = fixture.sterile_environment(selected, source=source)
            self.assertEqual(env, fixture.sterile_environment(selected, source=source))
            self.assertEqual(env["PATH"], os.defpath)
            for key in source:
                if key not in {"PATH", "HOME", "XDG_CONFIG_HOME", "DOCKER_CONFIG", "UV_CACHE_DIR", "PIP_CONFIG_FILE"}:
                    self.assertNotIn(key, env)
            for key in (
                "HOME",
                "XDG_CONFIG_HOME",
                "XDG_DATA_HOME",
                "XDG_STATE_HOME",
                "XDG_CACHE_HOME",
                "XDG_RUNTIME_DIR",
                "TMPDIR",
                "DOCKER_CONFIG",
                "BUILDX_CONFIG",
                "UV_CACHE_DIR",
                "PIP_CACHE_DIR",
            ):
                owned = Path(env[key])
                self.assertTrue(owned.is_relative_to(selected))
                self.assertEqual(stat.S_IMODE(owned.stat().st_mode), 0o700)
            self.assertEqual(env["NETRC"], os.devnull)
            self.assertEqual(env["PIP_CONFIG_FILE"], os.devnull)
            self.assertEqual(env["OPENSSL_CONF"], os.devnull)
            self.assertNotIn("secret", json.dumps(env))

    def test_prepared_caches_require_explicit_selection_not_ambient_inheritance(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            prepared = root / "public-package-cache"
            env = fixture.sterile_environment(
                root / "environment",
                source={"PATH": os.defpath, "GOCACHE": "/synthetic-private"},
                cache_dirs={"GOCACHE": prepared},
            )
            self.assertEqual(env["GOCACHE"], str(prepared))
            with self.assertRaisesRegex(ValueError, "unsupported isolated fixture cache"):
                fixture.sterile_environment(root / "environment", source={}, cache_dirs={"AWS_CONFIG_FILE": prepared})

    def test_directories_and_path_cannot_select_relative_or_symlink_locations(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            target = root / "target"
            target.mkdir()
            link = root / "linked"
            link.symlink_to(target, target_is_directory=True)
            for selected in (Path("relative"), link):
                with self.assertRaises(ValueError):
                    fixture.sterile_environment(selected, source={})
            for path in ("", "/bin::/usr/bin", ".:/bin", "/bin\nsecret", "/bin\x00secret"):
                with self.subTest(path=path), self.assertRaises(ValueError):
                    fixture.sterile_environment(root / "environment", source={"PATH": path})

    def test_only_registered_explicit_fixture_inputs_are_passed(self):
        source = {
            "RUNKITE_TEST_BINARY": "/owned/runkite",
            "RUNKITE_TEST_RUNNER_WHEEL": "/owned/runner.whl",
            "RUNKITE_TEST_ISOLATED_CLI": "/owned/openshell",
            "RUNKITE_TEST_ISOLATED": "1",
            "RUNKITE_TEST_ISOLATED_HA": "1",
            "RUNKITE_TEST_ISOLATED_REPETITIONS": "3",
            "RUNKITE_TEST_ISOLATED_GATEWAY": "https://127.0.0.1:1234",
            "RUNKITE_TEST_ISOLATED_GATEWAY_CONFIG": "/owned/cli-config",
            "RUNKITE_TEST_ISOLATED_IMAGE": "sha256:" + "a" * 64,
            "RUNKITE_TEST_UNKNOWN_SECRET": "secret",
            "RUNKITE_ISOLATED_HA_PG_PASSWORD": "a" * 43,
            "RUNKITE_TEST_ISOLATED_POSTGRES_DSN": "postgres://runkite:synthetic@127.0.0.1:15432/runkite_isolated_ha_fixture?sslmode=disable",
            "RUNKITE_TEST_ISOLATED_REDIS_URL": "redis://127.0.0.1:16379/0",
        }
        selected = fixture.fixture_inputs(source)
        self.assertNotIn("RUNKITE_TEST_UNKNOWN_SECRET", selected)
        self.assertEqual(
            fixture.fixture_inputs(source, ["RUNKITE_TEST_BINARY"]), {"RUNKITE_TEST_BINARY": "/owned/runkite"}
        )
        self.assertEqual(selected["RUNKITE_TEST_ISOLATED_REPETITIONS"], "3")
        with self.assertRaisesRegex(ValueError, "unsupported isolated fixture input"):
            fixture.fixture_inputs(source, ["RUNKITE_TEST_UNKNOWN_SECRET"])

    def test_invalid_selectors_fail_without_copying_their_values_into_errors(self):
        for key, value in (
            ("RUNKITE_TEST_BINARY", "relative-secret"),
            ("RUNKITE_TEST_ISOLATED_GATEWAY", "https://credential:secret@127.0.0.1:1234"),
            ("RUNKITE_TEST_ISOLATED_GATEWAY", "https://private.invalid:443"),
            ("RUNKITE_TEST_ISOLATED_GATEWAY", "https://127.0.0.1:1234/path?secret=value"),
            (
                "RUNKITE_TEST_ISOLATED_POSTGRES_DSN",
                "postgres://runkite:secret@127.0.0.1:5432/production?sslmode=disable",
            ),
            ("RUNKITE_TEST_ISOLATED_REDIS_URL", "redis://127.0.0.1:6379/1"),
            ("RUNKITE_TEST_ISOLATED_REPETITIONS", "21"),
            ("RUNKITE_TEST_ISOLATED", "true"),
            ("RUNKITE_TEST_ISOLATED_IMAGE", "public-image:latest"),
            ("RUNKITE_ISOLATED_HA_PG_PASSWORD", "secret\nvalue"),
        ):
            with self.subTest(key=key), self.assertRaises(ValueError) as raised:
                fixture.fixture_inputs({key: value})
            self.assertNotIn(value, str(raised.exception))
            self.assertNotIn("secret", str(raised.exception))

    def test_cli_executes_with_selected_environment_without_exposing_values(self):
        with tempfile.TemporaryDirectory() as directory:
            source = {
                "PATH": os.defpath,
                "RUNKITE_TEST_BINARY": "/owned/runkite",
                "OTEL_EXPORTER_OTLP_HEADERS": "secret",
            }
            with (
                patch.dict(os.environ, source, clear=True),
                patch.object(
                    sys,
                    "argv",
                    ["helper", "--directory", directory, "--input", "RUNKITE_TEST_BINARY", "--", "python3", "-V"],
                ),
                patch.object(fixture.os, "execvpe") as execute,
            ):
                fixture.main()
            command, arguments, env = execute.call_args.args
            self.assertEqual((command, arguments), ("python3", ["python3", "-V"]))
            self.assertEqual(env["RUNKITE_TEST_BINARY"], "/owned/runkite")
            self.assertNotIn("OTEL_EXPORTER_OTLP_HEADERS", env)

    def test_actual_child_cannot_observe_synthetic_ambient_auth_or_user_configuration(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            personal = root / "synthetic-personal"
            personal.mkdir()
            (personal / ".netrc").write_text("machine private.invalid login synthetic password synthetic-secret")
            source = {
                "PATH": os.defpath,
                "HOME": str(personal),
                "NETRC": str(personal / ".netrc"),
                "OPENAI_API_KEY": "synthetic-secret",
                "OTEL_EXPORTER_OTLP_HEADERS": "synthetic-secret",
            }
            env = fixture.sterile_environment(root / "environment", source=source)
            code = "import json,os,pathlib; print(json.dumps({'home':os.environ['HOME'],'netrc':os.environ['NETRC'],'user_netrc':(pathlib.Path.home()/'.netrc').exists(),'keys':sorted(os.environ)}))"
            result = subprocess.run(
                [sys.executable, "-I", "-c", code], env=env, capture_output=True, text=True, check=True, timeout=5
            )
            observed = json.loads(result.stdout)
            self.assertEqual(observed["home"], env["HOME"])
            self.assertEqual(observed["netrc"], os.devnull)
            self.assertFalse(observed["user_netrc"])
            self.assertNotIn("OPENAI_API_KEY", observed["keys"])
            self.assertNotIn("OTEL_EXPORTER_OTLP_HEADERS", observed["keys"])


class DockerPluginEnvironmentTests(unittest.TestCase):
    def test_only_explicit_executables_get_owned_plugin_links(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            executable = root / "public-compose-tool"
            executable.write_text("#!/bin/sh\nexit 0\n")
            executable.chmod(0o700)
            selected = root / "selected-plugin"
            selected.symlink_to(executable)
            env = fixture.sterile_environment(
                root / "environment",
                source={"PATH": os.defpath, "DOCKER_CONFIG": "/synthetic-personal/docker"},
                docker_plugins={"compose": selected},
            )
            plugin = Path(env["DOCKER_CONFIG"]) / "cli-plugins/docker-compose"
            self.assertTrue(plugin.is_symlink())
            self.assertEqual(plugin.resolve(), executable.resolve())
            self.assertFalse((plugin.parent / "docker-buildx").exists())
            self.assertEqual(
                env,
                fixture.sterile_environment(
                    root / "environment", source={"PATH": os.defpath}, docker_plugins={"compose": executable}
                ),
            )
            fresh = fixture.sterile_environment(root / "fresh", source={"PATH": os.defpath})
            self.assertFalse((Path(fresh["DOCKER_CONFIG"]) / "cli-plugins").exists())

    def test_unknown_missing_nonexecutable_and_directory_plugins_fail_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            nonexecutable = root / "nonexecutable"
            nonexecutable.write_text("synthetic tool")
            nonexecutable.chmod(0o600)
            for name, path in (
                ("credential", nonexecutable),
                ("compose", root / "missing"),
                ("compose", root),
                ("compose", nonexecutable),
            ):
                with self.subTest(name=name, path=str(path)), self.assertRaises(ValueError):
                    fixture.sterile_environment(root / "environment", source={}, docker_plugins={name: path})

    def test_cli_installs_only_selected_plugin_inputs(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            executable = root / "compose"
            executable.write_text("#!/bin/sh\nexit 0\n")
            executable.chmod(0o700)
            with (
                patch.dict(
                    os.environ, {"PATH": os.defpath, "RUNKITE_TEST_DOCKER_COMPOSE_PLUGIN": str(executable)}, clear=True
                ),
                patch.object(
                    sys,
                    "argv",
                    [
                        "helper",
                        "--directory",
                        str(root / "environment"),
                        "--input",
                        "RUNKITE_TEST_DOCKER_COMPOSE_PLUGIN",
                        "--",
                        "docker",
                        "compose",
                        "version",
                    ],
                ),
                patch.object(fixture.os, "execvpe") as execute,
            ):
                fixture.main()
            env = execute.call_args.args[2]
            self.assertEqual(
                (Path(env["DOCKER_CONFIG"]) / "cli-plugins/docker-compose").resolve(), executable.resolve()
            )


if __name__ == "__main__":
    unittest.main()
