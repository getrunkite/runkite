"""Build and verify the independent decision helper, then use installed copies.

Run with pinned requirements-build.txt tools. Set
RUNKITE_GOVERNANCE_FRAMEWORK_PYTHON to an interpreter with the tested optional
Microsoft SDK installed. No provider or paid model is used.
"""

from __future__ import annotations

import email
import importlib.util
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

ROOT = Path(__file__).resolve().parents[2]
SOURCE = ROOT / "python/runkite_governance"
RUNTIME = {"__init__.py", "agent_framework.py", "py.typed"}


class GovernanceArtifactTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temporary = tempfile.TemporaryDirectory(prefix="runkite-governance-artifact-")
        cls.addClassCleanup(cls.temporary.cleanup)
        cls.root = Path(cls.temporary.name)
        cls.project = tomllib.loads((SOURCE / "pyproject.toml").read_text())["project"]
        configured = os.environ.get("RUNKITE_GOVERNANCE_PYTHON_ARTIFACTS")
        cls.artifacts = Path(configured).resolve() if configured else cls.root / "artifacts"
        if not configured:
            spec = importlib.util.spec_from_file_location("release_artifacts", ROOT / "scripts/release_artifacts.py")
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            source = cls.root / "source"
            shutil.copytree(SOURCE, source, ignore=shutil.ignore_patterns("__pycache__", "*.egg-info", "build", "dist"))
            for name in ("test_packaging_fixture.py", "e2e_packaging_fixture.py"):
                (source / name).write_text("raise RuntimeError('source-only fixture')\n")
            (source / "tests").mkdir()
            (source / "tests/private_fixture.txt").write_text("source-only fixture\n")
            module.build("pypi", source, cls.artifacts)
        [cls.wheel] = cls.artifacts.glob("*.whl")
        [cls.sdist] = cls.artifacts.glob("*.tar.gz")
        with ZipFile(cls.wheel) as archive:
            cls.wheel_files = {name: archive.read(name) for name in archive.namelist() if not name.endswith("/")}
        with tarfile.open(cls.sdist) as archive:
            regular = [member for member in archive.getmembers() if member.isfile()]
            [prefix] = {member.name.split("/", 1)[0] for member in regular}
            cls.sdist_files = {
                member.name.removeprefix(prefix + "/"): archive.extractfile(member).read() for member in regular
            }
        cls.framework_python = os.environ.get("RUNKITE_GOVERNANCE_FRAMEWORK_PYTHON", sys.executable)

    def run_command(self, command):
        env = {**os.environ, "PYTHONDONTWRITEBYTECODE": "1", "PIP_DISABLE_PIP_VERSION_CHECK": "1"}
        env.pop("PYTHONPATH", None)
        result = subprocess.run(command, cwd=self.root, env=env, capture_output=True, text=True, timeout=120)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        return result.stdout

    def test_allowlists_exact_runtime_and_licenses(self):
        metadata_root = f"runkite_governance-{self.project['version']}.dist-info"
        self.assertEqual(
            set(self.wheel_files),
            {
                *(f"runkite_governance/{name}" for name in RUNTIME),
                *(
                    f"{metadata_root}/{name}"
                    for name in ("METADATA", "WHEEL", "RECORD", "top_level.txt", "licenses/LICENSE", "licenses/NOTICE")
                ),
            },
        )
        actual = {
            name.removeprefix("runkite_governance/")
            for name in self.wheel_files
            if name.startswith("runkite_governance/")
        }
        self.assertEqual(actual, RUNTIME)
        for name in RUNTIME:
            self.assertEqual(self.wheel_files[f"runkite_governance/{name}"], (SOURCE / name).read_bytes())
            self.assertEqual(self.sdist_files[name], (SOURCE / name).read_bytes())
        self.assertEqual(
            set(self.sdist_files),
            {
                *RUNTIME,
                "LICENSE",
                "NOTICE",
                "README.md",
                "pyproject.toml",
                "MANIFEST.in",
                "PKG-INFO",
                "setup.cfg",
                "runkite_governance.egg-info/PKG-INFO",
                "runkite_governance.egg-info/SOURCES.txt",
                "runkite_governance.egg-info/dependency_links.txt",
                "runkite_governance.egg-info/requires.txt",
                "runkite_governance.egg-info/top_level.txt",
            },
        )
        for name in ("LICENSE", "NOTICE"):
            [data] = [data for path, data in self.wheel_files.items() if path.endswith(f".dist-info/licenses/{name}")]
            self.assertEqual(data, (SOURCE / name).read_bytes())
            self.assertEqual(self.sdist_files[name], data)
        self.assertFalse(any("test" in part or "e2e" in part for name in self.wheel_files for part in Path(name).parts))

    def test_metadata_and_optional_framework_stay_independent(self):
        [content] = [content for name, content in self.wheel_files.items() if name.endswith(".dist-info/METADATA")]
        for raw in (content, self.sdist_files["PKG-INFO"]):
            metadata = email.message_from_bytes(raw)
            self.assertEqual(metadata["Name"], "runkite-governance")
            self.assertEqual(metadata["Version"], self.project["version"])
            self.assertEqual(metadata["License-Expression"], "Apache-2.0")
            self.assertEqual(set(metadata.get_all("License-File")), {"LICENSE", "NOTICE"})
            self.assertEqual(metadata["Requires-Python"], ">=3.11")
            self.assertEqual(metadata.get_all("Provides-Extra"), ["agent-framework"])
            self.assertEqual(
                metadata.get_all("Requires-Dist"), ['agent-framework-core==1.23.0; extra == "agent-framework"']
            )
        self.assertFalse(any(name.endswith("entry_points.txt") for name in self.wheel_files))

    def test_installed_wheel_and_rebuilt_sdist_behavior_outside_checkout(self):
        rebuilt = self.root / "rebuilt"
        self.run_command(
            [
                sys.executable,
                "-m",
                "pip",
                "wheel",
                "--no-index",
                "--no-deps",
                "--no-build-isolation",
                "--no-cache-dir",
                "--wheel-dir",
                str(rebuilt),
                str(self.sdist),
            ]
        )
        [from_sdist] = rebuilt.glob("*.whl")
        for index, wheel in enumerate((self.wheel, from_sdist)):
            with self.subTest(distribution=wheel.name, source="wheel" if index == 0 else "sdist"):
                installed = self.root / f"installed-{index}"
                self.run_command(
                    [
                        sys.executable,
                        "-m",
                        "pip",
                        "install",
                        "--no-index",
                        "--no-deps",
                        "--no-compile",
                        "--target",
                        str(installed),
                        str(wheel),
                    ]
                )
                bootstrap = (
                    "import sys,pathlib,runpy; "
                    f"sys.path.insert(0,{str(installed)!r}); "
                    "import runkite_governance as package; "
                    f"assert pathlib.Path(package.__file__).resolve().is_relative_to(pathlib.Path({str(installed)!r}).resolve()); "
                )
                self.run_command(
                    [sys.executable, "-I", "-c", bootstrap + "assert 'agent_framework' not in sys.modules"]
                )
                for source, interpreter in (
                    (ROOT / "python/tests/test_governance.py", sys.executable),
                    (ROOT / "python/tests/test_governance_deadlines.py", sys.executable),
                    (ROOT / "python/tests/test_governance_agent_framework.py", self.framework_python),
                    (ROOT / "examples/governance_agent_framework/test_integration.py", self.framework_python),
                ):
                    copied = self.root / f"case-{index}-{source.name}"
                    shutil.copyfile(source, copied)
                    # Copied tests can prepend their temporary parent, never the
                    # repository. The helper was already imported from the wheel.
                    requirement = "import agent_framework; " if interpreter == self.framework_python else ""
                    self.run_command(
                        [
                            interpreter,
                            "-I",
                            "-c",
                            bootstrap
                            + requirement
                            + f"sys.argv=[{str(copied)!r}]; runpy.run_path({str(copied)!r},run_name='__main__')",
                        ]
                    )


if __name__ == "__main__":
    unittest.main()
