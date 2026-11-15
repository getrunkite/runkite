"""Offline release recovery contract. No registry credentials or writes."""

import io
import json
import tarfile
import tempfile
import unittest
import urllib.error
from pathlib import Path
from unittest.mock import patch

import release_artifacts as release


def archive(content=b"public runtime", *, modified_time=0):
    stream = io.BytesIO()
    with tarfile.open(fileobj=stream, mode="w:gz") as output:
        entry = tarfile.TarInfo("package/index.js")
        entry.size = len(content)
        entry.mtime = modified_time
        entry.mode = 0o644
        output.addfile(entry, io.BytesIO(content))
    return stream.getvalue()


class NpmPackMetadataTests(unittest.TestCase):
    def setUp(self):
        self.record = {"name": "runkite-governance", "version": "0.8.0", "filename": "runkite-governance-0.8.0.tgz"}

    def test_build_accepts_legacy_list_and_npm12_keyed_result(self):
        for shape in ([self.record], {self.record["name"]: self.record}):
            with self.subTest(shape=type(shape).__name__), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                source, output = root / "source", root / "artifacts"
                source.mkdir()
                (source / "package.json").write_text(
                    json.dumps(
                        {"name": self.record["name"], "version": self.record["version"], "license": "Apache-2.0"}
                    )
                )

                def pack(command, packed_shape=shape, **_kwargs):
                    self.assertEqual(command[:4], ["npm", "pack", "--ignore-scripts", "--json"])
                    (Path(command[-1]) / self.record["filename"]).write_bytes(archive())
                    return json.dumps(packed_shape).encode()

                with patch.object(release, "_run"), patch.object(release.subprocess, "check_output", side_effect=pack):
                    manifest = release.build("npm", source, output)
                self.assertEqual(manifest["name"], self.record["name"])
                self.assertEqual(manifest["version"], "0.8.0")
                self.assertEqual(manifest["artifacts"][0]["filename"], self.record["filename"])
                self.assertEqual(json.loads((output / "manifest.json").read_text()), manifest)

    def test_pack_rejects_malformed_multiple_and_wrong_identity(self):
        shapes = [
            [],
            [self.record, self.record],
            {},
            {"other-package": self.record},
            {self.record["name"]: self.record, "other-package": self.record},
            [None],
            {self.record["name"]: []},
            [dict(self.record, name="other-package")],
            {self.record["name"]: dict(self.record, version="0.9.0")},
            "unexpected text",
            None,
        ]
        for shape in shapes:
            with self.subTest(shape=shape), self.assertRaisesRegex(release.ArtifactError, "metadata"):
                release._npm_pack_filename(json.dumps(shape).encode(), self.record["name"], self.record["version"])

    def test_pack_rejects_paths_and_invalid_filenames_in_both_formats(self):
        for filename in (
            "../package.tgz",
            "/tmp/package.tgz",
            "dir/package.tgz",
            "dir\\package.tgz",
            "",
            ".",
            "..",
            None,
            42,
            "bad\nname.tgz",
            "package.zip",
        ):
            record = dict(self.record, filename=filename)
            for shape in ([record], {record["name"]: record}):
                with (
                    self.subTest(filename=filename, shape=type(shape).__name__),
                    self.assertRaisesRegex(release.ArtifactError, "filename"),
                ):
                    release._npm_pack_filename(json.dumps(shape).encode(), record["name"], record["version"])


class RegistryRecoveryTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.data = archive()
        self.file = self.root / "runkite-governance-0.7.0.tgz"
        self.file.write_bytes(self.data)
        self.entry = release._record(self.file)
        self.manifest = {
            "format": 1,
            "ecosystem": "npm",
            "name": "runkite-governance",
            "version": "0.7.0",
            "artifacts": [self.entry],
        }
        self.npm = {
            "name": "runkite-governance",
            "version": "0.7.0",
            "dist": {
                "integrity": self.entry["integrity"],
                "tarball": "https://registry.npmjs.org/runkite-governance/-/runkite-governance-0.7.0.tgz",
            },
        }

    def fetch(self, data=None):
        payload = self.data if data is None else data

        def get(url, _limit, **_kwargs):
            return json.dumps(self.npm).encode() if url.endswith("/0.7.0") else payload

        return get

    def test_matching_npm_requires_both_integrity_and_download(self):
        self.assertEqual(release.inspect_registry(self.manifest, self.fetch()), [])
        self.npm["dist"]["integrity"] = "sha512-wrong"
        with self.assertRaisesRegex(release.ArtifactError, "integrity"):
            release.inspect_registry(self.manifest, self.fetch())

    def test_matching_metadata_does_not_hide_different_tarball(self):
        with self.assertRaisesRegex(release.ArtifactError, "tarball"):
            release.inspect_registry(self.manifest, self.fetch(archive(b"changed")))

    def test_absent_is_only_an_explicit_metadata_not_found(self):
        self.assertEqual(release.inspect_registry(self.manifest, lambda *args, **kwargs: None), [self.file.name])
        for status in (401, 403, 404, 429, 500):
            error = urllib.error.HTTPError(
                "https://registry.npmjs.org/p/v", status, "private body", {}, io.BytesIO(b"never print")
            )
            with patch.object(release.urllib.request, "build_opener") as factory:
                factory.return_value.open.side_effect = error
                if status == 404:
                    self.assertIsNone(release.public_get("https://registry.npmjs.org/p/v", 100, missing_ok=True))
                else:
                    with self.assertRaisesRegex(release.ArtifactError, f"HTTP {status}"):
                        release.public_get("https://registry.npmjs.org/p/v", 100, missing_ok=True)

    def test_existing_download_404_and_network_failure_are_not_absence(self):
        with patch.object(release.urllib.request, "build_opener") as factory:
            for failure in (
                urllib.error.URLError("unavailable"),
                urllib.error.HTTPError("https://registry.npmjs.org/p.tgz", 404, "missing", {}, io.BytesIO()),
            ):
                factory.return_value.open.side_effect = failure
                with self.assertRaises(release.ArtifactError):
                    release.public_get("https://registry.npmjs.org/p.tgz", 100)

    def test_registry_cannot_redirect_download_to_another_host(self):
        for url in (
            "http://registry.npmjs.org/p.tgz",
            "https://example.com/p.tgz",
            "https://user:password@registry.npmjs.org/p.tgz",
            "https://registry.npmjs.org/p.tgz?token=value",
        ):
            self.npm["dist"]["tarball"] = url
            with self.assertRaisesRegex(release.ArtifactError, "URL"):
                release.inspect_registry(self.manifest, self.fetch())

    def test_invalid_identity_and_duplicate_json_fail_closed(self):
        self.npm["name"] = "another-package"
        with self.assertRaisesRegex(release.ArtifactError, "identity"):
            release.inspect_registry(self.manifest, self.fetch())
        with self.assertRaisesRegex(release.ArtifactError, "Duplicate"):
            release.inspect_registry(self.manifest, lambda *args, **kwargs: b'{"name":"a","name":"b"}')

    def test_changed_local_artifact_is_never_used_for_recovery(self):
        manifest_path = self.root / "manifest.json"
        manifest_path.write_text(json.dumps(self.manifest))
        self.file.write_bytes(archive(b"modified after build"))
        with patch.object(release, "inspect_registry") as inspect:
            with self.assertRaisesRegex(release.ArtifactError, "changed"):
                release.check(manifest_path)
            inspect.assert_not_called()

    def test_public_requests_ask_for_cache_revalidation(self):
        with patch.object(release.urllib.request, "build_opener") as factory:
            response = factory.return_value.open.return_value.__enter__.return_value
            response.status = 200
            response.read.return_value = b"{}"
            self.assertEqual(release.public_get("https://registry.npmjs.org/package/0.8.0", 100), b"{}")
            request = factory.return_value.open.call_args.args[0]
        self.assertEqual(request.get_header("Cache-control"), "no-cache")
        self.assertEqual(request.get_header("Pragma"), "no-cache")

    def test_check_polls_explicit_missing_until_matching_version_is_visible(self):
        manifest_path = self.root / "manifest.json"
        manifest_path.write_text(json.dumps(self.manifest))
        with (
            patch.object(release, "inspect_registry", side_effect=[[self.file.name], []]) as inspect,
            patch.object(release.time, "monotonic", return_value=0),
            patch.object(release.time, "sleep") as sleep,
            patch.object(release, "_outputs"),
        ):
            result = release.check(manifest_path, require_present=True, wait_seconds=300)
        self.assertEqual(result["state"], "already_matching")
        self.assertEqual(inspect.call_count, 2)
        sleep.assert_called_once_with(1)

    def test_check_does_not_poll_uncertain_or_mismatching_registry_failures(self):
        manifest_path = self.root / "manifest.json"
        manifest_path.write_text(json.dumps(self.manifest))
        for message in ("Public registry connection failed", "npm registry tarball differs from intended artifact"):
            with (
                self.subTest(message=message),
                patch.object(release, "inspect_registry", side_effect=release.ArtifactError(message)) as inspect,
                patch.object(release.time, "sleep") as sleep,
                self.assertRaisesRegex(release.ArtifactError, message),
            ):
                release.check(manifest_path, require_present=True, wait_seconds=300)
            inspect.assert_called_once()
            sleep.assert_not_called()

    def test_check_default_does_not_wait_and_explicit_wait_is_bounded(self):
        manifest_path = self.root / "manifest.json"
        manifest_path.write_text(json.dumps(self.manifest))
        with (
            patch.object(release, "inspect_registry", return_value=[self.file.name]) as inspect,
            patch.object(release.time, "sleep") as sleep,
            patch.object(release, "_outputs"),
        ):
            self.assertEqual(release.check(manifest_path)["state"], "needs_publish")
        inspect.assert_called_once()
        sleep.assert_not_called()
        clock = [0]

        def advance(seconds):
            clock[0] += seconds

        with (
            patch.object(release, "inspect_registry", return_value=[self.file.name]) as inspect,
            patch.object(release.time, "monotonic", side_effect=lambda: clock[0]),
            patch.object(release.time, "sleep", side_effect=advance),
            patch.object(release, "_outputs"),
            self.assertRaisesRegex(release.ArtifactError, "remain unpublished"),
        ):
            release.check(manifest_path, require_present=True, wait_seconds=3)
        self.assertEqual(clock[0], 3)
        self.assertEqual(inspect.call_count, 4)
        for invalid in (-1, 301, 0.5):
            with self.subTest(wait_seconds=invalid), self.assertRaisesRegex(release.ArtifactError, "Registry wait"):
                release.check(manifest_path, wait_seconds=invalid)

    def test_pypi_checks_each_file_and_recovers_only_missing_distribution(self):
        self.manifest["ecosystem"] = "pypi"
        second = dict(self.entry, filename="runkite_governance-0.7.0.whl")
        self.manifest["artifacts"].append(second)
        remote = {
            "info": {"name": "runkite_governance", "version": "0.7.0"},
            "urls": [
                {
                    "filename": self.file.name,
                    "url": "https://files.pythonhosted.org/p.tgz",
                    "digests": {"sha256": release.sha256(self.data)},
                }
            ],
        }

        def get(url, _limit, **_kwargs):
            return json.dumps(remote).encode() if url.endswith("/json") else self.data

        self.assertEqual(release.inspect_registry(self.manifest, get), [second["filename"]])
        remote["urls"][0]["digests"]["sha256"] = "0" * 64
        with self.assertRaisesRegex(release.ArtifactError, "digest"):
            release.inspect_registry(self.manifest, get)

    def test_pypi_accepts_container_dates_but_never_changed_member_bytes(self):
        self.manifest["ecosystem"] = "pypi"

        def get_for(data):
            remote = {
                "info": {"name": "runkite-governance", "version": "0.7.0"},
                "urls": [
                    {
                        "filename": self.file.name,
                        "url": "https://files.pythonhosted.org/p.tgz",
                        "digests": {"sha256": release.sha256(data)},
                    }
                ],
            }
            return lambda url, _limit, **_kwargs: json.dumps(remote).encode() if url.endswith("/json") else data

        self.assertEqual(release.inspect_registry(self.manifest, get_for(archive(modified_time=123))), [])
        with self.assertRaisesRegex(release.ArtifactError, "contents"):
            release.inspect_registry(self.manifest, get_for(archive(b"different", modified_time=123)))


if __name__ == "__main__":
    unittest.main()
