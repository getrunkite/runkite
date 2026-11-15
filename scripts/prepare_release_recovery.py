#!/usr/bin/env python3
"""Authorize package-only recovery from a published tag and original release run."""
import argparse
import json
import os
import re
import subprocess
import sys
from pathlib import Path

REPOSITORY = "getrunkite/runkite"
WORKFLOW = ".github/workflows/release.yml"
MAX_JSON = 8 * 1024 * 1024


class RecoveryError(Exception):
    """A safe recovery diagnostic without credentials or provider response bodies."""


def strict_json(data):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise RecoveryError("Duplicate JSON field")
            result[key] = value
        return result
    if len(data) > MAX_JSON:
        raise RecoveryError("Recovery JSON exceeds size limit")
    try:
        return json.loads(data, object_pairs_hook=unique)
    except (ValueError, UnicodeError) as error:
        raise RecoveryError("Invalid recovery JSON") from error


def github_json(endpoint):
    result = subprocess.run(
        ["gh", "api", "--hostname", "github.com", "--method", "GET", endpoint],
        capture_output=True, timeout=45, check=False,
    )
    if result.returncode:
        raise RecoveryError("GitHub recovery metadata request failed")
    return strict_json(result.stdout)


def prepare(tag, source_run_id, repository, get=github_json):
    if repository != REPOSITORY:
        raise RecoveryError("Recovery is restricted to getrunkite/runkite")
    if not isinstance(tag, str) or not re.fullmatch(r"v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)", tag):
        raise RecoveryError("Recovery tag must be a stable vN.N.N version")
    if not isinstance(source_run_id, str) or not re.fullmatch(r"[1-9][0-9]*", source_run_id):
        raise RecoveryError("Source run ID must be a positive decimal")
    if len(tag) > 64 or len(source_run_id) > 20:
        raise RecoveryError("Recovery identifier exceeds size limit")
    prefix = "/repos/" + REPOSITORY
    reference = get(prefix + "/git/ref/tags/" + tag)
    if not isinstance(reference, dict) or reference.get("ref") != "refs/tags/" + tag:
        raise RecoveryError("Tag reference does not match recovery tag")
    target = reference.get("object")
    for depth in range(5):
        if not isinstance(target, dict) or not isinstance(target.get("sha"), str) or not re.fullmatch(r"[0-9a-f]{40}", target["sha"]):
            raise RecoveryError("Tag target is not a full Git commit or tag SHA")
        if target.get("type") == "commit":
            revision = target["sha"]
            break
        if target.get("type") != "tag" or depth == 4:
            raise RecoveryError("Tag cannot be peeled within four annotated tags")
        tagged = get(prefix + "/git/tags/" + target["sha"])
        target = tagged.get("object") if isinstance(tagged, dict) else None
    run = get(prefix + "/actions/runs/" + source_run_id)
    if not isinstance(run, dict):
        raise RecoveryError("Original run metadata is invalid")
    path = run.get("path")
    run_repository = run.get("repository")
    if (type(run.get("id")) is not int or run["id"] != int(source_run_id)
            or not isinstance(run_repository, dict) or run_repository.get("full_name") != REPOSITORY
            or run.get("event") != "push" or run.get("head_branch") != tag
            or run.get("head_sha") != revision or not isinstance(path, str)
            or not re.fullmatch(re.escape(WORKFLOW) + r"(?:@refs/[A-Za-z0-9._/-]+)?", path)):
        raise RecoveryError("Original run does not match the release tag and workflow")
    jobs = get(prefix + "/actions/runs/" + source_run_id + "/jobs?filter=latest&per_page=100")
    if (not isinstance(jobs, dict) or type(jobs.get("total_count")) is not int
            or not 0 < jobs["total_count"] <= 100 or not isinstance(jobs.get("jobs"), list)
            or len(jobs["jobs"]) != jobs["total_count"]):
        raise RecoveryError("Original job list is incomplete or exceeds recovery limit")
    for required in ("version-check", "goreleaser"):
        matching = [job for job in jobs["jobs"] if isinstance(job, dict) and job.get("name") == required]
        if len(matching) != 1 or matching[0].get("status") != "completed" or matching[0].get("conclusion") != "success":
            raise RecoveryError("Original version-check and goreleaser jobs must have succeeded")
    release = get(prefix + "/releases/tags/" + tag)
    if (not isinstance(release, dict) or release.get("tag_name") != tag
            or release.get("draft") is not False
            or not isinstance(release.get("published_at"), str) or not release["published_at"]):
        raise RecoveryError("A matching published non-draft GitHub release is required")
    return {"tag": tag, "version": tag[1:], "source_revision": revision,
            "source_run_id": source_run_id}


def validate_intended_manifest(manifest_path, name, version, ecosystem):
    """Check retained artifact identity before the existing immutable-content checker."""
    if (name not in {"runkite-runner", "runkite-governance"} or ecosystem not in {"npm", "pypi"}
            or not isinstance(version, str)
            or not re.fullmatch(r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)", version)):
        raise RecoveryError("Unexpected recovery package")
    with Path(manifest_path).open("rb") as stream:
        manifest = strict_json(stream.read(MAX_JSON + 1))
    if (not isinstance(manifest, dict) or type(manifest.get("format")) is not int
            or manifest["format"] != 1 or manifest.get("name") != name
            or manifest.get("version") != version or manifest.get("ecosystem") != ecosystem):
        raise RecoveryError("Retained manifest identity differs from requested recovery")
    stem = name.replace("-", "_") if ecosystem == "pypi" else name
    expected = ({f"{stem}-{version}-py3-none-any.whl", f"{stem}-{version}.tar.gz"}
                if ecosystem == "pypi" else {f"{stem}-{version}.tgz"})
    artifacts = manifest.get("artifacts")
    if not isinstance(artifacts, list) or len(artifacts) != len(expected):
        raise RecoveryError("Retained manifest has an unexpected artifact set")
    names = set()
    for artifact in artifacts:
        if (not isinstance(artifact, dict) or not isinstance(artifact.get("filename"), str)
                or artifact["filename"] not in expected
                or artifact["filename"] in names or not isinstance(artifact.get("sha256"), str)
                or not re.fullmatch(r"[0-9a-f]{64}", artifact["sha256"])):
            raise RecoveryError("Retained manifest has invalid or duplicate artifacts")
        names.add(artifact["filename"])
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--source-run-id", required=True)
    args = parser.parse_args()
    try:
        outputs = prepare(args.tag, args.source_run_id, os.environ.get("GITHUB_REPOSITORY"))
        if output_file := os.environ.get("GITHUB_OUTPUT"):
            with open(output_file, "a", encoding="utf-8") as stream:
                stream.write("".join(f"{key}={value}\n" for key, value in outputs.items()))
        print(json.dumps(outputs, sort_keys=True))
    except (RecoveryError, OSError, subprocess.SubprocessError) as error:
        print(str(error) if isinstance(error, RecoveryError) else "Recovery metadata could not be validated", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
