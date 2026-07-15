# Design: Multiple capability-scoped credentials (per-capability token sourcing)

> Status: **Draft for review** · Area: `SEC` / `area:runner` · Milestone: **V0.2** (unblocks
> the #30 live-run gate) with a defined path to per-stage isolation.
> References: `docs/ARCHITECTURE.md` §9, `docs/requirements/security.md` (SEC-042/045/046),
> `docs/guides/github-token-scopes.md`, `internal/credentials`,
> `cmd/goobers/runnerwiring.go`.

## 1. Problem

A single instance today runs on **one GitHub token**, and that assumption has now hit a
hard wall on the live loop (#30):

- The **Copilot CLI** (every agentic stage: curator, implementer, reviewer, nominator,
  analyst, config-author) authenticates its model requests with a fine-grained PAT that
  carries the **"Copilot Requests"** permission. That permission is **only grantable on a
  personal-account fine-grained PAT** — it cannot be added to an org-owned token (upstream
  gap: `github/copilot-cli#223`).
- The instance's actual work targets a repo in a **different org** (`Agent-Clubhouse/Goobers`):
  issue claim/label/comment/close, branch push, PR open. Those need a token **authorized
  against that org's repo** (Issues/PRs/Contents).

A personal Copilot-Requests PAT and an org-repo-authorized PAT are, in the general case,
**two different tokens** (org PAT policy is a separate approval axis, and the personal token
Mason can mint is Copilot-Requests-only). The system must therefore hold **more than one
credential at a time** and route each to the capability that needs it. Today it can't.

## 2. Current state — what's built vs. what's missing

**Already built (not the gap):** `internal/credentials` is fully multi-token capable. Its
`Resolver` takes an arbitrary list of named `TokenRef`s (each an env var or a file), and its
`Injector`/`Grant` map an arbitrary `[]Grant{Capability, Ref}` to those distinct sources,
fail-closed: nothing is materialized for a capability a stage didn't declare
(`ErrUndeclaredCapability` / `ErrNoCredentialForCapability`, SEC-042/045).

**The gap is entirely in the wiring + config surface:**

1. **`cmd/goobers/runnerwiring.go:buildCredentials` collapses to one token.** Its own doc
   comment states the V0 simplification: "the first configured repo's token backs every
   credentialed capability." It grants `repo:push` + `github:issues:write` +
   `github:pr:write` all to `repos[0].token`. There is no way, from config, to point a
   capability at a *different* token — even though the `credentials.Grant` model supports it
   and `docs/guides/github-token-scopes.md` already *describes* a "one token ref per
   capability" model as if it existed.
2. **There is no capability representing "agentic model access."** The Copilot CLI isn't a
   capability at all; it inherits whatever token the agentic stage's declared capability
   resolves to (typically the repo token). So "the Copilot token" and "the repo-push token"
   are definitionally the same value — by design, not oversight.
3. **`instance.yaml` has no per-capability token field.** Each repo has exactly one `token:`
   ref; capabilities can't be individually sourced.

## 3. Design

Three additive changes; no rework of the resolver/injector core.

### 3.1 A new `agent:model` capability

Add `agent:model` to the canonical capability registry (`internal/capability`). It
represents "authenticate the agentic harness's model backend" and is **harness-neutral** —
the *env-var* it maps to is an adapter concern (below), exactly as `repo:push` is a
capability while `GH_TOKEN` is the copilot adapter's chosen env var for it.

Crucially, `agent:model` is **not** added to the auto-granted `credentialedCapabilities`
set — it must be sourced explicitly, so it never silently defaults to the repo token.

### 3.2 A `credentials:` block in `instance.yaml` (per-capability sourcing)

Give the instance config a way to source a capability from its own token ref:

```yaml
repos:
  - owner: Agent-Clubhouse
    name: Goobers
    token: { file: /path/to/org-repo-token }     # backs repo:push / issues / pr (unchanged)

credentials:                                       # NEW — per-capability token overrides
  - capability: agent:model
    token: { file: /path/to/copilot-requests-token }
```

Semantics: each `credentials[]` entry registers an additional **uniquely-named** `TokenRef`
and contributes a `Grant{Capability, Ref}` pointing the named capability at it. An entry
whose capability is already default-granted to the repo token **replaces** that default
grant — `buildCredentials` must **dedupe the grant set by capability** (the explicit entry
wins), *not* append a second grant for the same capability. This is load-bearing:
`credentials.NewInjector` **rejects duplicate grants for the same capability**
(`internal/credentials/capability.go`: `"duplicate grant for capability %q"`), and
`NewResolver` likewise rejects duplicate ref *names* (`source.go`) — so the wiring must build
at most one grant per capability and one ref per name, or it fails closed at construction
rather than overriding. `agent:model` never collides (it has no default grant); the
`repo:push` override in AC3 is the case that exercises the dedupe. This generalizes:
`repo:push`, `github:issues:write`, etc. can each be re-sourced the same way, with no further
wiring (the strategic endpoint in §5).

`buildCredentials` grows a loop over `cfg.Credentials` that merges into the default grant
set by capability (replace-on-conflict); validation rejects an entry naming an unknown
capability or a missing token ref at config-load (fail-closed). Note: `instance.yaml` is
Go-validated only (no JSON schema), so the new `credentials:` field carries no schema-parity
obligation (cf. #125/#273).

### 3.3 Copilot adapter env mapping — two tokens, two env vars, one subprocess

The copilot adapter's `EnvCapabilities` gains `agent:model → COPILOT_GITHUB_TOKEN`,
alongside the existing `repo:push`/`github:*` → `GH_TOKEN`. This is safe because the Copilot
CLI resolves its **model-auth** token with a defined precedence (from `copilot login --help`):

> `COPILOT_GITHUB_TOKEN` → `GH_TOKEN` → `GITHUB_TOKEN`

while its `github` tool reads the conventional `GH_TOKEN`/`GITHUB_TOKEN` for repo API calls.
So a single agentic subprocess can carry **both** without collision:

| Env var | Token | Consumer |
|---|---|---|
| `COPILOT_GITHUB_TOKEN` | personal PAT, **Copilot Requests** only | Copilot model auth (wins by precedence) |
| `GH_TOKEN` | org-repo PAT (Issues/PRs/Contents) | the `github` tool's repo API calls |

A GitHub-mutating agentic goober (e.g. the curator, which labels/comments/closes issues)
declares **both** `agent:model` (personal token → `COPILOT_GITHUB_TOKEN`) and its existing
`github:issues:write` (org token → `GH_TOKEN`). A purely code-editing agentic goober (e.g.
the implementer, which commits locally; push is a separate deterministic stage) declares
only `agent:model`. Fail-closed is preserved end to end: an undeclared `agent:model` means
`COPILOT_GITHUB_TOKEN` is never materialized; both tokens are registered with the secret
scrubber.

## 4. Spec / acceptance criteria

- **AC1 — two tokens coexist.** With `repo:push` sourced from token A and `agent:model` from
  token B, an agentic stage declaring both sees **two different values** in `GH_TOKEN`
  (=A) and `COPILOT_GITHUB_TOKEN` (=B) in one subprocess env.
- **AC2 — fail-closed.** An agentic stage that does **not** declare `agent:model` has no
  `COPILOT_GITHUB_TOKEN` in its env. A `credentials:` entry naming an unknown capability, or
  a ref that resolves to nothing, is rejected at config-load.
- **AC3 — override (dedupe-by-capability).** A `credentials:` entry for `repo:push`
  **replaces** the default repo-token grant for that capability — the resulting grant set
  has exactly one `repo:push` grant (pointing at the override ref), so `NewInjector` accepts
  it rather than erroring on a duplicate grant. Proves the mechanism generalizes beyond
  `agent:model`.
- **AC4 — redaction.** Both token values are registered with the scrubber and are redacted
  from journal/telemetry/log output.
- **AC5 — docs.** `docs/guides/github-token-scopes.md` gains an `agent:model` row noting it
  requires a **personal-account** fine-grained PAT with Copilot Requests, that the copilot
  adapter reads it from `COPILOT_GITHUB_TOKEN`, and that in the common cross-org case it is a
  **different token** from the repo credential.
- **AC6 — live acceptance (in #30, not the impl PR).** With both tokens wired, the `curate`
  stage authenticates to Copilot **and** still mutates issues via the org token.

### Test plan (unit/integration — the impl PR)

- Config-load: valid `credentials:` parses; unknown capability rejected; missing ref
  rejected.
- `buildCredentials`: grants map each capability to the right ref; override replaces the
  default; `agent:model` present only when configured.
- Injector: subprocess env for a two-capability agentic stage contains both env vars with
  the correct distinct values; single-capability stage contains only its own; undeclared →
  absent (fail-closed); both values scrubbed.

## 5. Forward path — toward per-stage / per-goober isolated tokens

This design is the **first, config-visible slice** of the broader "each stage/goober gets its
own isolated credential(s)" goal:

- **Now (V0.2):** per-capability token *sourcing* — the instance can hold N tokens and route
  each capability to one. Unblocks the cross-org Copilot case.
- **Next (V1, epic #35 "S1"):** scope the resolved grants to a **goober identity**, so goober
  A cannot resolve goober B's credential even within one instance. Builds directly on the
  `credentials:` surface here — no config redesign.
- **Later (V1+):** per-*stage* grants and multiple tokens per stage as workflows need them
  (the general `{stage → [capability → ref]}` mapping), plus the secret-resolver seam (#38)
  that lets refs come from Key Vault / managed identity instead of files.

No V2 scope (Temporal, cloud identity) is pulled in here.

## 6. Risks & fallbacks

- **R1 (primary): does the copilot `github` tool honor `GH_TOKEN`, or does it also prefer
  `COPILOT_GITHUB_TOKEN`?** If the bundled github MCP server prefers the Copilot-only token,
  the curator's org issue writes would fail. This is an empirical question the #30 live re-run
  resolves cheaply. **Fallbacks, in order:** (a) pin the github MCP server's token explicitly
  via `~/.copilot/mcp-config.json`, decoupling it from env precedence entirely; (b) move the
  curator's issue mutations into a following **deterministic** stage (agent decides →
  deterministic acts, mirroring how `query-backlog` already claims deterministically),
  leaving the agent with only `agent:model`.
- **R2: org PAT approval.** A personal fine-grained PAT can only touch org repos if the org's
  PAT policy allows and (by default) an org owner approves that specific token. This is a
  human/org-admin step, orthogonal to the code, and only relevant to whichever token backs the
  **repo** capabilities — `agent:model`'s token needs no repo access at all.
- **R3: classic PATs unsupported.** The Copilot CLI rejects classic `ghp_` tokens; the
  `agent:model` token must be a v2 fine-grained PAT. Note in the guide.

## 7. Open questions (for PO / PM)

- **OQ-1:** Confirm the `credentials:` block (capability → token ref, override semantics) as
  the config surface — vs. a narrower one-off `copilotToken:` field. *(Recommend: the general
  `credentials:` block; it's barely more code and is the substrate #35/S1 needs anyway.)*
- **OQ-2:** Name the capability `agent:model` (harness-neutral) vs. `copilot:request`
  (mirrors GitHub's permission name). *(Recommend: `agent:model`; the harness→env-var mapping
  stays adapter-specific, so a future non-Copilot harness reuses the capability.)*
- **OQ-3:** Is per-capability sourcing sufficient for V0.2, with per-**goober** identity
  scoping deferred to #35/S1? *(Recommend: yes — this unblocks #30; identity scoping is a
  clean follow-on.)*
