#!/usr/bin/env python3
"""Wait for this attempt's completed Pages upload using read-only metadata calls."""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

MAX_METADATA_BYTES = 1024 * 1024
WAIT_SECONDS = 60
REQUEST_SECONDS = 10
POLL_SECONDS = 2


class ArtifactError(Exception):
    """A safe diagnostic that contains no token or remote response body."""


class RetryableRead(ArtifactError):
    """A temporary metadata-read failure, before deployment is attempted."""


def github_json(url, token, timeout):
    request = urllib.request.Request(url, headers={
        "Authorization": f"Bearer {token}",
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
    })
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            data = response.read(MAX_METADATA_BYTES + 1)
    except urllib.error.HTTPError as error:
        if error.code in {404, 408, 429, 500, 502, 503, 504}:
            raise RetryableRead(f"GitHub metadata temporarily unavailable (HTTP {error.code})") from None
        raise ArtifactError(f"GitHub metadata request failed (HTTP {error.code})") from None
    except (urllib.error.URLError, OSError):
        raise RetryableRead("GitHub metadata request temporarily failed") from None
    if len(data) > MAX_METADATA_BYTES:
        raise ArtifactError("GitHub artifact metadata exceeds size limit")
    try:
        return json.loads(data)
    except (ValueError, UnicodeError):
        raise ArtifactError("Invalid GitHub artifact metadata JSON") from None


def metadata_ready(metadata, artifact_id, artifact_name):
    if not isinstance(metadata, dict):
        raise ArtifactError("Invalid GitHub artifact metadata")
    count, artifacts = metadata.get("total_count"), metadata.get("artifacts")
    if type(count) is not int or count < 0 or not isinstance(artifacts, list):
        raise ArtifactError("Invalid GitHub artifact metadata")
    if count > 1:
        raise ArtifactError("Multiple artifacts have this attempt's Pages name")
    if count != len(artifacts):
        raise ArtifactError("Incomplete GitHub artifact metadata")
    if count == 0:
        return False
    artifact = artifacts[0]
    if (
        not isinstance(artifact, dict)
        or type(artifact.get("id")) is not int
        or artifact["id"] != artifact_id
        or artifact.get("name") != artifact_name
    ):
        raise ArtifactError("Pages artifact identity differs from this attempt's upload")
    if artifact.get("expired") is not False:
        raise ArtifactError("Pages artifact is expired or missing its expiry state")
    size = artifact.get("size_in_bytes")
    if type(size) is not int or size < 0:
        raise ArtifactError("Invalid Pages artifact size")
    return size > 0


def wait_for_artifact(repository, run_id, artifact_id, artifact_name, token,
                      fetch=github_json, now=time.monotonic, sleep=time.sleep):
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
        raise ArtifactError("Invalid GitHub repository")
    if not re.fullmatch(r"[1-9][0-9]*", run_id):
        raise ArtifactError("Invalid workflow run ID")
    if not re.fullmatch(r"[1-9][0-9]*", artifact_id):
        raise ArtifactError("Missing or invalid upload artifact ID")
    if not re.fullmatch(r"github-pages-" + run_id + r"-[1-9][0-9]*", artifact_name):
        raise ArtifactError("Pages artifact name must identify this run and attempt")
    if not token:
        raise ArtifactError("GitHub metadata token is required")
    query = urllib.parse.urlencode({"name": artifact_name, "per_page": 100})
    url = f"https://api.github.com/repos/{repository}/actions/runs/{run_id}/artifacts?{query}"
    deadline = now() + WAIT_SECONDS
    while True:
        remaining = deadline - now()
        if remaining <= 0:
            raise ArtifactError("Timed out waiting for this attempt's Pages artifact metadata")
        try:
            ready = metadata_ready(fetch(url, token, min(REQUEST_SECONDS, remaining)),
                                   int(artifact_id), artifact_name)
        except RetryableRead:
            ready = False
        if now() >= deadline:
            raise ArtifactError("Timed out waiting for this attempt's Pages artifact metadata")
        if ready:
            return
        sleep(max(0, min(POLL_SECONDS, deadline - now())))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--artifact-id", required=True)
    parser.add_argument("--artifact-name", required=True)
    args = parser.parse_args()
    try:
        wait_for_artifact(args.repository, args.run_id, args.artifact_id,
                          args.artifact_name, os.environ.get("GH_TOKEN", ""))
    except ArtifactError as error:
        print(f"Pages artifact check failed: {error}", file=sys.stderr)
        return 1
    print("This attempt's uploaded Pages artifact is visible in GitHub REST metadata.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
