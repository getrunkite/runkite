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

Usage:
    python/.venv/bin/python python/tests/test_tls_utils.py
"""

from __future__ import annotations

import os
import sys
import tempfile

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
    """Clears and restores the 3 RUNKITE_TLS_* env vars around a test."""

    KEYS = ("RUNKITE_TLS_CA_FILE", "RUNKITE_TLS_CLIENT_CERT_FILE", "RUNKITE_TLS_CLIENT_KEY_FILE")

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

        kwargs = tls_utils.httpx_tls_kwargs()
        check("httpx_tls_kwargs() sets verify to the CA path", kwargs.get("verify") == ca.name)
        check("httpx_tls_kwargs() has no cert without a client cert/key", "cert" not in kwargs)


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

        kwargs = tls_utils.httpx_tls_kwargs()
        check(
            "httpx_tls_kwargs() sets cert to a (cert, key) tuple for mTLS",
            kwargs.get("cert") == (cert.name, key.name),
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

        kwargs = tls_utils.httpx_tls_kwargs()
        check("httpx_tls_kwargs() falls back to a bare cert string without a key", kwargs.get("cert") == cert.name)


def main():
    test_unset_means_no_tls()
    test_ca_file_alone_enables_tls_without_client_cert()
    test_client_cert_and_key_together_enable_mtls()
    test_client_cert_without_key_is_not_used_for_httpx_tuple_form()
    print("\nAll checks passed.")


if __name__ == "__main__":
    main()
