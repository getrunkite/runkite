#!/usr/bin/env python3
"""Build once and check public immutable packages before release or recovery.

This command never publishes. Registry lookups are unauthenticated HTTPS reads
of the public npm/PyPI endpoints. Only an explicit metadata 404 means absent.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import io
import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import zipfile
from pathlib import Path, PurePosixPath

import tomllib

MAX_METADATA = 8 * 1024 * 1024
MAX_ARCHIVE = 64 * 1024 * 1024
MAX_UNPACKED = 256 * 1024 * 1024
MAX_MEMBERS = 10000


class ArtifactError(Exception):
    """A safe, bounded release diagnostic without HTTP bodies or credentials."""


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def _object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ArtifactError("Duplicate registry JSON field")
        result[key] = value
    return result


def _json(data):
    try:
        return json.loads(data, object_pairs_hook=_object)
    except (ValueError, UnicodeError) as error:
        raise ArtifactError("Invalid registry or artifact JSON") from error


def archive_members(data, filename):
    """Hash every regular member, retaining executable bits, ignoring container dates."""
    members = {}
    total = 0

    def add(name, content, mode):
        nonlocal total
        parts = PurePosixPath(name).parts
        if not parts or name.startswith("/") or ".." in parts or "\\" in name:
            raise ArtifactError("Unsafe archive member")
        if name in members or len(members) >= MAX_MEMBERS:
            raise ArtifactError("Duplicate or excessive archive members")
        total += len(content)
        if total > MAX_UNPACKED:
            raise ArtifactError("Archive exceeds unpacked size limit")
        members[name] = {"sha256": sha256(content), "size": len(content), "mode": mode & 0o7777}

    if len(data) > MAX_ARCHIVE:
        raise ArtifactError("Archive exceeds download limit")
    try:
        if filename.endswith(".whl"):
            with zipfile.ZipFile(io.BytesIO(data)) as archive:
                for entry in archive.infolist():
                    if entry.is_dir():
                        continue
                    mode = entry.external_attr >> 16
                    if (mode & 0o170000) not in (0, 0o100000) or entry.file_size > MAX_UNPACKED - total:
                        raise ArtifactError("Unsupported wheel member")
                    add(entry.filename, archive.read(entry), mode)
        else:
            with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as archive:
                for entry in archive:
                    if entry.isdir():
                        continue
                    if not entry.isfile() or entry.size > MAX_UNPACKED - total:
                        raise ArtifactError("Unsupported tar member")
                    add(entry.name, archive.extractfile(entry).read(), entry.mode)
    except (tarfile.TarError, zipfile.BadZipFile, EOFError, OSError) as error:
        raise ArtifactError("Invalid package archive") from error
    return members


def _record(path):
    data = path.read_bytes()
    members = archive_members(data, path.name)
    return {
        "filename": path.name,
        "sha256": sha256(data),
        "integrity": "sha512-" + base64.b64encode(hashlib.sha512(data).digest()).decode(),
        "members": members,
    }


def _outputs(**values):
    if output := os.environ.get("GITHUB_OUTPUT"):
        with Path(output).open("a", encoding="utf-8") as stream:
            for key, value in values.items():
                if "\n" in str(value) or "\r" in str(value):
                    raise ArtifactError("Invalid workflow output")
                stream.write(f"{key}={value}\n")


def _run(command, cwd=None, env=None):
    subprocess.run(command, cwd=cwd, env=env, check=True, timeout=180)


def build(ecosystem, directory, output):
    directory, output = directory.resolve(), output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    if list(output.iterdir()):
        raise ArtifactError("Build output directory must be empty")
    if ecosystem == "npm":
        project = _json((directory / "package.json").read_bytes())
        name, version = project["name"], project["version"]
        _run(["npm", "run", "build"], directory)
        packed = subprocess.check_output(
            ["npm", "pack", "--ignore-scripts", "--json", "--pack-destination", str(output)],
            cwd=directory,
            timeout=60,
        )
        records = _json(packed)
        if len(records) != 1 or records[0]["name"] != name or records[0]["version"] != version:
            raise ArtifactError("npm pack metadata differs from project")
        filename = records[0]["filename"]
        if Path(filename).name != filename:
            raise ArtifactError("Invalid npm artifact filename")
        paths = [output / filename]
    else:
        project = tomllib.loads((directory / "pyproject.toml").read_text())["project"]
        name, version = project["name"], project["version"]
        with tempfile.TemporaryDirectory(prefix="runkite-package-source-") as temporary:
            source = Path(temporary) / "source"
            shutil.copytree(
                directory,
                source,
                ignore=shutil.ignore_patterns(
                    ".venv", "node_modules", "__pycache__", "*.egg-info", "build", "dist", ".ruff_cache"
                ),
            )
            # The default build builds an sdist and then its wheel, with no
            # dependency resolution. Build tools are supplied by the caller.
            _run([sys.executable, "-m", "build", "--no-isolation", "--outdir", str(output), str(source)])
        paths = sorted([*output.glob("*.whl"), *output.glob("*.tar.gz")])
        if len(paths) != 2 or not any(path.suffix == ".whl" for path in paths):
            raise ArtifactError("Expected one wheel and one source distribution")
    if project.get("license") != "Apache-2.0":
        raise ArtifactError("Expected Apache-2.0 project metadata")
    manifest = {
        "format": 1,
        "ecosystem": ecosystem,
        "name": name,
        "version": version,
        "artifacts": [_record(path) for path in paths],
    }
    path = output / "manifest.json"
    path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
    _outputs(manifest_path=path, artifact_dir=output, artifact_path=paths[0])
    return manifest


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def public_get(url, limit, *, missing_ok=False):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), _NoRedirect())
    request = urllib.request.Request(
        url, headers={"Accept": "application/json", "User-Agent": "runkite-release-check/1"}
    )
    try:
        with opener.open(request, timeout=30) as response:
            if response.status != 200:
                raise ArtifactError(f"Public registry returned HTTP {response.status}")
            data = response.read(limit + 1)
            if len(data) > limit:
                raise ArtifactError("Public registry response exceeded size limit")
            return data
    except urllib.error.HTTPError as error:
        error.close()
        if missing_ok and error.code == 404:
            return None
        raise ArtifactError(f"Public registry lookup failed: HTTP {error.code}") from None
    except (urllib.error.URLError, TimeoutError, OSError):
        raise ArtifactError("Public registry connection failed") from None


def _identity(manifest):
    if manifest.get("format") != 1 or manifest.get("ecosystem") not in {"npm", "pypi"}:
        raise ArtifactError("Unknown artifact manifest format")
    for field in ("name", "version"):
        if not isinstance(manifest.get(field), str) or not re.fullmatch(
            r"[A-Za-z0-9][A-Za-z0-9._+-]*", manifest[field]
        ):
            raise ArtifactError("Invalid manifest identity")


def _file_url(url, hostname):
    parsed = urllib.parse.urlsplit(url)
    if (
        parsed.scheme != "https"
        or parsed.hostname != hostname
        or parsed.port not in (None, 443)
        or parsed.username
        or parsed.password
        or parsed.query
        or parsed.fragment
    ):
        raise ArtifactError("Unexpected registry artifact URL")
    return url


def inspect_registry(manifest, get=public_get):
    """Return missing filenames, or fail on any mismatch or uncertain lookup."""
    _identity(manifest)
    name, version = manifest["name"], manifest["version"]
    expected = {entry["filename"]: entry for entry in manifest["artifacts"]}
    if not expected or len(expected) != len(manifest["artifacts"]):
        raise ArtifactError("Empty or duplicate local artifact manifest")
    if manifest["ecosystem"] == "npm":
        if len(expected) != 1:
            raise ArtifactError("Expected exactly one npm tarball")
        raw = get(f"https://registry.npmjs.org/{name}/{version}", MAX_METADATA, missing_ok=True)
        if raw is None:
            return sorted(expected)
        remote = _json(raw)
        if remote.get("name") != name or remote.get("version") != version:
            raise ArtifactError("npm registry identity differs from intended package")
        [entry] = expected.values()
        dist = remote.get("dist", {})
        if dist.get("integrity") != entry["integrity"]:
            raise ArtifactError("Existing npm version has different immutable integrity")
        url = _file_url(dist.get("tarball", ""), "registry.npmjs.org")
        data = get(url, MAX_ARCHIVE)
        if sha256(data) != entry["sha256"]:
            raise ArtifactError("Existing npm tarball does not match intended artifact")
        return []
    raw = get(f"https://pypi.org/pypi/{name}/{version}/json", MAX_METADATA, missing_ok=True)
    if raw is None:
        return sorted(expected)
    remote = _json(raw)

    def normalize(value):
        return re.sub(r"[-_.]+", "-", value).lower()

    if (
        normalize(remote.get("info", {}).get("name", "")) != normalize(name)
        or remote.get("info", {}).get("version") != version
    ):
        raise ArtifactError("PyPI registry identity differs from intended package")
    seen = set()
    for entry in remote.get("urls", []):
        filename = entry.get("filename")
        if filename not in expected or filename in seen:
            raise ArtifactError("Unexpected or duplicate existing PyPI distribution")
        seen.add(filename)
        url = _file_url(entry.get("url", ""), "files.pythonhosted.org")
        data = get(url, MAX_ARCHIVE)
        if sha256(data) != entry.get("digests", {}).get("sha256"):
            raise ArtifactError("PyPI download differs from registry digest")
        # Wheel ZIP timestamps and sdist gzip/tar dates can change on rebuild.
        # Compare every file's bytes and executable bit, never just metadata.
        if archive_members(data, filename) != expected[filename]["members"]:
            raise ArtifactError("Existing PyPI distribution contents differ from intended artifact")
    return sorted(set(expected) - seen)


def check(manifest_path, *, pending_dir=None, require_present=False, wait_seconds=0):
    manifest = _json(manifest_path.read_bytes())
    for entry in manifest["artifacts"]:
        filename = entry["filename"]
        if Path(filename).name != filename or _record(manifest_path.parent / filename) != entry:
            raise ArtifactError("Local artifact changed after verification")
    deadline = time.monotonic() + wait_seconds
    while True:
        missing = inspect_registry(manifest)
        if not missing or time.monotonic() >= deadline:
            break
        time.sleep(min(1, max(0, deadline - time.monotonic())))
    result = {
        "name": manifest["name"],
        "version": manifest["version"],
        "state": "needs_publish" if missing else "already_matching",
        "pending": missing,
    }
    if pending_dir is not None:
        pending_dir.mkdir(parents=True, exist_ok=True)
        if list(pending_dir.iterdir()):
            raise ArtifactError("Pending artifact directory must be empty")
        for filename in missing:
            shutil.copyfile(manifest_path.parent / filename, pending_dir / filename)
    _outputs(pending=str(bool(missing)).lower(), state=result["state"])
    print(json.dumps(result, sort_keys=True))
    if require_present and missing:
        raise ArtifactError("Expected registry artifacts remain unpublished")
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    build_command = commands.add_parser("build")
    build_command.add_argument("ecosystem", choices=("npm", "pypi"))
    build_command.add_argument("directory", type=Path)
    build_command.add_argument("output", type=Path)
    check_command = commands.add_parser("check")
    check_command.add_argument("manifest", type=Path)
    check_command.add_argument("--pending-dir", type=Path)
    check_command.add_argument("--require-present", action="store_true")
    check_command.add_argument("--wait-seconds", type=int, default=0, choices=range(0, 31), metavar="0..30")
    args = parser.parse_args()
    try:
        if args.command == "build":
            build(args.ecosystem, args.directory, args.output)
        else:
            check(
                args.manifest,
                pending_dir=args.pending_dir,
                require_present=args.require_present,
                wait_seconds=args.wait_seconds,
            )
    except (
        ArtifactError,
        subprocess.CalledProcessError,
        subprocess.TimeoutExpired,
        OSError,
        KeyError,
        TypeError,
        ValueError,
    ) as error:
        print(f"Release artifact check failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
