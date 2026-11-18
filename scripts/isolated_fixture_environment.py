#!/usr/bin/env python3
"""Sterile process environments for disposable isolated-runtime qualification.

Only PATH is inherited automatically. Fixture selectors and prepared public
package caches require explicit selection. This helper never prints values or
loads a user profile, credential file, Docker config or CLI registration.
"""

from __future__ import annotations

import argparse
import os
import re
import stat
from collections.abc import Mapping, Sequence
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

_PATH_INPUTS = {
    "RUNKITE_TEST_BINARY",
    "RUNKITE_TEST_RUNNER_WHEEL",
    "RUNKITE_TEST_ISOLATED_CLI",
    "RUNKITE_TEST_ISOLATED_PYTHON",
    "RUNKITE_TEST_DOCKER_SOCKET",
    "RUNKITE_TEST_ISOLATED_OUTPUT",
    "RUNKITE_TEST_ISOLATED_HA_OUTPUT",
    "RUNKITE_TEST_ISOLATED_GATEWAY_CONFIG",
    "RUNKITE_TEST_DOCKER_COMPOSE_PLUGIN",
    "RUNKITE_TEST_DOCKER_BUILDX_PLUGIN",
}
_BOOL_INPUTS = {"RUNKITE_TEST_ISOLATED", "RUNKITE_TEST_ISOLATED_HA", "RUNKITE_TEST_ISOLATED_GATEWAY_TLS"}
FIXTURE_INPUT_NAMES = frozenset(
    _PATH_INPUTS
    | _BOOL_INPUTS
    | {
        "RUNKITE_TEST_ISOLATED_GATEWAY",
        "RUNKITE_TEST_ISOLATED_IMAGE",
        "RUNKITE_TEST_ISOLATED_REPETITIONS",
        "RUNKITE_TEST_ISOLATED_POSTGRES_DSN",
        "RUNKITE_TEST_ISOLATED_REDIS_URL",
        "RUNKITE_ISOLATED_HA_PG_PASSWORD",
    }
)
CACHE_NAMES = frozenset({"GOCACHE", "GOMODCACHE", "UV_CACHE_DIR", "PIP_CACHE_DIR"})


def _absolute(value: str) -> bool:
    return bool(value) and "\x00" not in value and "\r" not in value and "\n" not in value and Path(value).is_absolute()


def _loopback_origin(value: str) -> bool:
    try:
        url = urlsplit(value)
        return (
            url.scheme in {"http", "https"}
            and url.hostname in {"127.0.0.1", "::1"}
            and url.port is not None
            and url.port > 0
            and url.username is None
            and url.password is None
            and url.path in {"", "/"}
            and not url.query
            and not url.fragment
        )
    except ValueError:
        return False


def _database(value: str, *, postgres: bool) -> bool:
    try:
        url = urlsplit(value)
        if url.hostname not in {"127.0.0.1", "::1"} or url.port is None or url.port < 1 or url.fragment:
            return False
        if postgres:
            return (
                url.scheme in {"postgres", "postgresql"}
                and url.username == "runkite"
                and bool(url.password)
                and re.fullmatch(r"/runkite_isolated_ha_[a-z0-9_]+", url.path) is not None
                and parse_qs(url.query, strict_parsing=True) == {"sslmode": ["disable"]}
            )
        return (
            url.scheme == "redis"
            and url.path == "/0"
            and not url.query
            and url.username is None
            and url.password is None
        )
    except ValueError:
        return False


def fixture_inputs(source: Mapping[str, str], names: Sequence[str] | None = None) -> dict[str, str]:
    """Select known fixture settings; callers still verify artifact/file ownership.

    Paths must be explicit and absolute. No directories or credential contents
    are inspected. Runtime endpoint and database selectors accept owned local
    fixture shapes only. Unknown names fail closed, including unknown test keys.
    """
    selected = sorted(FIXTURE_INPUT_NAMES) if names is None else names
    result = {}
    for name in selected:
        if name not in FIXTURE_INPUT_NAMES:
            raise ValueError("unsupported isolated fixture input")
        value = source.get(name)
        if value is None or value == "":
            continue
        valid = isinstance(value, str) and len(value) <= 8192 and not any(c in value for c in "\x00\r\n")
        if valid:
            if name in _PATH_INPUTS:
                valid = _absolute(value)
            elif name in _BOOL_INPUTS:
                valid = value in {"0", "1"}
            elif name == "RUNKITE_TEST_ISOLATED_REPETITIONS":
                valid = re.fullmatch(r"(?:[1-9]|1[0-9]|20)", value) is not None
            elif name == "RUNKITE_TEST_ISOLATED_GATEWAY":
                valid = _loopback_origin(value)
            elif name == "RUNKITE_TEST_ISOLATED_IMAGE":
                valid = re.fullmatch(r"(?:[^\s]+@sha256:|sha256:)[a-f0-9]{64}", value) is not None
            elif name == "RUNKITE_TEST_ISOLATED_POSTGRES_DSN":
                valid = _database(value, postgres=True)
            elif name == "RUNKITE_TEST_ISOLATED_REDIS_URL":
                valid = _database(value, postgres=False)
            elif name == "RUNKITE_ISOLATED_HA_PG_PASSWORD":
                valid = re.fullmatch(r"[A-Za-z0-9_-]{32,128}", value) is not None
        if not valid:
            raise ValueError("invalid isolated fixture input: " + name)
        result[name] = value
    return result


def _directory(path: Path, *, private: bool) -> str:
    if not _absolute(str(path)) or path.is_symlink():
        raise ValueError("isolated fixture directory must be an absolute non-symlink path")
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    if not path.is_dir():
        raise ValueError("isolated fixture directory is unavailable")
    if private:
        path.chmod(0o700)
    return str(path)


def sterile_environment(
    directory: Path,
    *,
    source: Mapping[str, str] | None = None,
    cache_dirs: Mapping[str, Path] | None = None,
    docker_plugins: Mapping[str, Path] | None = None,
) -> dict[str, str]:
    """Create stable task-owned configuration paths, inheriting only PATH."""
    source = os.environ if source is None else source
    path = source.get("PATH", os.defpath)
    if not isinstance(path, str) or len(path) > 32768 or any(c in path for c in "\x00\r\n"):
        raise ValueError("invalid isolated fixture tool path")
    entries = path.split(os.pathsep)
    if not entries or any(not _absolute(entry) for entry in entries):
        raise ValueError("isolated fixture tool path requires absolute entries")
    directory = Path(_directory(Path(directory), private=True))
    home = _directory(directory / "home", private=True)
    env = {"PATH": path, "HOME": home, "LANG": "C", "PYTHONUTF8": "1", "PYTHONNOUSERSITE": "1", "PYTHONUNBUFFERED": "1"}
    for key, child in {
        "XDG_CONFIG_HOME": "config",
        "XDG_DATA_HOME": "data",
        "XDG_STATE_HOME": "state",
        "XDG_CACHE_HOME": "cache",
        "XDG_RUNTIME_DIR": "runtime",
        "TMPDIR": "tmp",
        "TMP": "tmp",
        "TEMP": "tmp",
        "DOCKER_CONFIG": "docker",
        "BUILDX_CONFIG": "buildx",
        "UV_CACHE_DIR": "uv-cache",
        "PIP_CACHE_DIR": "pip-cache",
        "NPM_CONFIG_CACHE": "npm-cache",
    }.items():
        env[key] = _directory(directory / child, private=True)
    env.update(
        {
            "NETRC": os.devnull,
            "PIP_CONFIG_FILE": os.devnull,
            "PIP_DISABLE_PIP_VERSION_CHECK": "1",
            "PIP_NO_INPUT": "1",
            "UV_NO_CONFIG": "true",
            "UV_PYTHON_DOWNLOADS": "never",
            "CURL_HOME": home,
            "OPENSSL_CONF": os.devnull,
            "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_CONFIG_NOSYSTEM": "1",
            "NPM_CONFIG_USERCONFIG": os.devnull,
            "NPM_CONFIG_GLOBALCONFIG": os.devnull,
        }
    )
    for key, cache in (cache_dirs or {}).items():
        if key not in CACHE_NAMES:
            raise ValueError("unsupported isolated fixture cache")
        env[key] = _directory(Path(cache), private=False)
    plugin_root = Path(env["DOCKER_CONFIG"]) / "cli-plugins"
    for name, supplied in (docker_plugins or {}).items():
        if name not in {"compose", "buildx"} or not _absolute(str(supplied)):
            raise ValueError("invalid isolated fixture Docker plugin")
        try:
            executable = Path(supplied).resolve(strict=True)
            if not stat.S_ISREG(executable.stat().st_mode) or not os.access(executable, os.X_OK):
                raise ValueError("invalid isolated fixture Docker plugin")
            _directory(plugin_root, private=True)
            link = plugin_root / ("docker-" + name)
            if link.is_symlink() and link.resolve(strict=True) == executable:
                continue
            if link.exists() or link.is_symlink():
                raise ValueError("owned Docker plugin selection changed")
            link.symlink_to(executable)
        except OSError:
            raise ValueError("invalid isolated fixture Docker plugin") from None
    return env


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", required=True, type=Path)
    parser.add_argument("--input", action="append", default=[])
    parser.add_argument("--cache", action="append", default=[])
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    if not command:
        parser.error("provide an owned fixture command after --")
    caches = {}
    try:
        for item in args.cache:
            key, separator, value = item.partition("=")
            if not separator or key not in CACHE_NAMES or key in caches:
                raise ValueError("invalid isolated fixture cache selection")
            caches[key] = Path(value)
        inputs = fixture_inputs(os.environ, args.input)
        plugins = {
            name: Path(inputs[key])
            for name, key in {
                "compose": "RUNKITE_TEST_DOCKER_COMPOSE_PLUGIN",
                "buildx": "RUNKITE_TEST_DOCKER_BUILDX_PLUGIN",
            }.items()
            if key in inputs
        }
        environment = sterile_environment(args.directory, cache_dirs=caches, docker_plugins=plugins)
        environment.update(inputs)
    except (ValueError, OSError):
        parser.error("invalid isolated fixture environment selection")
    os.execvpe(command[0], command, environment)


if __name__ == "__main__":
    main()
