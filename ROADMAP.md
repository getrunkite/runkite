# Runkite community roadmap

Proposed on 6 November 2026. This roadmap describes priorities for review, not
delivery guarantees. Runkite is applying for AAIF Sandbox consideration; it is
not an admitted AAIF project. The milestones below are measured from admission
if the application succeeds. Useful work can start before that decision.

The priority is to make the existing operating model easier to adopt and
maintain across independent teams. Adding framework names is less useful than
showing that an integration recovers correctly and that another maintainer can
support it.

## First three months: make contribution and evaluation repeatable

- Publish the current maintainer list and decision process. Invite contributors
  through small, scoped issues for documentation, reproduction cases and
  protocol compatibility. Recognize authorship and review work publicly.
- Keep a clean-install walkthrough and an offline, reproducible MCP example
  usable with the current release. State which components execute agents,
  which calls cross policy gates and which credentials each process receives.
- Recheck the packaged runner capability matrix against dispatch, streaming,
  cancellation, committed-turn recovery, usage and tenant isolation. Link
  limitations to the tests that establish each supported behavior.
- Extend the [dependency inventory](docs/foundation/dependency-scope.md) into a
  release-artifact inventory covering resolved Python dependencies, bundled
  JavaScript, Go binaries and container contents. Preserve required notices
  and resolve licensing questions before making foundation IP attestations.

## By the six-month checkpoint: gather independent evidence

- Invite independent teams to evaluate real workflows on infrastructure they
  own. Publish deployment reports only with their permission, distinguishing
  experiments from production use. Ask what they could not operate reliably.
- Turn recurring onboarding and recovery problems into examples, tests or
  deletions of unnecessary setup. Keep the supported Postgres/Redis HA path
  distinct from local SQLite demonstrations and bounded recovery rehearsals.
- Seek contributors from outside the founder's project work. Use the
  [maintainer nomination process](GOVERNANCE.md#committer-and-maintainer-nominations) when a
  contributor wants continuing responsibility. A commit does not imply
  organizational sponsorship or grant repository access automatically.
- Publish progress against this roadmap, unresolved risks, contributor
  activity and adoption evidence. If admitted, provide that record for the
  Sandbox checkpoint rather than presenting interest as adoption.

## By twelve months: establish a credible Growth case or reassess

The target is to meet the then-current AAIF Growth criteria, including
documented production use by two unaffiliated organizations, commits from at
least two organizations over the preceding six months, named committers and a
documented acceptance process. None of those community or adoption outcomes is
claimed as achieved today.

A Growth application would also need a TC sponsor and an accepted growth plan
with a project-specific definition of Impact readiness. For Runkite, that plan
should connect independent operation of the supported deployment to shared
maintenance of its protocol, recovery and security boundaries. Counts alone
would not establish sustainable maintenance.

AAIF expects Sandbox projects to apply for Growth within twelve months and may
open an Emeritus discussion otherwise. If evidence does not support graduation,
publish the gaps and discuss the appropriate status with the foundation. Do
not turn a target into an unsupported graduation claim.

## Work deliberately outside this roadmap

Runkite will not build a hosted service merely to support a foundation
application, replace SDK model loops, absorb another project's runtime or UI,
or claim enforcement over tools that bypass its gates. A protocol integration
should use the public boundary and leave each project's responsibilities clear.

The proposed foundation relationship is about neutral stewardship and a wider
maintainer base. Sandbox infrastructure is not a commitment of foundation
marketing, funding, mentorship or security scanning. See the
[AAIF lifecycle policy](https://github.com/aaif/technical-committee/blob/main/governance/project-lifecycle-policy.md).

## Updating this roadmap

Discuss a change in a public issue or pull request, explain the caller and
evidence behind the priority, and record the decision under
[project governance](GOVERNANCE.md). Release availability continues to follow
[CHANGELOG](CHANGELOG.md), not a roadmap target.
