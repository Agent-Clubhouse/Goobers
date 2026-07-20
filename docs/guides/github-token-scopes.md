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

| Capability | Recommended fine-grained PAT permissions | Notes |
|---|---|---|
| `github:issues:read` | Issues: Read-only | Backlog polling, triage stages. |
| `github:issues:write` | Issues: Read and write | Create, claim, comment, ordinary-label, close. Does not authorize the `goobers:approved` trust decision. |
| `github:issues:approve` | Issues: Read and write | Apply `goobers:approved` to nominated work. Keep this out of workflow stages unless self-approval is intentional. |
| `github:pr:write` | Pull requests: Read and write, Contents: Read and write | Only for stages that open/update PRs. |
| `github:pr:review` | Pull requests: Read and write | Submit native approve/request-changes reviews. For goober-authored PRs, source this from a different GitHub identity than `github:pr:write`; GitHub forbids self-approval. |
| `repo:push` | Contents: Read and write | Branch + commit + push. Broadest local-tier grant; scope to the exact target repo(s), never an org-wide token. |
| `repo:clone` (read-only stages) | Contents: Read-only | Curation/analysis stages that never push. |
| `agent:model` | *(Account permissions)* Copilot Requests: Read-only — **not** a repository permission | Copilot model authentication for any agentic (`copilot`-harness) stage. Sourced from its **own** token (a personal fine-grained PAT), injected as `COPILOT_GITHUB_TOKEN`; needs no repo/issue/PR access. See ["the `agent:model` token"](#agentic-copilot-harness-stages-the-agentmodel-token) below. |

Repository access: select **Only select repositories** and list exactly the
gaggle's target repo(s) — never "All repositories".

### Agentic (Copilot-harness) stages: the `agent:model` token

The permissions above cover the ordinary GitHub API — issues, pull requests,
contents. They are **not** sufficient for an **agentic** stage (any stage whose
goober uses the `copilot` harness — curator, implementer, reviewer, nominator,
analyst, config-author in the shipped gaggle). The GitHub Copilot CLI
authenticates to its **model backend** with a token, and that requires a
**separate fine-grained permission — "Copilot Requests" (Account permissions →
Copilot Requests: Read-only)** — unrelated to any repo/issue/PR access. A PAT
without it authenticates fine for deterministic stages (`backlog-query`,
`open-pr`, `issue-close-out`) but **fails immediately at the first agentic
stage** with `Authentication failed … ensure it has the 'Copilot Requests'
permission` (#284) — even though claims, comments, and PRs all work.

Goobers models model authentication as its own capability, **`agent:model`**,
sourced from its **own** token and injected into the agentic subprocess as
**`COPILOT_GITHUB_TOKEN`** (which the Copilot CLI prefers over `GH_TOKEN` for
model auth). This is the multi-token model (#287–#289): one agentic stage can
hold *two* tokens at once — `agent:model → COPILOT_GITHUB_TOKEN` for the model,
and its repo/issue/PR grants → `GH_TOKEN` for the `github` tool — because they
are distinct env vars that never clobber each other. Classic PATs (`ghp_`) are
**not** accepted by the Copilot CLI at all; agentic stages require a
fine-grained (v2) PAT regardless.

**Cross-org reality — why it must be a separate token.** "Copilot Requests" is
an **account-level** permission: it can only be granted on a **personal**
fine-grained PAT, never on a token scoped to an organization's repositories. So
when your target repo lives in an org, `agent:model`'s token is necessarily a
**different token** from the repo credential:

- **`agent:model` token** — a *personal* fine-grained PAT with **Copilot
  Requests: Read-only** and **no repository access at all** (it authenticates
  the model, nothing else).
- **repo/issue/PR token** — the org-scoped fine-grained PAT with the
  Contents/Issues/Pull-requests permissions from the table above. An org owner
  must **approve** a personal fine-grained PAT before it can access org repos
  (org *Settings → Third-party Access → Personal access tokens*), so budget for
  that approval step on the repo credential.

(For a target repo under your **personal** account, one personal PAT carrying
both the repo permissions *and* Copilot Requests can back both capabilities —
point `agent:model` and the repo grants at the same ref. The two-token split is
mandatory only across the org boundary above.)

Wire the tokens with a `credentials:` block in `instance.yaml`: the repo token
stays on the repo's `token:` ref, and `agent:model` gets its own ref.

```yaml
repos:
  - provider: github
    owner: your-org
    name: your-repo
    token:
      env: GOOBERS_GITHUB_TOKEN      # org repo PAT → GH_TOKEN (github tool, repo/issue/PR)
credentials:
  - capability: agent:model
    token:
      env: GOOBERS_COPILOT_TOKEN     # personal "Copilot Requests" PAT → COPILOT_GITHUB_TOKEN
  - capability: github:pr:review
    token:
      env: GOOBERS_GITHUB_REVIEW_TOKEN # different GitHub identity for native reviews
```

Each `credentials:` entry sources one capability from its own token ref; an
entry for a capability the repo token would otherwise back **overrides** it (so
`repo:push` can be pointed at a distinct token too, if ever needed). Values are
never inline — `token.env` or `token.file` only (`CFG-009`/`SEC-010`).

Verify before a live run: `goobers validate --check-harness` preflights the
Copilot CLI (and, when `AuthCheckArgs` is configured, its authentication) so a
mis-scoped token fails fast at validation rather than mid-run.

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
