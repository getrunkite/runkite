"""Coordinated source versions and pre-publication tag checks, without services."""

import importlib.util
import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

SOURCE = Path(__file__).resolve().parents[2] / "scripts/check-release-version.py"
spec = importlib.util.spec_from_file_location("release_version", SOURCE)
checker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checker)
VERSION = "1.2.3"


def write(root, relative, value):
    path = root / relative
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(value, encoding="utf-8")


def write_json(root, relative, value):
    write(root, relative, json.dumps(value))


def fixture(root, version=VERSION):
    write(root, "VERSION", version + "\n")
    write(root, "python/runkite_runner/__init__.py", f'__version__ = "{version}"\n')
    for path in ["python/pyproject.toml", "python/runkite_governance/pyproject.toml"]:
        write(root, path, f'[project]\nversion = "{version}"\n')
    write(root, "deploy/helm/runkite/Chart.yaml", f'version: {version}\nappVersion: "{version}"\n')
    for name, relative in checker.PACKAGES.items():
        write_json(root, relative + "/package.json", {"name": name, "version": version})
        write_json(root, relative + "/package-lock.json", {"version": version, "packages": {"": {"version": version}}})
        example = "examples/" + name
        linked = "../../" + relative
        write_json(root, example + "/package.json", {"version": "0.1.0", "devDependencies": {name: "file:" + linked}})
        write_json(
            root,
            example + "/package-lock.json",
            {"version": "0.1.0", "packages": {"": {"version": "0.1.0"}, linked: {"version": version}}},
        )
    write_json(root, "admin-ui/package.json", {"version": "9.9.9"})
    write_json(root, "runner-protocol/package.json", {"version": "9.9.9"})
    write_json(root, "spec/openapi.json", {"info": {"version": "9.9.9"}})
    write_json(root, "examples/unrelated/package.json", {"version": "9.9.9"})


class ReleaseVersionTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="release-version-")
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        fixture(self.root)

    def test_coordinated_versions_match_while_other_versions_are_independent(self):
        self.assertEqual(checker.check_release_versions(self.root), [])
        self.assertEqual(checker.check_release_versions(self.root, "v" + VERSION), [])

    def test_valid_prerelease_and_build_identifiers_preserve_exact_tag_matching(self):
        for version in ["1.2.3-rc.1", "1.2.3-0.alpha-1+build.002", "1.2.3+2026.11"]:
            with self.subTest(version=version):
                fixture(self.root, version)
                self.assertEqual(checker.check_release_versions(self.root, "v" + version), [])
                self.assertTrue(checker.check_release_versions(self.root, "v" + VERSION))

    def test_missing_and_malformed_version_cannot_authorize_a_release_tag(self):
        for version in [
            "",
            "v1.2.3",
            "1.2",
            "01.2.3",
            "1.2.3-..",
            "1.2.3-01",
            "1.2.3-rc..1",
            "1.2.3+.",
            "1.2.3+build..1",
            "1.2.3\n1.2.4",
        ]:
            with self.subTest(version=version):
                write(self.root, "VERSION", version)
                errors = checker.check_release_versions(self.root, "v" + VERSION)
                self.assertTrue(errors)
                self.assertTrue(all(error.startswith("VERSION:") for error in errors), errors)
        (self.root / "VERSION").unlink()
        self.assertTrue(checker.check_release_versions(self.root, "v" + VERSION)[0].startswith("VERSION:"))

    def test_each_coordinated_version_is_checked(self):
        cases = [
            ("python/runkite_runner/__init__.py", "python"),
            ("python/pyproject.toml", "toml"),
            ("python/runkite_governance/pyproject.toml", "toml"),
            ("deploy/helm/runkite/Chart.yaml", "chart"),
        ]
        for relative in checker.PACKAGES.values():
            cases.extend(
                [
                    (relative + "/package.json", "manifest"),
                    (relative + "/package-lock.json", "lock-top"),
                    (relative + "/package-lock.json", "lock-root"),
                ]
            )
        for path, kind in cases:
            with self.subTest(path=path, kind=kind):
                fixture(self.root)
                if kind == "python":
                    write(self.root, path, '__version__ = "1.2.2"\n')
                elif kind == "toml":
                    write(self.root, path, '[project]\nversion = "1.2.2"\n')
                elif kind == "chart":
                    for field in ["version", "appVersion"]:
                        write(
                            self.root,
                            path,
                            f'version: {VERSION}\nappVersion: "{VERSION}"\n'.replace(
                                f'{field}: "{VERSION}"' if field == "appVersion" else f"{field}: {VERSION}",
                                f"{field}: 1.2.2",
                            ),
                        )
                        self.assertTrue(checker.check_release_versions(self.root))
                    continue
                else:
                    value = json.loads((self.root / path).read_text())
                    if kind == "lock-root":
                        value["packages"][""]["version"] = "1.2.2"
                    else:
                        value["version"] = "1.2.2"
                    write_json(self.root, path, value)
                errors = checker.check_release_versions(self.root)
                self.assertTrue(any(path in error and "1.2.2" in error for error in errors), errors)

    def test_linked_package_snapshots_cannot_stay_stale_or_disappear(self):
        for name, relative in checker.PACKAGES.items():
            path = "examples/" + name + "/package-lock.json"
            key = "../../" + relative
            for missing in [False, True]:
                with self.subTest(package=name, missing=missing):
                    fixture(self.root)
                    value = json.loads((self.root / path).read_text())
                    if missing:
                        del value["packages"][key]
                    else:
                        value["packages"][key]["version"] = "1.2.2"
                    write_json(self.root, path, value)
                    self.assertTrue(any(path in error for error in checker.check_release_versions(self.root)))

    def test_missing_metadata_and_nonliteral_python_version_fail_closed(self):
        (self.root / "typescript/runkite-runner/package-lock.json").unlink()
        write(self.root, "python/runkite_runner/__init__.py", "__version__ = calculate_version()\n")
        errors = checker.check_release_versions(self.root)
        self.assertTrue(any("package-lock.json" in error for error in errors))
        self.assertTrue(any("__init__.py" in error for error in errors))

    def test_cli_blocks_mismatched_tags_before_publication(self):
        target = self.root / "scripts/check-release-version.py"
        target.parent.mkdir()
        shutil.copyfile(SOURCE, target)
        for tag, status in [("v" + VERSION, 0), ("v1.2.2", 1), (VERSION, 1), ("v1.2.3-extra", 1)]:
            with self.subTest(tag=tag):
                result = subprocess.run(
                    [sys.executable, str(target), "--tag", tag], capture_output=True, text=True, timeout=10
                )
                self.assertEqual(result.returncode, status, result.stderr)
                if status:
                    self.assertIn("release tag: expected 'v1.2.3'", result.stderr)
                else:
                    self.assertIn("Release metadata matches VERSION 1.2.3", result.stdout)
        missing = subprocess.run([sys.executable, str(target), "--tag"], capture_output=True, text=True, timeout=10)
        self.assertEqual(missing.returncode, 2)
        self.assertIn("expected one argument", missing.stderr)


if __name__ == "__main__":
    unittest.main()
