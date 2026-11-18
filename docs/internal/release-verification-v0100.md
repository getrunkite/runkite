# v0.10.0 publication verification

Status: actual publication verified on November 18, 2026. All eight GitHub
assets, four registry projects with six distribution files and all six container
platform variants passed verification. The
[package-only verification run](https://github.com/getrunkite/runkite/actions/runs/44305000833)
completed successfully after the original uploads and npm runner recovery.

## Source and publication

The immutable annotated `v0.10.0` tag identifies commit
`33cf561355e3b7284e1964de4b7f763d2925233b`, tree
`2e883bca2546a4b6eec7a73fcb6ef1d0c7b9342d`. The tagger is Sharan Harsoor
using `sharanharsoor@gmail.com`; the normal push used personal account
`sharanharsoor`. No earlier tag or registry version was replaced.

[PR #94](https://github.com/getrunkite/runkite/pull/94) prepared the final
package READMEs and qualification notes. Its tested tree matches the tagged
merge exactly. The tagged merge passed
[main CI](https://github.com/getrunkite/runkite/actions/runs/44298168726),
including the full end-to-end suite and image, package, OpenAPI and Admin gates.
The [candidate record](release-candidate-v0100.md#merged-main-qualification-november-18-2026)
retains the separately executed hosted three-cycle HA and integration evidence
at `7a7ae07`. Only four Markdown files differ from that qualified runtime source.

The original tag-push [Release run](https://github.com/getrunkite/runkite/actions/runs/44302235493)
is the publication controller. Continued OIDC step conclusions, matching-existing
checks and upload acknowledgements alone do not establish actual publication.

## Published Go and OpenAPI assets

The actual Release contains four macOS/Linux archives, `checksums.txt` and three
OpenAPI JSON files. All eight public downloads match GitHub's asset SHA256 and
size metadata. Archive checksums match the downloaded checksum file. Legal
files, README and all three OpenAPI documents match the immutable tag. All 19
tracked Admin assets occur byte-for-byte in every binary, for 76 comparisons.
Go build metadata identifies full source `33cf561`, `vcs.modified=false`,
Go 1.26.11 and `CGO_ENABLED=0`.

All four downloaded archive binaries executed `version` and reported `0.10.0`,
commit `33cf561` and build time `2026-11-18T12:16:13Z`. macOS arm64 ran natively;
macOS amd64 used the installed Rosetta runtime. Linux probes used matching
prequalified candidate images only as execution bases, with networking disabled,
read-only filesystems, UID 65532 and confirmed cleanup. Those bases identify
`7a7ae07`; they are not the published v0.10.0 images. The local Linux engine is
arm64, so Linux amd64 uses userspace emulation. Literal Admin asset membership
and version probes do not establish browser behavior on every platform.

## Registry publication and authentication

The original Release uploaded the PyPI runner through its project token and
the PyPI governance helper through its Trusted Publisher. Each uploaded its
wheel and source distribution and passed its public-artifact check.

The npm governance helper's fresh Trusted Publishing upload succeeded and
published provenance. Its original job failed at the subsequent short public
visibility check. An upload acknowledgement and later public-byte verification
remain separate evidence.

The npm runner's automated OIDC exchange failed with `ENEEDAUTH`; its configured
fallback failed with `E404`. The continued OIDC step's successful conclusion is
not an authentication success. The maintainer then published the original
retained CI tarball through personal account `sharanharsoor`, using npm 12.2.2
and npm's direct verification. Publication scripts were disabled. The tarball
was not rebuilt or repacked, and no existing version was replaced. This personal
publication does not prove the runner's configured Trusted Publisher works.
Its publishing authority needs investigation before the next automated release.

All six public npm/PyPI files match the exact retained original CI bytes and
registry digests. Their 276 regular members include 250 direct immutable-source
comparisons and two generated governance files independently derived from the
exact tagged source with locked TypeScript 7.0.2. Generated compiler output is
recorded separately from tracked files. All packaged legal files and READMEs
match; wheel metadata carries the correct Apache license and version.
Both registries' latest pointers report `0.10.0` for both projects.

The npm tarball SHA256 values are:

- `runkite-runner-0.10.0.tgz`:
  `c8f8c3b7dec9e6060c41a983f7774552d1f6361430e29bf82c76aeceb39d0bf2`.
- `runkite-governance-0.10.0.tgz`:
  `dce57d8d7b5a8cc8de85b0fe110ef9b8d172f7c3fbb8e096a14546f7e1f97269`.

Installed contracts consumed only those verified public files, including the
runner's strict-mTLS tests against the actual downloaded native plane binary
and the governance helper's native AI SDK and Microsoft middleware checks.
Python source distributions were rebuilt only into private consumer wheels for
installation tests. No replacement release payload was built or uploaded.

The successful package-only recovery verified original PyPI files and matching
existing npm packages. It did not repeat uploads, exercise fresh publishing
authority or rebuild Go/images. The original run's failed outcomes remain part
of the record. A post-tag controller follow-up extends npm's public visibility
wait from 30 to 300 seconds while retaining immediate failure for authentication,
network, malformed-response and content-mismatch errors. This does not change
the immutable v0.10.0 runtime source or fix the runner's OIDC configuration.

## Container artifacts

All three public versioned indexes match the successful Docker job's logged
push digests and contain exactly Linux amd64 and arm64 runtime descriptors.
Attestations are recorded separately. Observed `latest` tags resolve to the same
indexes as `0.10.0`:

- `ghcr.io/getrunkite/runkite`:
  `sha256:0d4fbf4f88aeadb5a32962b0c61c45079a1444dfe1f64f25193bd1eccfea4dc2`.
- `ghcr.io/getrunkite/runkite-runner`:
  `sha256:46ec148cce6b833e5eefb423937b6427324f13cb715b74d3a23411622dadda32`.
- `ghcr.io/getrunkite/runkite-runner-ts`:
  `sha256:5e495796e17889bf18a6593abb7736ab2eb0ab4ef32d808222544a1e9698a306`.

Every immutable platform digest passed its offline, read-only runtime checks:
expected architecture, default UID 65532, version, license label and exact legal
content. Both Python variants executed CLI help and the provider-free LangGraph
probe. Both TypeScript variants passed the tagged native LangGraph and AI SDK
image fixture, including local tools, streaming and usage. Owned containers were
removed and their absence confirmed. The arm64 Linux engine uses userspace
emulation for amd64; this is separate from hosted native Ubuntu qualification.

Trivy 0.72.0 read all six published platform digests remotely using a freshly
downloaded database updated `2026-11-18T07:15:21Z`. All six passed the existing
HIGH/CRITICAL, `--ignore-unfixed`, fail-on-findings policy. The scanner had no
Docker socket or publishing credentials. This is not a zero-vulnerability claim
or an assessment of unfixed, LOW or MEDIUM findings.

## Upgrade and limits

Follow the [coordinated upgrade sequence](release-candidate-v0100.md#upgrade-sequence)
before deploying: stop and drain writers, retain restorable backups, apply
Postgres/SQLite migration 23 or MySQL migration 24, and deploy matching
protocol-1 planes, runners and custom integrations. Legacy accounting, hook
claims and ambiguous receipts require explicit reconciliation. Kafka's weaker
mode needs explicit opt-in even with Redis. Rollback requires a reviewed backup
restore and accounting reconciliation.

The accepted Admin build-tool exception for `GHSA-fc5q-39xc-vjpf` remains bounded
to its exact manifest, lockfile and five-finding report. It expires November 25,
2026 at 00:00 UTC and is not a claim that the advisory is fixed. Existing image
scan policy checks HIGH/CRITICAL findings with `--ignore-unfixed`; it does not
assess all vulnerabilities. The external OpenShell profile remains experimental,
and independent security assessment and sustained production soak remain outside
this release's maintainer qualification.

Langflow, promptfoo, AG-UI and other application/qualification examples remain
checkout files. Their interoperability checks do not establish upstream
endorsement, every native feature, business-effect idempotency or live-provider
behavior. App-owned decisions still create no managed Thread or Run and do not
apply managed-run kill switches or budgets.
