"""Shared TLS/mTLS configuration for connections FROM this runner TO the
control plane -- both the gRPC bridge (worker.py, generic_worker.py) and
the proxy-mode HTTP calls (store.py, vectorstore.py, a2a.py).

- RUNKITE_TLS_CA_FILE: verify the control plane's server certificate
  against this CA instead of the system trust store (it REPLACES the
  system store for that verification, not "in addition to" -- the same
  behavior grpc.ssl_channel_credentials and httpx's own `verify=<path>`
  both already have). Required for a self-signed or internal-CA-signed
  control plane cert.
- RUNKITE_GRPC_TLS: enables gRPC TLS using the system trust store when
  RUNKITE_TLS_CA_FILE is NOT set -- the gRPC equivalent of what an
  https:// URL already gives HTTP for free. gRPC has no URL scheme to
  carry that signal the way HTTP's http://` vs `https://` does, so
  without this there would be no way to ask for "TLS, but a publicly-
  trusted cert, no custom CA needed" on the gRPC side at all -- only
  "plaintext" or "TLS with a specific custom CA." Sets
  root_certificates=None on grpc.ssl_channel_credentials, which per its
  own docstring means "retrieve them from a default location chosen by
  the gRPC runtime" (the system trust store). Ignored (redundant, not a
  conflict) when RUNKITE_TLS_CA_FILE is also set. HTTP proxy-mode calls
  need no equivalent: httpx already defaults to system-trust
  verification for any https:// base URL with nothing extra set.
- RUNKITE_TLS_CLIENT_CERT_FILE / RUNKITE_TLS_CLIENT_KEY_FILE: this
  runner's own client certificate for mTLS, when the control plane
  requires one (TLS_CLIENT_CA_FILE / GRPC_TLS_CLIENT_CA_FILE on the Go
  side -- see cmd/tls.go). Independent of which trust store is in use
  above -- mTLS and server-cert verification are orthogonal.

All are optional and off by default -- unset envs mean exactly today's
plaintext gRPC / whatever the http_base_url's own scheme already
implies for HTTP, matching every other env-var-driven piece of this
runner's config (RUNNER_TOKEN, POSTGRES_DSN, etc).
"""

import os

import grpc


def _read(path: str | None) -> bytes | None:
    if not path:
        return None
    with open(path, "rb") as f:
        return f.read()


def _truthy(value: str | None) -> bool:
    return (value or "").strip().lower() in ("1", "true", "yes")


def grpc_channel_credentials() -> grpc.ChannelCredentials | None:
    """Returns TLS channel credentials for grpc.aio.secure_channel, or
    None if neither RUNKITE_TLS_CA_FILE nor RUNKITE_GRPC_TLS is set --
    callers should fall back to grpc.aio.insecure_channel in that case,
    preserving today's default plaintext behavior exactly.

    root_certificates is None (system trust store) when
    RUNKITE_TLS_CA_FILE is unset but RUNKITE_GRPC_TLS is -- see this
    module's own doc comment for why that combination needs to exist at
    all. A client cert/key (mTLS) is meaningful either way;
    grpc.ssl_channel_credentials accepts them as optional
    private_key/certificate_chain arguments regardless of which trust
    store is selected.
    """
    ca_file = os.environ.get("RUNKITE_TLS_CA_FILE")
    if not ca_file and not _truthy(os.environ.get("RUNKITE_GRPC_TLS")):
        return None
    root_certs = _read(ca_file)  # None -> grpc's own system-trust default
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
    (httpx's own default: verify against the system trust store) when
    RUNKITE_TLS_CA_FILE is unset -- correct as-is for both a plain
    http:// base URL (verify is simply unused) and an https:// one with
    a publicly-trusted certificate; no RUNKITE_GRPC_TLS-style extra flag
    needed here since https:// itself is already the "I want TLS"
    signal HTTP has and gRPC doesn't. When RUNKITE_TLS_CA_FILE IS set,
    it REPLACES the system trust store for this connection (httpx's own
    `verify=<path>` semantics), not an addition to it.
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
