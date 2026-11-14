"""Self-check for tls_utils.py: TLS/mTLS config for gRPC channel and
httpx client construction, shared by worker.py, generic_worker.py,
store.py, vectorstore.py, and a2a.py.

Proves:
1. Unset RUNKITE_TLS_CA_FILE means "no TLS" for both gRPC and httpx --
   grpc_channel_credentials() returns None, httpx_tls_kwargs() returns
   an empty dict, preserving today's default plaintext behavior exactly.
2. Setting RUNKITE_TLS_CA_FILE enables both, reading the real file.
3. Client cert + key together are picked up for mTLS on both surfaces.
4. A client cert without a key (or vice versa) is not treated as usable
   mTLS material for httpx's own (cert_file, key_file) tuple form.
5. RUNKITE_GRPC_TLS=1 alone (no CA file) enables gRPC TLS using the
   system trust store -- the "publicly-trusted cert, no custom CA
   needed" path gRPC has no URL-scheme signal for the way HTTP's
   https:// does -- without leaking into httpx_tls_kwargs() (HTTP needs
   no equivalent flag at all).

Usage:
    python/.venv/bin/python python/tests/test_tls_utils.py
"""

from __future__ import annotations

import asyncio
import os
import sys
import tempfile
from unittest.mock import MagicMock, patch

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner import tls_utils  # noqa: E402

# Not a real certificate -- grpc.ssl_channel_credentials and httpx's
# `verify=<path>` both just need a readable file to exercise the
# plumbing; parsing/validating the PEM content happens deeper in
# grpc/httpx's own code, not in tls_utils itself.
_FAKE_PEM = b"-----BEGIN CERTIFICATE-----\nZmFrZQ==\n-----END CERTIFICATE-----\n"


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


class _EnvSandbox:
    """Clears and restores the RUNKITE_TLS_*/RUNKITE_GRPC_TLS env vars around a test."""

    KEYS = (
        "RUNKITE_TLS_CA_FILE",
        "RUNKITE_TLS_CLIENT_CERT_FILE",
        "RUNKITE_TLS_CLIENT_KEY_FILE",
        "RUNKITE_GRPC_TLS",
    )

    def __enter__(self):
        self._saved = {k: os.environ.pop(k, None) for k in self.KEYS}
        return self

    def __exit__(self, *exc):
        for k, v in self._saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v


def test_unset_means_no_tls():
    with _EnvSandbox():
        check("grpc_channel_credentials() is None when unset", tls_utils.grpc_channel_credentials() is None)
        check("httpx_tls_kwargs() is empty when unset", tls_utils.httpx_tls_kwargs() == {})


def test_ca_file_alone_enables_tls_without_client_cert():
    with _EnvSandbox(), tempfile.NamedTemporaryFile(suffix=".pem") as ca:
        ca.write(_FAKE_PEM)
        ca.flush()
        os.environ["RUNKITE_TLS_CA_FILE"] = ca.name

        creds = tls_utils.grpc_channel_credentials()
        check("grpc_channel_credentials() returns something when CA is set", creds is not None)

        context = MagicMock()
        with patch("runkite_runner.tls_utils.ssl.create_default_context", return_value=context) as create:
            kwargs = tls_utils.httpx_tls_kwargs()
        create.assert_called_once_with(cafile=ca.name)
        check("httpx_tls_kwargs() supplies the prepared verification context", kwargs == {"verify": context})
        context.load_cert_chain.assert_not_called()


def test_client_cert_and_key_together_enable_mtls():
    with (
        _EnvSandbox(),
        tempfile.NamedTemporaryFile(suffix=".pem") as ca,
        tempfile.NamedTemporaryFile(suffix=".pem") as cert,
        tempfile.NamedTemporaryFile(suffix=".pem") as key,
    ):
        for f in (ca, cert, key):
            f.write(_FAKE_PEM)
            f.flush()
        os.environ["RUNKITE_TLS_CA_FILE"] = ca.name
        os.environ["RUNKITE_TLS_CLIENT_CERT_FILE"] = cert.name
        os.environ["RUNKITE_TLS_CLIENT_KEY_FILE"] = key.name

        creds = tls_utils.grpc_channel_credentials()
        check("grpc_channel_credentials() returns something with mTLS configured", creds is not None)

        context = MagicMock()
        with patch("runkite_runner.tls_utils.ssl.create_default_context", return_value=context) as create:
            kwargs = tls_utils.httpx_tls_kwargs()
        create.assert_called_once_with(cafile=ca.name)
        context.load_cert_chain.assert_called_once_with(cert.name, keyfile=key.name)
        check(
            "httpx gets a context with the client chain already loaded",
            kwargs.get("verify") is context and "cert" not in kwargs,
        )


def test_client_cert_without_key_is_not_used_for_httpx_tuple_form():
    # httpx's own cert= accepts a bare string (cert-only, no separate
    # key file, e.g. a combined PEM) OR a (cert, key) tuple -- but never
    # a cert path paired with a missing key when the caller only meant
    # to provide one half of a split cert+key pair. tls_utils falls back
    # to the single-string form here rather than silently dropping the
    # cert or guessing.
    with (
        _EnvSandbox(),
        tempfile.NamedTemporaryFile(suffix=".pem") as ca,
        tempfile.NamedTemporaryFile(suffix=".pem") as cert,
    ):
        ca.write(_FAKE_PEM)
        ca.flush()
        cert.write(_FAKE_PEM)
        cert.flush()
        os.environ["RUNKITE_TLS_CA_FILE"] = ca.name
        os.environ["RUNKITE_TLS_CLIENT_CERT_FILE"] = cert.name

        context = MagicMock()
        with patch("runkite_runner.tls_utils.ssl.create_default_context", return_value=context):
            kwargs = tls_utils.httpx_tls_kwargs()
        context.load_cert_chain.assert_called_once_with(cert.name, keyfile=None)
        check("combined certificate PEM is loaded in the context", kwargs.get("verify") is context)


def test_mtls_with_system_server_trust_and_incomplete_pair():
    with (
        _EnvSandbox(),
        tempfile.NamedTemporaryFile(suffix=".pem") as cert,
        tempfile.NamedTemporaryFile(suffix=".pem") as key,
    ):
        for f in (cert, key):
            f.write(_FAKE_PEM)
            f.flush()
        os.environ["RUNKITE_TLS_CLIENT_CERT_FILE"] = cert.name
        os.environ["RUNKITE_TLS_CLIENT_KEY_FILE"] = key.name
        check(
            "client certificate enables secure gRPC with system roots", tls_utils.grpc_channel_credentials() is not None
        )
        context = MagicMock()
        with patch("runkite_runner.tls_utils.ssl.create_default_context", return_value=context) as create:
            kwargs = tls_utils.httpx_tls_kwargs()
        create.assert_called_once_with(cafile=None)
        context.load_cert_chain.assert_called_once_with(cert.name, keyfile=key.name)
        check("client certificate works without a custom CA on HTTPS", kwargs.get("verify") is context)
        del os.environ["RUNKITE_TLS_CLIENT_CERT_FILE"]
        for helper in (tls_utils.grpc_channel_credentials, tls_utils.httpx_tls_kwargs):
            try:
                helper()
            except ValueError:
                pass
            else:
                raise AssertionError("orphan client key silently accepted")


def test_http_context_load_failure_is_not_downgraded():
    with _EnvSandbox():
        os.environ["RUNKITE_TLS_CLIENT_CERT_FILE"] = "synthetic-client.pem"
        os.environ["RUNKITE_TLS_CLIENT_KEY_FILE"] = "synthetic-client-key.pem"
        context = MagicMock()
        context.load_cert_chain.side_effect = ValueError("synthetic bad key pair")
        with patch("runkite_runner.tls_utils.ssl.create_default_context", return_value=context):
            try:
                tls_utils.httpx_tls_kwargs()
            except ValueError:
                pass
            else:
                raise AssertionError("mTLS failure silently downgraded to no client certificate")


def test_client_certificate_never_sends_headers_or_body_to_plaintext_transport():
    import httpx

    async def exercise(kwargs):
        seen = []

        def handler(request):
            seen.append(request)
            return httpx.Response(200, json={"ok": True})

        async with httpx.AsyncClient(transport=httpx.MockTransport(handler), **kwargs) as client:
            for path in (
                "/internal/runs/run/status",
                "/internal/checkpoints/thread/state",
                "/internal/runs/run/usage",
                "/internal/connectors/tool/session",
                "/internal/connectors/tool/mcp",
                "/internal/store/items",
            ):
                try:
                    await client.post(
                        "http://127.0.0.1:2026" + path,
                        headers={"X-Runner-Token": "synthetic"},
                        content=b"synthetic private body",
                    )
                except ValueError as error:
                    check("plaintext mTLS rejection has safe fixed text", "requires HTTPS" in str(error))
                else:
                    raise AssertionError("configured mTLS client sent plaintext")
            check("no headers or body reached the plaintext transport", seen == [])
            response = await client.get("https://controlplane.test/internal/runs/run/status")
            check("HTTPS still reaches the configured transport", response.status_code == 200 and len(seen) == 1)

    with _EnvSandbox(), patch("runkite_runner.tls_utils.ssl.create_default_context", return_value=MagicMock()):
        os.environ["RUNKITE_TLS_CLIENT_CERT_FILE"] = "synthetic.pem"
        os.environ["RUNKITE_TLS_CLIENT_KEY_FILE"] = "synthetic-key.pem"
        asyncio.run(exercise(tls_utils.httpx_tls_kwargs()))


def test_grpc_tls_flag_alone_enables_system_trust_grpc_without_affecting_http():
    with _EnvSandbox():
        os.environ["RUNKITE_GRPC_TLS"] = "1"
        creds = tls_utils.grpc_channel_credentials()
        check("RUNKITE_GRPC_TLS=1 alone enables gRPC TLS (system trust store)", creds is not None)
        check(
            "RUNKITE_GRPC_TLS has no effect on HTTP (no CA/https:// signal there)", tls_utils.httpx_tls_kwargs() == {}
        )


def test_grpc_tls_flag_accepts_common_truthy_spellings_and_rejects_others():
    for value, want_enabled in [
        ("1", True),
        ("true", True),
        ("True", True),
        ("yes", True),
        ("0", False),
        ("false", False),
        ("", False),
    ]:
        with _EnvSandbox():
            os.environ["RUNKITE_GRPC_TLS"] = value
            got_enabled = tls_utils.grpc_channel_credentials() is not None
            check(f"RUNKITE_GRPC_TLS={value!r} -> enabled={want_enabled}", got_enabled == want_enabled)


def main():
    test_unset_means_no_tls()
    test_ca_file_alone_enables_tls_without_client_cert()
    test_client_cert_and_key_together_enable_mtls()
    test_client_cert_without_key_is_not_used_for_httpx_tuple_form()
    test_mtls_with_system_server_trust_and_incomplete_pair()
    test_http_context_load_failure_is_not_downgraded()
    test_client_certificate_never_sends_headers_or_body_to_plaintext_transport()
    test_grpc_tls_flag_alone_enables_system_trust_grpc_without_affecting_http()
    test_grpc_tls_flag_accepts_common_truthy_spellings_and_rejects_others()
    print("\nAll checks passed.")


if __name__ == "__main__":
    main()
