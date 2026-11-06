# Project governance

Status: proposed documentation on this branch. This document takes effect when
merged. It records Runkite's current founder-maintained decision process and
proposes an explicit way to add committers and maintainers.

## Current stewardship

Runkite is maintained by Sharan Harsoor in a personal capacity. The current
maintainer is listed in [MAINTAINERS.md](MAINTAINERS.md), and the default code
owner is recorded in [.github/CODEOWNERS](.github/CODEOWNERS).

The maintainer currently makes final decisions on project scope, design,
compatibility, merge readiness and releases. Runkite does not currently have an
elected technical committee, an independent governing board or a formal voting
membership. These documents do not establish any of those bodies.

## Contributions and decisions

Anyone can open an issue, propose a focused pull request, review a change or
contribute tests and documentation. Follow [CONTRIBUTING.md](CONTRIBUTING.md),
including its existing Apache-2.0 contribution terms and testing expectations.
This governance document adds no contributor agreement or copyright assignment.

For a significant change, open an issue explaining the caller's problem, the
proposed behavior, alternatives and compatibility or operational consequences.
Use a pull request for the implementation and record the reasoning where future
contributors can find it. Document changes to public behavior alongside the
code. Prefer existing interfaces and explicit capability boundaries over claims
of support that have not been tested.

The maintainer reviews the evidence and feedback and records the decision in the
issue or pull request. Discussion should seek a workable agreement; disagreement
does not create a veto or an automatic vote. While the project remains
founder-maintained, the maintainer makes the final decision and should explain
material trade-offs. A contributor may ask for reconsideration with new
evidence or an alternative proposal in the same public discussion.

Security reports and embargoed fixes follow [SECURITY.md](SECURITY.md). Do not
publish vulnerability details to satisfy the ordinary public discussion process.
The maintainer should share the relevant fix and release information when
coordinated disclosure permits.

## Committer and maintainer nominations

The following process is proposed for adding people beyond the current
maintainer:

1. Anyone may nominate themselves or another contributor through a public
   issue. Obtain the nominee's consent before publishing a nomination on their
   behalf. Describe the responsibilities being requested and link relevant
   contributions, reviews or project support.
2. Evaluate demonstrated understanding of the relevant code and runtime
   boundaries, review quality, collaboration and willingness to maintain the
   proposed area. There is no fixed contribution count, employer requirement
   or financial contribution requirement.
3. The current maintainer reviews the nomination, invites community feedback
   and records an acceptance, deferral or rejection with reasons. The nominee
   must explicitly accept the proposed responsibilities. A nomination has no
   deadline or implied approval through silence.
4. Record an accepted appointment and its scope in `MAINTAINERS.md`. Update
   `CODEOWNERS` where appropriate. A committer may be appointed for a limited
   area; the title does not imply authority over every component or release.
5. An authorized administrator separately grants the minimum repository
   permissions needed for that scope. Release publication, organization
   administration and access to credentials are separate decisions. Neither a
   nomination nor a merged documentation change grants those permissions.

New committers follow the same contribution, review and security expectations.
They should request another review for changes beyond their agreed scope and
must not treat write access as a reason to bypass relevant checks. This process
does not assert any particular GitHub branch protection or access configuration.

The current maintainer may revise or end an appointment for a change in
responsibilities, inactivity, a person's request or misuse of access. Seek a
handover and record the public outcome where appropriate, without disclosing
private security or personal information. An authorized administrator must
separately make the corresponding access changes.

## Governance changes and foundation applications

Propose changes to governance in a pull request with the reason and impact.
Invite public review before the current maintainer records a decision. A future
change to shared decision authority should explicitly define who accepts that
responsibility and how decisions and succession will work.

Preparing or submitting an Agentic AI Foundation application does not establish
admission, endorsement, membership or Linux Foundation affiliation. It does not
transfer copyright, trademarks, domains, repositories or credentials. Any future
foundation participation or asset transfer requires its own accepted process,
authority and documentation. This file does not authorize or substitute for such
an agreement.
