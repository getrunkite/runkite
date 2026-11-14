"""TLS configuration for runner gRPC and HTTP connections.

RUNKITE_TLS_CA_FILE selects a custom server trust store. Without it, secure
connections use system roots. RUNKITE_GRPC_TLS enables gRPC TLS explicitly;
a configured client certificate also enables it. HTTP uses its URL scheme.
RUNKITE_TLS_CLIENT_CERT_FILE and RUNKITE_TLS_CLIENT_KEY_FILE configure the
same externally issued mTLS credential on both transports. gRPC requires both
files. HTTP also supports a combined certificate and key PEM in the cert file.
No identity headers are derived from these files. The server verifies TLS,
extracts the URI SAN and public-key fingerprint, and binds that holder to work.
Certificate changes require rebuilding clients. Strict owned runs must drain
or reclaim before their original certificate binding expires.
"""

import os
import ssl


def _read(path: str | None) -> bytes | None:
    if not path:
        return None
    with open(path, "rb") as f:
        return f.read()


def _truthy(value: str | None) -> bool:
    return (value or "").strip().lower() in ("1", "true", "yes")


async def _require_https_request(request) -> None:
    if request.url.scheme != "https":
        raise ValueError("configured client certificate requires HTTPS for control-plane requests")


def grpc_channel_credentials():
    """Return secure gRPC credentials when TLS or a client pair is configured.

    No custom CA means the gRPC runtime selects its normal trusted roots.
    An incomplete client pair fails instead of silently selecting plaintext.
    """
    import grpc  # lazy: httpx_tls_kwargs callers need not install grpc

    ca_file = os.environ.get("RUNKITE_TLS_CA_FILE")
    client_cert_file = os.environ.get("RUNKITE_TLS_CLIENT_CERT_FILE")
    client_key_file = os.environ.get("RUNKITE_TLS_CLIENT_KEY_FILE")
    if bool(client_cert_file) != bool(client_key_file):
        raise ValueError("gRPC mTLS requires both client certificate and key")
    if not ca_file and not client_cert_file and not _truthy(os.environ.get("RUNKITE_GRPC_TLS")):
        return None
    root_certs = _read(ca_file)  # None -> grpc's own system-trust default
    client_key = _read(client_key_file)
    client_cert = _read(client_cert_file)
    return grpc.ssl_channel_credentials(
        root_certificates=root_certs,
        private_key=client_key,
        certificate_chain=client_cert,
    )


def httpx_tls_kwargs() -> dict:
    """Prepare a verified SSLContext before passing it to httpx.

    Loading the client chain in the context avoids httpx versions that ignore
    cert= when verify= is a path. Custom CA roots replace system roots; client
    credentials without a custom CA retain normal server verification.
    """
    ca_file = os.environ.get("RUNKITE_TLS_CA_FILE")
    client_cert = os.environ.get("RUNKITE_TLS_CLIENT_CERT_FILE")
    client_key = os.environ.get("RUNKITE_TLS_CLIENT_KEY_FILE")
    if client_key and not client_cert:
        raise ValueError("HTTP mTLS requires a client certificate for its key")
    if not ca_file and not client_cert:
        return {}
    # httpx 0.28 returns early for verify=<path>, before processing cert=.
    # A prepared context both retains verification and reliably loads mTLS.
    context = ssl.create_default_context(cafile=ca_file)
    if client_cert:
        context.load_cert_chain(client_cert, keyfile=client_key)
    kwargs: dict = {"verify": context}
    if client_cert:
        kwargs["event_hooks"] = {"request": [_require_https_request]}
    return kwargs
