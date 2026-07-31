"""Shared TLS/mTLS configuration for connections FROM this runner TO the
control plane -- both the gRPC bridge (worker.py, generic_worker.py) and
the proxy-mode HTTP calls (store.py, vectorstore.py, a2a.py).

Three env vars, shared across both transports since a real deployment
signs both the control plane's HTTP and gRPC server certs with the same
CA, and a runner talks to exactly one control plane:

- RUNKITE_TLS_CA_FILE: verify the control plane's server certificate
  against this CA instead of (or in addition to) the system trust
  store. Required for a self-signed or internal-CA-signed control
  plane cert; a publicly-trusted cert needs nothing set at all --
  https:// URLs and grpc.aio.secure_channel with default credentials
  already verify against the system trust store on their own.
- RUNKITE_TLS_CLIENT_CERT_FILE / RUNKITE_TLS_CLIENT_KEY_FILE: this
  runner's own client certificate for mTLS, when the control plane
  requires one (TLS_CLIENT_CA_FILE / GRPC_TLS_CLIENT_CA_FILE on the Go
  side -- see cmd/tls.go).

All three are optional and off by default -- unset envs mean exactly
today's plaintext gRPC / whatever the http_base_url's own scheme
already implies for HTTP, matching every other env-var-driven piece of
this runner's config (RUNNER_TOKEN, POSTGRES_DSN, etc).
"""

import os

import grpc


def _read(path: str | None) -> bytes | None:
    if not path:
        return None
    with open(path, "rb") as f:
        return f.read()


def grpc_channel_credentials() -> grpc.ChannelCredentials | None:
    """Returns TLS channel credentials for grpc.aio.secure_channel, or
    None if RUNKITE_TLS_CA_FILE is unset -- callers should fall back to
    grpc.aio.insecure_channel in that case, preserving today's default
    plaintext behavior exactly.

    A client cert/key (mTLS) is only meaningful paired with a CA file;
    grpc.ssl_channel_credentials accepts them as optional
    private_key/certificate_chain arguments regardless.
    """
    ca_file = os.environ.get("RUNKITE_TLS_CA_FILE")
    if not ca_file:
        return None
    root_certs = _read(ca_file)
    client_key = _read(os.environ.get("RUNKITE_TLS_CLIENT_KEY_FILE"))
    client_cert = _read(os.environ.get("RUNKITE_TLS_CLIENT_CERT_FILE"))
    return grpc.ssl_channel_credentials(
        root_certificates=root_certs,
        private_key=client_key,
        certificate_chain=client_cert,
    )


def httpx_tls_kwargs() -> dict:
    """Returns kwargs to splat into httpx.AsyncClient(...) for TLS
    verification/mTLS against the control plane's HTTP API. Empty dict
    (httpx's own defaults: verify against the system trust store) when
    RUNKITE_TLS_CA_FILE is unset -- correct as-is for both a plain
    http:// base URL (verify is simply unused) and an https:// one with
    a publicly-trusted certificate.
    """
    ca_file = os.environ.get("RUNKITE_TLS_CA_FILE")
    if not ca_file:
        return {}
    kwargs: dict = {"verify": ca_file}
    client_cert = os.environ.get("RUNKITE_TLS_CLIENT_CERT_FILE")
    client_key = os.environ.get("RUNKITE_TLS_CLIENT_KEY_FILE")
    if client_cert and client_key:
        kwargs["cert"] = (client_cert, client_key)
    elif client_cert:
        kwargs["cert"] = client_cert
    return kwargs
