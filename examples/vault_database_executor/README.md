# Credential-aware PostgreSQL MCP executor

v0.7.0 example. This is an external server for Runkite's managed
`vault_database` connector path. It is not a general database MCP server and is not
embedded in the control-plane binary.

The tool `report_count` runs one fixed read-only query against
`public.runkite_reports`. It accepts empty arguments, opens PostgreSQL with the
short-lived username/password privately supplied through HTTP Basic and returns
only the row count. It never returns a credential or accepts arbitrary SQL.

The configured Vault database role must grant only `CONNECT`, schema `USAGE` and
`SELECT` on that table. Its `max_ttl` is the real provider ceiling, including when an
issuance response is lost. Configure the role and workload token outside Runkite;
do not give the agent a Vault path, role-administration rights or bootstrap token.

For an existing local PostgreSQL target without embedded credentials:

```sh
go run ./examples/vault_database_executor \
  -listen 127.0.0.1:8788 \
  -postgres 'host=127.0.0.1 port=5432 dbname=reporting sslmode=disable'
```

The loopback-only HTTP exception is for a local test. Non-loopback MCP requires
`-tls-cert` and `-tls-key`; a non-loopback database target requires verified TLS
(for example `sslmode=verify-full` with the correct trust configuration). Keep this
executor on a trusted network reachable from the control plane, not the agent.
Configure its endpoint, registered credential binding and policy on Runkite. Model
calls and approval still use the existing managed runner and connector APIs.

Initialize, ping, notifications/initialized and tools/list do not acquire a
database credential. Only the authorized tools/call path receives Basic credentials.
Resources and prompts are unsupported. The server does not log request bodies,
Authorization headers, connection strings or provider error details.

The disposable local qualification requires Docker and an installed `vault` CLI:

```sh
RUNKITE_TEST_VAULT_DATABASE=1 go test -count=1 -v \
  ./examples/vault_database_executor -run '^TestLiveVault'
```

It creates and removes only its own PostgreSQL container and native Vault process.
All database rows and credentials are synthetic. It uses no cloud, model provider
or existing Vault installation. Generated fixture credentials and Vault startup
logs stay in private temporary files; the test output reports no secret values.

See [the engineering record](../../docs/internal/credential-lifecycle.md) for the
lease states, encrypted cleanup handles, unknown issuance/revocation outcomes,
restart recovery and operational limits. A confirmed revoke does not undo an
already executed query or prove that a different executor terminated its sessions.
