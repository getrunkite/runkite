# v0.8.0 package recovery

Status: forward recovery preparation and partial publication verified on
November 15, 2026. The original
[Release run 43695507859](https://github.com/getrunkite/runkite/actions/runs/43695507859)
completed its Docker job successfully, and the Go release assets were separately
verified. Both Python packages are public and match their original retained
distributions. All six published image variants passed runtime checks and the
configured vulnerability gate. The npm recovery has not been published. See
the [artifact qualification checkpoint](identity-distribution-qualification.md#actual-partial-publication)
for executed checks, evidence and remaining publication gates. This record extends
[RFE #45, governance helper distribution](https://github.com/getrunkite/runkite/issues/45)
and [RFE #48, immutable publication recovery](https://github.com/getrunkite/runkite/issues/48).
See the [operator procedure](../releasing.md#recover-an-existing-release-without-moving-its-tag)
for dispatch inputs and commands.

## What the first release exposed

The release job installed npm 12.2.2, while the artifact checker assumed
`npm pack --json` always returned a one-element array. That client returns an
object keyed by package name. Indexing the object with `0` failed before the
npm publication and verified-artifact retention steps. A successful source
build had therefore not qualified the actual release client's output contract.

The corrected `scripts/release_artifacts.py:_npm_pack_filename` accepts the
supported array and package-keyed forms. It still requires exactly one package,
the expected name/version and a safe tarball filename. Unknown shapes,
ambiguous JSON and unexpected identities fail. This is a packaging-controller
correction, not permission to change the contents of a published version.

Both PyPI publication steps succeeded. The runner used its project token; the
governance helper used its configured Trusted Publisher. All four wheel/sdist
uploads received HTTP 200. Their following public metadata checks continued to
report missing distributions through the 30-second bound. Those logs establish
an upload acknowledgement followed by an unsuccessful visibility check. They
do not establish the exact CDN, cache or origin cause.

PyPI verification now permits a bounded 300-second wait for explicit metadata
absence or missing files and asks intermediaries to revalidate. Authentication
failures, network errors, malformed responses and content mismatches still fail
immediately. The verifier still requires actual public downloads matching the
intended manifests. Neither an upload acknowledgement nor a longer wait can
substitute for that comparison.

## Keep release source separate from its recovery controller

The immutable `v0.8.0` tag remains at
`c63b8203123f6b7d032b0b5aca59abe9227fd973`. Its Go assets and existing registry
files remain intact. Rerunning that tag would reuse its old workflow and its
old npm parser. The forward fix therefore runs the reviewed `main` workflow as
a controller, with a separate checkout of the original tag's validated commit.

`scripts/prepare_release_recovery.py` restricts recovery to
`getrunkite/runkite`, a stable version tag and an original tag-push Release run
at the same commit. It requires successful original version-check and
GoReleaser jobs plus a matching published GitHub Release. The original source's
version checker then verifies its coordinated package metadata against the tag.
Invalid or incomplete evidence stops recovery before package publication.

The recovery paths deliberately use different evidence:

- PyPI downloads the original retained distributions and manifests. Its recovery
  job verifies identity and public contents without rebuilding or uploading.
  Missing original evidence is a failure, not a reason to invent replacements.
- npm installs the original source's locked dependencies and runs its package
  builds and helper integration checks. The corrected controller builds and
  packs once, validates the intended package/version before any publication
  attempt and retains that exact tarball with its manifest. A matching existing tarball is
  accepted only after immutable integrity and download checks.

Each recovery artifact set retains `source.json` with the original tag, source
run, package-source commit and controller commit. These are distinct identities.
Workflow provenance describes the running controller; retain the receipt to
connect it to the separately validated package source. Do not describe the
controller commit as the immutable tag commit.

Manual recovery skips the ordinary tag version-check, GoReleaser, Docker and
PyPI upload jobs. The npm job requires either the successful normal tag gates
or the successful manual recovery-source gate. Its existing OIDC,
acknowledgement reconciliation and scoped fallback remain visible. A read-only
check of an existing package does not prove a new OIDC publication.

## Regression and acceptance

The new `publish-tools` CI job uses the release npm client, builds both npm
packages through the real artifact command and exercises the installed helper
against the pinned offline AI SDK fixture. It grants only read access to the
repository, with no publisher credential or OIDC permission. This catches a
release-client contract failure before another tag is cut.

Local validation passed 16 artifact/retry tests and 13 recovery-guard tests.
These cover supported npm JSON forms, malformed identities, immutable content
checks, bounded visibility retries and rejection of unrelated tags, runs,
repositories or incomplete job evidence. They use synthetic metadata and no
publishing credentials. The workflow also checks each built or downloaded
manifest against the requested package and release version before proceeding.

Keep the original failed package job outcomes in the record. Record the actual
manual recovery run, artifact hashes and registry verification separately when
it succeeds. Prepared workflows, passing local tests and verified Go/container
publication do not establish that the npm recovery has completed.
