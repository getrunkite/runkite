# Apache-2.0 transition

## Decision and scope

Runkite's first-party source changes from BUSL-1.1 to the unmodified Apache
License, Version 2.0. The change applies from the relicensing commit onward and
to future releases built from that source. The purpose is to remove the
source-available restriction on third-party hosting and make commercial use,
redistribution and contribution possible under a standard open-source license.
It does not establish demand, foundation acceptance or investment.

The original license identified Sharan Harsoor as the copyright owner. The new
`NOTICE` files retain that attribution. Licensing grants permissions; it does
not transfer copyright, company ownership or trademarks to Apache or CNCF.
Apache-2.0 also grants the patent permissions described in section 3 and limits
trademark permissions in section 6. Its text, including the appendix, is kept
intact in each first-party `LICENSE` copy.

Runkite remains a self-hosted control plane. Permitting others to host a fork
does not announce a hosted Runkite service. SDK availability, support tiers,
runtime guarantees and the distinction between managed execution and
decision-only governance are unchanged.

## Published versions

The v0.6.0 release and earlier published artifacts were distributed under
BUSL-1.1. Their tags, archives, npm/PyPI versions and images retain their
shipped license terms, including the change-license provisions in those
licenses. This change does not rewrite those artifacts or issue an additional
license grant for those historical versions.

Current-source documentation can say Apache-2.0 while linking to v0.6.0 for
downloads only if it explains that boundary. The existing changelog entries
remain historical. The first Apache-licensed package cut is v0.6.1.
Older published versions keep their shipped terms. Never replace an existing package or image version to make its license
match the current branch.

## Rights review before merge

The repository history can identify authors and attribution; it cannot prove
copyright ownership or permission to relicense. Before merging, the owner must
confirm rights to the first-party work, including any employer or contractual
obligations and any contributor permission needed. Prior submission under
BUSL-1.1 alone is not evidence of permission to relicense another person's
contribution under Apache-2.0. This record does not certify that review as done.

Third-party dependencies and bundled assets retain their own licenses. Existing
notices must survive the transition. A complete transitive dependency license
inventory is separate work; changing the root license does not license those
dependencies under Apache-2.0 or establish foundation IP-policy compliance.

## Distribution paths

- Root `LICENSE` and `NOTICE` cover the first-party repository source. GoReleaser
  includes both in binary archives.
- `python/`, `python/runkite_governance/`,
  `typescript/runkite-runner/`, `typescript/runkite-governance/` and
  `runner-protocol/` carry standalone license and notice copies. Python metadata
  uses the Apache-2.0 SPDX expression and includes both files in wheels and
  sdists. npm manifests and lockfiles use Apache-2.0 for Runkite entries and
  include both files in packed distributions.
- The control-plane and runner Dockerfiles declare the first-party license in
  `org.opencontainers.image.licenses` and copy legal files into
  `/usr/local/share/licenses/`. Base images and installed dependencies keep
  their own terms; the OCI label is not a dependency license inventory.
- Inter and JetBrains Mono font binaries retain OFL-1.1. Their upstream copyright
  and full license texts live in `admin-ui/public/licenses/`, are copied by Vite
  into `internal/adminui/dist/licenses/`, and are embedded by
  `internal/adminui/adminui.go`. They are available under `/admin/licenses/`.
  Go archives include them in `licenses/`; the control-plane image also includes
  them in `/usr/local/share/licenses/runkite/fonts/`.
- The public OpenAPI document contains definitions derived from LangChain's
  Agent Protocol. Its original MIT notice is preserved in
  `spec/third-party/agent-protocol-MIT.txt` and embedded in
  `spec/openapi.json` as `x-third-party-notices`, so the standalone JSON download
  retains the notice. Go archives and the control-plane image also carry the
  notice file. `scripts/openapi/build.py` preserves this attribution when
  rebuilding the specs and adds current first-party license metadata.
- `runner-protocol/` is mirrored by the existing publication workflow when
  merged on `main`. No separate edit to the public mirror is part of the local
  preparation. That workflow must succeed before the mirror can be described
  as updated.

The governance helpers remain unpublished. Adding open-source metadata does not
add them to the release workflow or publish a new package.

## Verification and release handoff

1. Review the first-party rights and third-party notice scope above.
2. Confirm all six first-party license copies contain the complete official
   Apache-2.0 text and all six notice files retain copyright attribution.
3. Build and inspect npm tarballs, Python wheels and sdists. Check their actual
   license metadata and notice contents, including source-distribution rebuilds.
4. Verify Go archive inputs, Docker legal-file paths and the Admin embedded
   font notice routes. Rebuild the Admin bundle when changing its assets;
   ordinary builds must keep the `public/licenses/` files.
5. Regenerate OpenAPI and run the route/contract checks. Confirm schemas and
   operations are unchanged by the license-only metadata addition and that the
   standalone public spec retains the full upstream notice.
6. Scan current documentation and package metadata for obsolete BUSL
   restrictions. Keep historical release notes and explicitly labeled history.
7. Cut a new version using the normal release process, then verify the new
   artifacts before announcing their license. Keep old downloads accurately
   labeled. Do not claim CNCF or ASF affiliation merely for adopting Apache-2.0.

This transition changes licensing and distribution attribution. It introduces
no control-plane behavior change and no new SDK, provider or hosted service.

## References

- [Apache License, Version 2.0](https://www.apache.org/licenses/LICENSE-2.0)
- [Apache licensing guidance](https://www.apache.org/foundation/license-faq.html)
- [Runkite v0.6.0 license](https://github.com/getrunkite/runkite/blob/v0.6.0/LICENSE)
- [Agent Protocol MIT license](https://github.com/langchain-ai/agent-protocol/blob/main/LICENSE)
- [Inter license](https://github.com/rsms/inter/blob/master/LICENSE.txt)
- [JetBrains Mono license](https://github.com/JetBrains/JetBrainsMono/blob/master/OFL.txt)
