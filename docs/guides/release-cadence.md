# Proposed release cadence and mechanics

> Status: proposed

This document defines the intended cadence and source-control mechanics for a
sustained Goobers release train. It covers version selection, candidate
creation, promotion, stabilization branches, and patch releases. Packaging and
signing details remain in [Releases & packaging](releases.md); the design of
individual validation suites is outside this document.

## Release model

Goobers uses a trunk-based model with `main` as the development line and, at
most, one short-lived stabilization branch by default. If the current stable
release is `vX.Y.Z`, the next planned feature release is normally
`vX.(Y+1).0`. The exact target version is declared before its first beta rather
than inferred at tag time.

Each candidate is an immutable SemVer tag:

- Beta: `vX.Y.0-beta.N`
- Release candidate: `vX.Y.0-rc.N`
- Stable: `vX.Y.0`
- Stable patch: `vX.Y.Z`

Candidate numbers increase monotonically for a release target. A failed or
withdrawn candidate leaves a gap; tags and published artifacts are never
replaced or reused.

## Weekly cadence

| Time | Automated action | Source |
|---|---|---|
| Monday-Wednesday nights | Qualify the latest eligible `main` commit and publish the next beta | `main` |
| Thursday night | Select the latest qualified beta commit, create the stabilization branch, and publish `rc.1` | `release/vX.Y` |
| During stabilization | Publish a new `rc.N` after each accepted blocker fix passes candidate validation | `release/vX.Y` |
| After final approval | Promote the final release-candidate commit to the stable version | `release/vX.Y` |

A scheduled run is allowed to do nothing. It does not publish a beta when
`main` has no eligible changes or when qualification fails, and it does not cut
an RC without a qualified beta. A missed candidate is not backfilled.

Only one stabilization branch is active under the default policy. If a release
is still stabilizing on the next Thursday, automation continues that train
instead of opening another RC branch. Overlapping stabilization branches
require an explicit human decision.

## Candidate and promotion mechanics

### Betas

Betas are snapshots of qualified commits on `main`; they do not receive their
own branches. A beta defect is fixed on `main` and appears in the next beta.
Previously published beta tags remain unchanged.

The Monday-Wednesday schedule provides regular feedback without making a
release from a commit that has not passed the current nightly qualification
gate.

### Release candidates

On Thursday, automation creates `release/vX.Y` at the commit selected from the
latest qualified beta and publishes `vX.Y.0-rc.1` from that commit. The branch
is a stabilization boundary, not a second development line. `main` remains
open for normal work, while the release branch accepts only fixes needed to
ship the target release.

Fixes should land on `main` first and then be backported to the release branch.
When urgency or branch differences make that impractical, a fix may land on
the release branch first only if an immediate forward-port pull request is
opened for `main`. Synchronization is tied to the fix, not delayed until the
next RC tag.

Every accepted change to the release branch requires a new `rc.N`. An RC tag is
never moved, and validation of one RC does not transfer to a different commit.

### Stable promotion

Stable publication is a deliberate human action after the final RC has met its
release criteria. The stable tag must identify the exact source commit used by
the approved RC: no code changes are permitted between final-candidate
validation and stable promotion. Stable artifacts may be rebuilt with the
stable version identity, but their source tree is unchanged.

After publication, automation verifies that every logical change on the
release branch is present on `main`, then deletes the branch. It does not merge
the stabilization branch wholesale into `main`; backports and forward ports
make the history relationship explicit and avoid reintroducing release-only
changes.

## Patch releases

A stable defect uses a short-lived branch created from the affected stable tag.
The fix normally lands on `main` first and is backported to that branch. An
emergency release-first fix must be forward-ported to `main` immediately.

After targeted qualification and human approval, automation publishes the next
patch version, verifies that the fix exists on `main`, and deletes the patch
branch. Patch branches contain only the fixes required for that patch; they are
not used for unrelated development.

## Release channels

Consumers may select one of three update channels:

| Channel | Receives |
|---|---|
| `stable` | Stable releases only |
| `preview` | Release candidates, followed by the stable promotion |
| `beta` | Betas, release candidates, and stable promotions |

Promotion changes channel pointers or metadata; it does not mutate an existing
tag or artifact. Explicit version pins remain on the selected version until the
consumer changes them.

## Automation and validation boundary

GitHub Actions is the release control plane. Scheduled and manually approved
workflows serialize candidate numbering, create branches and tags, invoke the
existing build/sign/publish pipeline, and record the result. Stable publication
uses a protected environment or equivalent approval gate.

Release automation consumes validation outcomes rather than owning every
validation implementation. Normal qualification includes repository CI and an
integration canary against synthetic repositories. Longer soak scenarios may
run independently and be required as recorded evidence for the final RC or
stable approval. Whether a check is implemented as a GitHub Actions job, a
Goobers workflow, or a validation gaggle does not change the release mechanics:
a required failure blocks that candidate, while an unavailable optional check
is reported without silently becoming a pass.

## Operating rules

- A release target and candidate number have one immutable source commit.
- Release creation is single-flight so two scheduled or manual runs cannot
  reserve the same version.
- Automation aborts before publication when required qualification fails.
- Retrying a failed run either resumes the unpublished candidate safely or
  allocates a new candidate number; it never rewrites a published release.
- Stable releases always require human approval.
- Abandoning a train closes its release branch but preserves its tags and
  audit history.
