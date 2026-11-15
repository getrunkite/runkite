#!/usr/bin/env python3
"""Adversarial metadata checks without GitHub requests or publication."""
import copy
import io
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import prepare_release_recovery as recovery


class RecoveryTests(unittest.TestCase):
    tag = "v0.8.0"
    run_id = "43695507859"
    sha = "c63b8203123f6b7d032b0b5aca59abe9227fd973"
    prefix = "/repos/getrunkite/runkite"

    def setUp(self):
        self.ref_path = self.prefix + "/git/ref/tags/" + self.tag
        self.run_path = self.prefix + "/actions/runs/" + self.run_id
        self.jobs_path = self.run_path + "/jobs?filter=latest&per_page=100"
        self.release_path = self.prefix + "/releases/tags/" + self.tag
        self.records = {
            self.ref_path: {"ref": "refs/tags/" + self.tag,
                            "object": {"type": "commit", "sha": self.sha}},
            self.run_path: {"id": int(self.run_id), "repository": {"full_name": recovery.REPOSITORY},
                            "event": "push", "head_branch": self.tag, "head_sha": self.sha,
                            "path": recovery.WORKFLOW, "status": "in_progress"},
            self.jobs_path: {"total_count": 2, "jobs": [
                {"name": name, "status": "completed", "conclusion": "success"}
                for name in ("version-check", "goreleaser")]},
            self.release_path: {"tag_name": self.tag, "draft": False,
                                "published_at": "2026-11-15T07:00:00Z"},
        }

    def prepare(self, **kwargs):
        return recovery.prepare(kwargs.get("tag", self.tag), kwargs.get("run_id", self.run_id),
                                kwargs.get("repository", recovery.REPOSITORY),
                                get=lambda endpoint: copy.deepcopy(self.records[endpoint]))

    def test_success_while_docker_is_in_progress(self):
        self.assertEqual(self.prepare(), {"tag": self.tag, "version": "0.8.0",
                                         "source_revision": self.sha, "source_run_id": self.run_id})

    def test_rejects_bad_inputs_before_any_api_request(self):
        def unavailable(_endpoint):
            self.fail("Invalid inputs must not make API calls")
        for tag in ("0.8.0", "v0.8.0-rc.1", "v00.8.0", "v0.8.0\n", "v0.8.0/../main"):
            with self.subTest(tag=tag), self.assertRaises(recovery.RecoveryError):
                recovery.prepare(tag, self.run_id, recovery.REPOSITORY, unavailable)
        for run_id in ("0", "01", "-1", "1e3", "1\n", "1;true"):
            with self.subTest(run_id=run_id), self.assertRaises(recovery.RecoveryError):
                recovery.prepare(self.tag, run_id, recovery.REPOSITORY, unavailable)
        with self.assertRaises(recovery.RecoveryError):
            recovery.prepare(self.tag, self.run_id, "fork/runkite", unavailable)

    def test_rejects_wrong_run_provenance(self):
        for field, value in (("event", "workflow_dispatch"), ("head_branch", "main"),
                             ("head_sha", "a" * 40), ("path", ".github/workflows/other.yml"),
                             ("path", recovery.WORKFLOW + "-other"), ("id", 42),
                             ("repository", {"full_name": "fork/runkite"}), ("repository", None)):
            with self.subTest(field=field, value=value):
                original = self.records[self.run_path][field]
                self.records[self.run_path][field] = value
                with self.assertRaises(recovery.RecoveryError):
                    self.prepare()
                self.records[self.run_path][field] = original

    def test_allows_documented_workflow_ref_suffix(self):
        self.records[self.run_path]["path"] += "@refs/tags/v0.8.0"
        self.assertEqual(self.prepare()["source_revision"], self.sha)

    def test_annotated_tag_peels_at_most_four_times(self):
        target = self.records[self.ref_path]["object"]
        for index in range(4):
            object_sha = str(index + 1) * 40
            target.update(type="tag", sha=object_sha)
            record = {"object": {"type": "commit", "sha": self.sha}}
            self.records[self.prefix + "/git/tags/" + object_sha] = record
            target = record["object"]
        self.assertEqual(self.prepare()["source_revision"], self.sha)
        target.update(type="tag", sha="9" * 40)
        with self.assertRaisesRegex(recovery.RecoveryError, "four"):
            self.prepare()

    def test_rejects_bad_tag_targets(self):
        for target in ({"type": "tree", "sha": self.sha}, {"type": "commit", "sha": "abc"}, None):
            self.records[self.ref_path]["object"] = target
            with self.subTest(target=target), self.assertRaises(recovery.RecoveryError):
                self.prepare()

    def test_requires_both_successful_original_jobs(self):
        for state in ("failure", "cancelled", "skipped", None):
            self.records[self.jobs_path]["jobs"][1]["conclusion"] = state
            with self.subTest(state=state), self.assertRaises(recovery.RecoveryError):
                self.prepare()
        self.records[self.jobs_path]["jobs"][1]["conclusion"] = "success"
        self.records[self.jobs_path]["jobs"][1]["status"] = "in_progress"
        with self.assertRaises(recovery.RecoveryError):
            self.prepare()

    def test_rejects_incomplete_or_duplicate_job_lists(self):
        for replacement in ({"total_count": 101, "jobs": []},
                            {"total_count": 3, "jobs": self.records[self.jobs_path]["jobs"]},
                            {"total_count": 2, "jobs": [self.records[self.jobs_path]["jobs"][0]] * 2}):
            self.records[self.jobs_path] = replacement
            with self.assertRaises(recovery.RecoveryError):
                self.prepare()

    def test_requires_published_matching_release(self):
        for field, value in (("draft", True), ("draft", None), ("published_at", None),
                             ("published_at", ""), ("tag_name", "v0.7.0")):
            original = self.records[self.release_path][field]
            self.records[self.release_path][field] = value
            with self.subTest(field=field, value=value), self.assertRaises(recovery.RecoveryError):
                self.prepare()
            self.records[self.release_path][field] = original

    def test_github_cli_is_read_only_and_never_prints_error_body(self):
        response = subprocess.CompletedProcess([], 1, b"", b"sensitive provider error")
        with patch.object(recovery.subprocess, "run", return_value=response) as command:
            with self.assertRaisesRegex(recovery.RecoveryError, "metadata request failed"):
                recovery.github_json(self.run_path)
            args = command.call_args.args[0]
            self.assertEqual(args[:7], ["gh", "api", "--hostname", "github.com", "--method", "GET", self.run_path])

    def test_outputs_written_only_after_full_validation(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "output"
            args = ["prepare_release_recovery.py", "--tag", self.tag, "--source-run-id", self.run_id]
            with patch.dict(os.environ, {"GITHUB_REPOSITORY": recovery.REPOSITORY, "GITHUB_OUTPUT": str(output)}), patch.object(recovery.sys, "argv", args), patch.object(recovery, "prepare", side_effect=recovery.RecoveryError("Denied")), patch("sys.stderr", io.StringIO()):
                self.assertEqual(recovery.main(), 1)
                self.assertFalse(output.exists())
            values = self.prepare()
            with patch.dict(os.environ, {"GITHUB_REPOSITORY": recovery.REPOSITORY, "GITHUB_OUTPUT": str(output)}), patch.object(recovery.sys, "argv", args), patch.object(recovery, "prepare", return_value=values), patch("sys.stdout", io.StringIO()):
                self.assertEqual(recovery.main(), 0)
            self.assertEqual(output.read_text(), "".join(f"{key}={value}\n" for key, value in values.items()))

    def test_manifest_identity_and_file_set(self):
        for ecosystem in ("pypi", "npm"):
            for name in ("runkite-runner", "runkite-governance"):
                stem = name.replace("-", "_")
                filenames = ([stem + "-0.8.0-py3-none-any.whl", stem + "-0.8.0.tar.gz"]
                             if ecosystem == "pypi" else [name + "-0.8.0.tgz"])
                manifest = {"format": 1, "name": name, "version": "0.8.0", "ecosystem": ecosystem,
                            "artifacts": [{"filename": filename, "sha256": "a" * 64} for filename in filenames]}
                with tempfile.TemporaryDirectory() as temporary:
                    path = Path(temporary) / "manifest.json"
                    path.write_text(json.dumps(manifest))
                    self.assertEqual(recovery.validate_intended_manifest(path, name, "0.8.0", ecosystem), manifest)
                    for field, value in (("name", "other"), ("version", "0.7.0"), ("ecosystem", "other")):
                        bad = dict(manifest)
                        bad[field] = value
                        path.write_text(json.dumps(bad))
                        with self.subTest(field=field), self.assertRaises(recovery.RecoveryError):
                            recovery.validate_intended_manifest(path, name, "0.8.0", ecosystem)
                    manifest["artifacts"][0]["filename"] = "../outside"
                    path.write_text(json.dumps(manifest))
                    with self.assertRaises(recovery.RecoveryError):
                        recovery.validate_intended_manifest(path, name, "0.8.0", ecosystem)

    def test_strict_json_rejects_duplicates(self):
        with self.assertRaises(recovery.RecoveryError):
            recovery.strict_json(b'{"name":"a","name":"b"}')


if __name__ == "__main__":
    unittest.main()
