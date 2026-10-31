"""Build the Python sdist, rebuild its wheel offline, and inspect public artifacts.

Run with a build environment containing build, setuptools, wheel and pip:
    python python/tests/test_package_artifacts.py
The test copies sources to a temporary directory and never installs dependencies.
"""

from __future__ import annotations

import configparser
import email
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path
from zipfile import ZipFile

import tomllib
from packaging.requirements import Requirement
from packaging.utils import canonicalize_name

ADAPTER_MODULES = {
    "agent_framework_adapter": {"__init__.py", "__main__.py", "adapter.py", "state.py"},
    "openai_agents_adapter": {"__init__.py", "__main__.py", "adapter.py", "state.py"},
    "pydantic_ai_adapter": {"__init__.py", "__main__.py", "adapter.py", "state.py"},
    "google_adk_adapter": {"__init__.py", "__main__.py", "adapter.py", "state.py", "sessions.py"},
}
BASE_DEPENDENCIES = {
    "grpcio",
    "protobuf",
    "langgraph",
    "langgraph-sdk",
    "langchain-core",
    "langgraph-checkpoint-postgres",
    "psycopg",
    "httpx",
    "uvicorn",
}


class PackageArtifactTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temporary = tempfile.TemporaryDirectory(prefix="runkite-python-artifact-")
        cls.addClassCleanup(cls.temporary.cleanup)
        cls.root = Path(cls.temporary.name)
        cls.source = cls.root / "source"
        shutil.copytree(
            Path(__file__).resolve().parents[1],
            cls.source,
            ignore=shutil.ignore_patterns(
                ".venv", "__pycache__", "*.egg-info", "build", "dist", "node_modules", ".ruff_cache"
            ),
        )
        cls.project = tomllib.loads((cls.source / "pyproject.toml").read_text())["project"]
        # Traps also prove exclusion when this check is run from an extracted sdist.
        for name in ADAPTER_MODULES:
            adapter = cls.source / "adapters" / name
            (adapter / "test_packaging_fixture.py").write_text("raise RuntimeError('checkout fixture')\n")
            (adapter / "e2e_packaging_fixture.py").write_text("raise RuntimeError('live proof fixture')\n")
            seed = adapter / "e2e_seed"
            seed.mkdir(exist_ok=True)
            (seed / "packaging-fixture.txt").write_text("source-only proof fixture\n")
        cls.env = {**os.environ, "PYTHONDONTWRITEBYTECODE": "1", "PIP_DISABLE_PIP_VERSION_CHECK": "1"}
        cls.env.pop("PYTHONPATH", None)
        cls.run_build("-m", "build", "--sdist", "--no-isolation", "--outdir", str(cls.root / "sdist"), str(cls.source))
        [cls.sdist] = (cls.root / "sdist").glob("*.tar.gz")
        cls.run_build(
            "-m",
            "pip",
            "wheel",
            "--no-index",
            "--no-deps",
            "--no-build-isolation",
            "--no-cache-dir",
            "--wheel-dir",
            str(cls.root / "wheel"),
            str(cls.sdist),
        )
        [cls.wheel] = (cls.root / "wheel").glob("*.whl")
        with ZipFile(cls.wheel) as wheel:
            cls.members = {name: wheel.read(name) for name in wheel.namelist() if not name.endswith("/")}
        [metadata] = [content for name, content in cls.members.items() if name.endswith(".dist-info/METADATA")]
        cls.metadata = email.message_from_bytes(metadata)

    @classmethod
    def run_build(cls, *args: str) -> None:
        command = [sys.executable, *args]
        result = subprocess.run(command, cwd=cls.root, env=cls.env, capture_output=True, text=True, timeout=120)
        if result.returncode:
            raise AssertionError(f"Artifact build failed: {command!r}\n{result.stdout}\n{result.stderr}")

    def test_artifacts_preserve_apache_license_and_notice(self) -> None:
        self.assertEqual(self.metadata["License-Expression"], "Apache-2.0")
        self.assertEqual(set(self.metadata.get_all("License-File", [])), {"LICENSE", "NOTICE"})
        self.assertNotIn("License :: Other/Proprietary License", self.metadata.get_all("Classifier", []))
        with tarfile.open(self.sdist) as sdist:
            [prefix] = {member.name.split("/", 1)[0] for member in sdist.getmembers() if member.isfile()}
            for name in ("LICENSE", "NOTICE"):
                expected = (self.source / name).read_bytes()
                [content] = [data for path, data in self.members.items() if path.endswith(f".dist-info/licenses/{name}")]
                self.assertEqual(content, expected, name)
                member = sdist.extractfile(f"{prefix}/{name}")
                self.assertIsNotNone(member)
                self.assertEqual(member.read(), expected, name)

    def test_wheel_preserves_runner_code_and_contains_only_adapter_runtime_modules(self) -> None:
        for source in (self.source / "runkite_runner").rglob("*"):
            if source.is_file() and (source.suffix == ".py" or source.name == "py.typed"):
                name = source.relative_to(self.source).as_posix()
                self.assertEqual(self.members.get(name), source.read_bytes(), name)
        for package, modules in ADAPTER_MODULES.items():
            adapter_members = {
                name.removeprefix(f"{package}/") for name in self.members if name.startswith(f"{package}/")
            }
            self.assertEqual(adapter_members, modules, package)
            for name in modules:
                self.assertEqual(
                    self.members[f"{package}/{name}"],
                    (self.source / "adapters" / package / name).read_bytes(),
                    f"{package}/{name}",
                )
        for name in self.members:
            self.assertTrue(
                name.startswith(("runkite_runner/", *(f"{package}/" for package in ADAPTER_MODULES)))
                or ".dist-info/" in name,
                name,
            )

    def test_sdist_contains_every_runtime_source_without_adapter_tests_or_live_fixtures(self) -> None:
        with tarfile.open(self.sdist) as sdist:
            regular = [member for member in sdist.getmembers() if member.isfile()]
            [prefix] = {member.name.split("/", 1)[0] for member in regular}
            for package, modules in ADAPTER_MODULES.items():
                adapter_members = {
                    member.name.removeprefix(f"{prefix}/adapters/{package}/")
                    for member in regular
                    if member.name.startswith(f"{prefix}/adapters/{package}/")
                }
                self.assertEqual(adapter_members, modules, package)
                for name in modules:
                    member = sdist.extractfile(f"{prefix}/adapters/{package}/{name}")
                    self.assertIsNotNone(member)
                    self.assertEqual(member.read(), (self.source / "adapters" / package / name).read_bytes())
            for member in regular:
                if member.name.startswith(f"{prefix}/runkite_runner/"):
                    data = sdist.extractfile(member)
                    self.assertIsNotNone(data)
                    self.assertEqual(data.read(), self.members[member.name.removeprefix(f"{prefix}/")])

    def test_metadata_keeps_adapters_optional_and_preserves_the_langgraph_entrypoint_and_dependencies(self) -> None:
        self.assertEqual(self.metadata["Name"], "runkite-runner")
        self.assertEqual(self.metadata["Version"], self.project["version"])
        self.assertEqual(self.metadata["Requires-Python"], ">=3.11")
        self.assertEqual(
            set(self.metadata.get_all("Provides-Extra")),
            {"agent-framework", "openai-agents", "pydantic-ai", "google-adk", "otel"},
        )
        requirements = [Requirement(value) for value in self.metadata.get_all("Requires-Dist")]
        base = {
            canonicalize_name(requirement.name): requirement
            for requirement in requirements
            if requirement.marker is None
        }
        self.assertEqual(set(base), BASE_DEPENDENCIES)
        self.assertEqual(base["psycopg"].extras, {"binary", "pool"})
        [framework] = [
            requirement for requirement in requirements if canonicalize_name(requirement.name) == "agent-framework-core"
        ]
        self.assertEqual(str(framework.specifier), "==1.23.0")
        self.assertIsNotNone(framework.marker)
        self.assertTrue(framework.marker.evaluate({"extra": "agent-framework"}))
        self.assertFalse(framework.marker.evaluate({"extra": ""}))
        self.assertFalse(framework.marker.evaluate({"extra": "otel"}))
        self.assertFalse(framework.marker.evaluate({"extra": "openai-agents"}))
        self.assertFalse(framework.marker.evaluate({"extra": "pydantic-ai"}))
        self.assertFalse(framework.marker.evaluate({"extra": "google-adk"}))
        [openai] = [
            requirement for requirement in requirements if canonicalize_name(requirement.name) == "openai-agents"
        ]
        self.assertEqual(str(openai.specifier), "==0.24.0")
        self.assertIsNotNone(openai.marker)
        self.assertTrue(openai.marker.evaluate({"extra": "openai-agents"}))
        for extra in ("", "otel", "agent-framework", "pydantic-ai", "google-adk"):
            self.assertFalse(openai.marker.evaluate({"extra": extra}))
        [pydantic_ai] = [
            requirement for requirement in requirements if canonicalize_name(requirement.name) == "pydantic-ai-slim"
        ]
        self.assertEqual(str(pydantic_ai.specifier), "==2.63.0")
        self.assertEqual(pydantic_ai.extras, set(), "Provider SDKs belong to the app, not the core runner extra")
        self.assertIsNotNone(pydantic_ai.marker)
        self.assertTrue(pydantic_ai.marker.evaluate({"extra": "pydantic-ai"}))
        for extra in ("", "otel", "agent-framework", "openai-agents", "google-adk"):
            self.assertFalse(pydantic_ai.marker.evaluate({"extra": extra}))
        [google_adk] = [
            requirement for requirement in requirements if canonicalize_name(requirement.name) == "google-adk"
        ]
        self.assertEqual(str(google_adk.specifier), "==2.13.0")
        self.assertEqual(google_adk.extras, set())
        self.assertIsNotNone(google_adk.marker)
        self.assertTrue(google_adk.marker.evaluate({"extra": "google-adk"}))
        for extra in ("", "otel", "agent-framework", "openai-agents", "pydantic-ai"):
            self.assertFalse(google_adk.marker.evaluate({"extra": extra}))
        [entry_points] = [
            content for name, content in self.members.items() if name.endswith(".dist-info/entry_points.txt")
        ]
        parser = configparser.ConfigParser()
        parser.read_string(entry_points.decode())
        self.assertEqual(
            dict(parser["console_scripts"]),
            {
                "runkite-runner": "runkite_runner.worker:main",
                "runkite-runner-agent-framework": "agent_framework_adapter.__main__:main",
                "runkite-runner-openai-agents": "openai_agents_adapter.__main__:main",
                "runkite-runner-pydantic-ai": "pydantic_ai_adapter.__main__:main",
                "runkite-runner-google-adk": "google_adk_adapter.__main__:main",
            },
        )


if __name__ == "__main__":
    unittest.main()
