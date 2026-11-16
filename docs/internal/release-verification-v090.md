# v0.9.0 publication verification

Status: actual publication verified on November 16, 2026. Four Go archives,
three OpenAPI downloads, four registry packages and all six image platform
variants passed their checks. The
[package-only verification run](https://github.com/getrunkite/runkite/actions/runs/43858695369)
also completed successfully.

The immutable `v0.9.0` tag identifies commit
`9c7afb97aa69977385275bce8f5970e34f68f419`. The annotated tag was created by
Sharan Harsoor using the personal maintainer account. Earlier release tags and
their existing assets were not replaced.

## Source qualification

[PR #70](https://github.com/getrunkite/runkite/pull/70) passed the full CI, HA and
dedicated Trinity example checks. Its merged runtime also passed
[main CI](https://github.com/getrunkite/runkite/actions/runs/43847333439).
The final README-only preparation in
[PR #71](https://github.com/getrunkite/runkite/pull/71) passed
[full Linux CI](https://github.com/getrunkite/runkite/actions/runs/43849234310)
and [HA checks](https://github.com/getrunkite/runkite/actions/runs/43849234322).
The tagged merge has the same source tree as that tested PR head.
The tagged commit subsequently passed
[main CI](https://github.com/getrunkite/runkite/actions/runs/43852961574)
and [HA checks](https://github.com/getrunkite/runkite/actions/runs/43852961633).

The gates include race-enabled Go tests with live supported test backends,
real-plane managed adapter fixtures, installed governance package contracts,
Admin build/tests, OpenAPI checks, image scans and packaged mTLS checks.
The Trinity example checks pinned upstream configuration, its built MCP caller
and real Runkite interoperability. This does not qualify native Trinity runtime
execution or establish upstream endorsement.

## Published Go assets

The [original Release run](https://github.com/getrunkite/runkite/actions/runs/43853089903)
published four macOS/Linux archives, their checksums and three OpenAPI JSON files.
Downloaded archive checksums, binary architecture headers, packaged README,
Apache license and third-party notices match the tag. All three downloaded
OpenAPI documents match the tagged bytes. Every one of the 19 tracked embedded
Admin assets appears byte-for-byte in all four archive binaries.

The macOS arm64 binary executed natively. The macOS x86_64 binary executed
through already installed Rosetta. Both reported `0.9.0`, commit `9c7afb9`,
and build time `2026-11-16T10:55:36Z`. Literal asset membership does not establish
HTTP routing or browser behavior on every architecture.
Both downloaded Linux archive binaries also executed `version` successfully
inside matching already verified immutable plane images, with networking
disabled, read-only filesystems and confirmed cleanup. All four archive version
probes reported the same version, commit and build timestamp.

## Registry publication and authentication

Both npm tarballs were retained by the original Release run, validated against
their manifests and source receipts, and published without rebuilding through
the personal `sharanharsoor` npm session. Publication lifecycle scripts were
disabled. Their intended SHA256 values are:

- `runkite-runner-0.9.0.tgz`:
  `860c63582e606aece8528ad000c5e0a543d9f9eb01b48d9df7f815be49b123c0`.
- `runkite-governance-0.9.0.tgz`:
  `506acd932ab545c3d9a1fb58da1174a1a8081058622a3861e74d65369d1e6d82`.

The original automated npm jobs failed authentication. Their continued OIDC
steps must not be described as successful publication merely because the
Actions API reports a successful step conclusion after `continue-on-error`.
The actual logs report `ENEEDAUTH`; the runner's configured fallback also
failed. Personal publication and later matching-existing verification do not
prove npm Trusted Publishing. Package-specific automation authority remains a
preflight requirement for the next release.

The PyPI runner used its existing project token. The PyPI governance helper
used its Trusted Publisher. Both original jobs passed actual public artifact
verification. Upload acknowledgement and actual public download checks are
separate evidence.

The governance npm upload was acknowledged before its normal download URL
stopped returning a cached HTTP 404 with a 300-second cache lifetime. The
artifact was not uploaded again to resolve that visibility delay.
Both public npm tarballs then matched the retained SHA512 integrity, SHA256 and
member contents. All four PyPI wheel/sdist downloads matched their registry
digests and retained member bytes/modes. Both registries' current package
pointers report `0.9.0`.

The fresh package-only run validated the original source and verified all four
existing packages. It did not move the tag, replace Go/container assets or
repeat the successful PyPI uploads. Its matching-existing npm checks did not
exercise OIDC publication. The original run's failed authorization outcomes
remain separate from this successful verification.

## Container artifacts

All three versioned indexes contain Linux amd64 and arm64 runtime descriptors.
Attestation descriptors are recorded separately. The observed `latest` tags
resolve to these same v0.9.0 index digests:

- `ghcr.io/getrunkite/runkite`:
  `sha256:4ee88ea197b9da1c8185fd249996bac955a066a1a315766fb7002dd1a3613d28`.
- `ghcr.io/getrunkite/runkite-runner`:
  `sha256:b53abdbb0e4dbfd083d8d1e52262e441a6e2b94e4459e1240a3fedda9429ee59`.
- `ghcr.io/getrunkite/runkite-runner-ts`:
  `sha256:207ec6b3f5ad3f94aec0a6e46d4051f2b37b5e8ed6d33f880d01760b5205e169`.

All six immutable platform digests passed offline, read-only runtime checks:
the expected platform, UID 65532, Apache license label, legal files and version
metadata. The Python images also executed CLI help. Both TypeScript variants
passed the exact tagged provider-free LangGraph and AI SDK image harness,
including native tool execution, streaming and usage assertions.

Trivy 0.72.0 scanned all six published platform digests using remote image
reads, a database updated on November 16, 2026, and the existing policy:
HIGH/CRITICAL vulnerabilities with `--ignore-unfixed` and failure on findings.
All six passed that gate. The scanner had no Docker socket or publishing
credentials. This does not assess unfixed, LOW or MEDIUM findings.

## Upgrade and limits

There is no new SQL migration or Runner Protocol change relative to v0.8.0.
Deploy matching plane and runner artifacts. Upgrade every MCP-serving replica
before enabling stable-ID retries, because older servers can ignore the new
argument. Existing session routing and retention limits still apply. Earlier
upgrades retain their SQL, identity and HTTPS requirements in the
[operations runbook](../ops-runbook.md).

The scripted governance walkthrough and Trinity specialist integration remain
checkout examples. App-owned governance creates no managed thread or run.
Public MCP still returns a result rather than raw progress/token streaming.
Cancellation does not undo external effects. These are maintainer-run bounded
checks, not live-provider qualification, native Trinity certification, a long
production soak or an independent security assessment.
