# Guide: GitHub token scopes for V0 (local runner)

> Companion to `docs/ARCHITECTURE.md` §9 and `docs/requirements/security.md`
> (`SEC-045`, `SEC-046`). Scope: tiers 1-2 (local runner), where credentials
> come from `instance.yaml` token refs resolved by `internal/credentials`
> (issue #14). Tier-3 identity (Key Vault, Entra) is out of scope here.

## Use fine-grained personal access tokens

Prefer GitHub's **fine-grained PATs** over classic PATs. Classic PATs grant
access to every repo the user can see under a coarse scope (`repo`, `admin:org`,
...); fine-grained PATs are scoped to specific repositories and specific
permissions, which is what `SEC-011` (least privilege) and `SEC-045`
(credentials scoped to declared capabilities) actually require in practice —
a token that is broader than what any goober run declares defeats capability
admission even though the seam correctly refuses to *inject* the excess
access.

## One token ref per capability, not one token for everything

Capability admission (`SEC-042`) is enforced by `internal/credentials`
refusing to materialize a credential for a capability a stage did not
declare. That only holds if the underlying tokens are actually scoped
narrowly — a single all-powerful PAT reused for every capability grant makes
the seam a formality. Mint a separate fine-grained PAT per capability class
your gaggle's workflows actually use, and register each as its own named ref
in `instance.yaml`:

`configrepo:read` is the runner-only exception: only the workflow-config
source may materialize it, directly from `workflowSource.token`.

| Capability | Recommended fine-grained PAT permissions | Notes |
|---|---|---|
| `github:issues:read` | Issues: Read-only | Backlog polling, triage stages. |
| `github:issues:write` | Issues: Read and write | Create, claim, comment, ordinary-label, close. Does not authorize the `goobers:approved` trust decision. |
| `github:milestones:write` | Issues: Read and write | Assign an existing milestone to an issue. Keep roadmap mutation out of stages that only perform ordinary issue writes. |
| `github:issues:approve` | Issues: Read and write | Apply `goobers:approved` to nominated work. Keep this out of workflow stages unless self-approval is intentional. |
| `github:pr:write` | Pull requests: Read and write, Contents: Read and write | Only for stages that open/update PRs. The canonical implementation workflow also uses this capability for `ci-poll`, which requires Checks: Read-only and Commit statuses: Read-only. |
| `github:pr:review` | Pull requests: Read and write | Submit native approve/request-changes reviews. For goober-authored PRs, source this from a different GitHub identity than `github:pr:write`; GitHub forbids self-approval. |
| `repo:push` | Contents: Read and write | Branch + commit + push. Broadest local-tier grant; scope to the exact target repo(s), never an org-wide token. |
| `repo:clone` (read-only stages) | Contents: Read-only | Curation/analysis stages that never push. |
| `configrepo:read` | Contents: Read-only | Runner-only access to the workflow-config repo. Configure only through `workflowSource.token`; stages cannot declare or source it through `credentials`. |
| `agent:model` | Stored Copilot CLI sign-in, or *(Account permissions)* Copilot Requests: Read-only for headless use | Copilot model authentication for agentic stages. An existing per-user CLI sign-in is the local default; a configured PAT is injected as `COPILOT_GITHUB_TOKEN` for services/CI. |

Repository access: select **Only select repositories** and list exactly the
gaggle's target repo(s) — never "All repositories".

### Agentic (Copilot-harness) stages: stored login or `agent:model` token

The permissions above cover the ordinary GitHub API — issues, pull requests,
contents. They are **not** sufficient for an **agentic** stage (any stage whose
goober uses the `copilot` harness — curator, implementer, reviewer, nominator,
analyst, config-author in the shipped gaggle). The GitHub Copilot CLI authenticates to its model backend independently of
repository credentials. For an interactive local daemon, first run `copilot`
and sign in normally. Goobers passes only the profile-location variables needed
to find that stored session; it does not copy ambient token variables.

For a headless Windows Service, CI runner, or dedicated account without a stored
session, configure a separate fine-grained PAT with **Copilot Requests:
Read-only**. A PAT without that account permission fails at the first agentic
stage even when ordinary repository operations work.

Goobers still models model access as **`agent:model`**. When no token grant is
configured, the Copilot adapter uses the stored CLI session. When a grant is
configured, it resolves fail-closed and injects `COPILOT_GITHUB_TOKEN`, distinct
from repo/issue/PR grants injected as `GH_TOKEN`, so neither clobbers the other.

**Cross-org reality — why it must be a separate token.** "Copilot Requests" is
an **account-level** permission: it can only be granted on a **personal**
fine-grained PAT, never on a token scoped to an organization's repositories. So
when your target repo lives in an org, `agent:model`'s token is necessarily a
**different token** from the repo credential:

- **`agent:model` token** — a *personal* fine-grained PAT with **Copilot
  Requests: Read-only** and **no repository access at all** (it authenticates
  the model, nothing else).
- **repository capability tokens** — org-scoped fine-grained PATs with the
  narrow Contents, Issues, Pull-requests, Checks, and Commit-statuses permissions
  required by the selected workflows. An org owner must **approve** personal
  fine-grained PATs before they can access org repos (org *Settings → Third-party
  Access → Personal access tokens*), so budget for that approval step.

(For a target repo under your **personal** account, one personal PAT carrying
both the repo permissions *and* Copilot Requests can back both capabilities —
point `agent:model` and the repo grants at the same ref. The two-token split is
mandatory only across the org boundary above.)

For local stored auth, omit the `agent:model` credentials entry. For headless
use, wire the model token alongside the repository capability tokens:

```yaml
repos:
  - provider: github
    owner: your-org
    name: your-repo
    token:
      env: GOOBERS_GITHUB_REPO_TOKEN # Contents: read-only
credentials:
  - capability: github:issues:write
    token:
      env: GOOBERS_GITHUB_ISSUES_TOKEN # Issues: read and write
  - capability: github:pr:write
    token:
      env: GOOBERS_GITHUB_PR_TOKEN   # PR/Contents: read-write; CI Checks/statuses: read-only
  - capability: repo:push
    token:
      env: GOOBERS_GITHUB_PUSH_TOKEN # Contents: read and write
  - capability: agent:model
    token:
      env: GOOBERS_COPILOT_TOKEN     # Copilot Requests: read-only; no repo access
```

Each `credentials:` entry sources one capability from its own token ref; an
entry for a capability the repo token would otherwise back **overrides** it (so
an issues-only stage never receives a token carrying code or PR authority).
Omit overrides for capabilities no selected workflow declares. Values are never
inline — `token.env` or `token.file` only (`CFG-009`/`SEC-010`).

Omitting only the `agent:model` entry opts into stored Copilot CLI
authentication. Missing grants for repository capabilities remain errors.

Verify before a live run: `goobers validate --check-harness` preflights the
Copilot CLI (and, when `AuthCheckArgs` is configured, its authentication) so a
mis-scoped token fails fast at validation rather than mid-run.

## GitHub App installation tokens (`auth.kind: github-app`)

Instead of a static PAT, a repo can authenticate through a **GitHub App
installation**: Goobers signs a short-lived App JWT with the App's private key
and exchanges it for an **installation token** on every run that needs one
(#686, `docs/design/v2-cloud-scale.md` §5 D4). Minted tokens flow everywhere
the repo token flows — capability grants, `gh`/API stages, mirror clone/fetch,
`push-branch` — with no other configuration:

```yaml
repos:
  - provider: github
    owner: your-org
    name: your-repo
    auth:
      kind: github-app
      appId: 123456            # or the App's client ID string
      installationId: 987654
      privateKey:
        file: /run/secrets/goobers-app.pem   # env: or store: work too
```

Exactly one identity mechanism per repo: `github-app` **forbids** `token`
(there is no silent PAT fallback — a failed mint fails the stage closed), and
an absent `auth` block keeps today's PAT behavior byte for byte.

**Why short-lived tokens for regulated environments.** Installation tokens
expire after about an hour and are minted on demand, so there is no long-lived
repo credential to rotate, escrow, or leak: a token captured from a log or a
compromised stage is dead within the hour, and revocation is immediate and
coarse (uninstall the App or rotate one key) instead of a hunt across every
copied PAT. Access is also auditable as the **App's own identity** — commits,
PRs, and API calls attribute to the app, not to whichever employee's PAT
happened to be pasted into the instance — and the blast radius is pinned to
the installation's repository list and permission set rather than a person's
entire grant. The remaining long-lived secret is the App private key, which
never leaves the daemon process (stages, git subprocesses, and providers only
ever see minted tokens) and can itself live in a secret store via
`privateKey.store`.

**Installation permissions.** Grant the App the union of what the selected
workflows' capabilities need — same table as above: Contents (Read and write
for `repo:push`, Read-only for clone-only), Issues (Read and write), Pull
requests (Read and write), Checks + Commit statuses (Read-only, for
`ci-poll`). Install it on **only the target repositories**.

**Limits.**

- `agent:model` cannot come from an App: "Copilot Requests" is an account
  permission on a personal fine-grained PAT. Keep the `credentials:` entry (or
  stored Copilot CLI login) from the section above.
- GitHub forbids self-approval: `github:pr:review` on goober-authored PRs
  still needs a second identity (the App counts as one identity).
- Per-capability `credentials:` overrides still work and still win over the
  repo credential for their capability — they remain static token refs.

Verify with `goobers validate`: the repository preflight performs a real
token exchange, so a missing installation or rejected key fails there with
GitHub's diagnosis instead of mid-run.

## Least privilege per workflow

A workflow's declared capabilities (`goober.md` GBO-052, `task.md` TSK-042)
should be the *union* of what its stages need, nothing more. When scoping a
new workflow:

1. List each stage and the GitHub operations it performs.
2. Map each operation to the narrowest capability above.
3. Only add a grant (`credentials.Grant{Capability, Ref}`) for capabilities
   actually declared by a stage in that workflow — an unused grant is an
   unused blast radius if the ref's underlying token is ever exposed.
4. If two workflows need the same capability against the same repo, they can
   share a token ref; don't mint one PAT per workflow when one per
   *capability* covers it.

For an intentionally self-directed nomination workflow, opt in at the stage
that files issues:

```yaml
- name: nominate
  type: agentic
  goober: nominator
  capabilities:
    - github:issues:write
    - github:issues:approve
```

The nominator goober definition must also list `github:issues:approve` as an
allowed capability. Omitting it from the workflow stage preserves the default:
nominated issues carry no trust label and wait for maintainer approval.

## Token lifecycle

- Store the PAT value in an env var or a file referenced by `instance.yaml`
  (`SEC-046`) — never in `config/` or committed anywhere.
- Fine-grained PATs support a mandatory expiration; set the shortest
  expiration your operational cadence tolerates and rotate before it lapses.
  `internal/credentials.Resolver` re-reads the env var/file on every
  resolution, so rotating the underlying value takes effect without
  restarting `goobers up`.
- If a token leaks (committed by accident, printed in a log a scrubber
  pattern missed), revoke it in GitHub first, then run
  `goobers journal redact` to remediate any journal blob that captured it
  before the scrubber saw it (`ARCHITECTURE.md` §4).
