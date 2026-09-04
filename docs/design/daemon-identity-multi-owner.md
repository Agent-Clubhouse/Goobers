# Daemon identity on multi-owner instances

**Status:** implemented — the routing this document designs shipped in #3414 and
#3415; `internal/instance/config.go`'s `DaemonIdentityConfig` carries the
per-owner-installation fields described below. Retained as the design of record
for #3341; the code, not this document, is authoritative for current behaviour.
**Verified against** `origin/main` @ `a1cd66dd`.

`daemonIdentity` is per-instance global and carries exactly one `installationId`. GitHub App
installations are owner-scoped. An instance whose `repos` span two GitHub owners therefore has **no
configuration of `daemonIdentity` that works**: every daemon mutation against the other owner's repo
dies at credential materialization with GitHub's 422 ("at least one repository … is not accessible to
the parent installation"), because the mint is correctly down-scoped to a repo the configured
installation does not cover.

Found live on a tier-3 cloud instance (v0.2.1, 2026-08-20 cutover) whose repos are
`Agent-Clubhouse/Goobers` (installation A) and `masra91/Goobers-Site` (installation B) — same App,
same bot login, different minting scopes. The operational workaround (remove `daemonIdentity`
entirely, fall back to per-repo tokens) works but degrades PR attribution to the branch-prefix
heuristic; §6 explains why that degradation is the expensive, drift-shaped kind.

**What this is not.** It is not #1779 (provisioning an App for the self-hosted instance), not #3343
(plumbing the declared bot login through `AuthenticatedLogin` — landed), and not a change to the
workflow DSL: `daemonIdentity` is `instance.yaml` provisioning surface, so everything here is
lifecycle-safe at any DSL version (operator ruling on #3341, 2026-08-20).

---

## 1. Decisions

| # | Decision | Why |
| --- | --- | --- |
| D1 | `daemonIdentity` (kind `github-app`) gains an **`installations:` list — per-owner installation bindings**: one App, one private key, one slug, N `{owner, installationId}` entries | The missing expressiveness is routing, not identity. Installations of one App share the App's `<slug>[bot]` login; only the minting scope is owner-shaped. This mirrors what `repos[].auth` already models correctly per repo |
| D2 | **Exactly one of** top-level `installationId` **or** `installations:` — never both | A "default plus overrides" hybrid invites silent drift: an owner missing from the list would fall back to an installation that provably cannot serve it. Fail-closed beats fallback here because the fallback is always wrong |
| D3 | Routing happens at credential-build time, **per gaggle, keyed by the gaggle repo's owner** | `buildCredentials` already composes one resolver per gaggle with `(gaggleOwner, gaggleName)` in hand, and `newDaemonIdentityGitHubAppTokenSource` already down-scopes each mint to that gaggle's repo (MGV-5 discipline). Selecting the installation whose `owner` matches `gaggleOwner` is the entire runtime change; the down-scoping, ref name (`daemonIdentityRefName`), capability set (`daemonIdentityCapabilities`), and last-wins precedence against explicit `credentials:` grants are all untouched |
| D4 | **Fail at config load, not at first run**: a `daemonIdentity` that cannot cover every configured GitHub repo is rejected by `validate` (§5) | The live instance validated clean and detonated on a scheduled run's first cross-owner mint. An installation belongs to exactly one owner, so "single `installationId` + repos spanning >1 GitHub owner" is statically impossible — same class as #3333 (schema-valid, runtime-fatal, statically detectable). Operator ruling: rejection only hits configs that are already runtime-fatal — strictly-better enforcement, no working config affected, 2.0-safe. This is deliberately an **error**, not a warning, under that explicit ruling |
| D5 | Bindings are **explicit**, not inferred from `repos[].auth` | When `repos[].auth` uses the same App, the owner→installation map is already in the config — but repos may use PAT auth, ADO, or a different App, and an inference that sometimes applies is a debugging trap. Instead, validation **cross-checks**: if a repo's `auth` is `github-app` with the same `appId` as `daemonIdentity`, its `installationId` must agree with the binding for that repo's owner (§5). Declared surface stays the single source of truth; disagreement is caught, not papered over |
| D6 | **One App per daemonIdentity.** Per-owner *different Apps* (different logins per owner) are out of scope | A second App is a second identity, and every attribution surface (§6) assumes one `expectedAuthorLogin` per instance (`isOwnPullRequest`, `daemonIdentityAuthorLogin`). Multi-login attribution is a much larger design; nothing observed needs it — the live incident was one App throughout |
| D7 | Per-repo scoping on `credentials:` grants (`capability` × `repo` → token) is **rejected** as the vehicle for this fix | It approximates identity instead of preserving it: N×M config surface, and the consumer-side check degrades from an identity comparison to bookkeeping. Merge-review's owner explicitly ranked the routed-identity form above it on #3341. `RepoScopedCapability` already exists for read-only reference-repo grants and stays as-is |
| D8 | `kind: pat` is untouched | PATs are not owner-scoped; a machine-account PAT already works on a multi-owner instance. The PAT/App co-equality ruling (2026-07-27, unattended-operation §3) stands |

---

## 2. Current mechanics (what the design builds on)

All symbol references are to `origin/main` @ `a1cd66dd`. Cited by symbol, not line.

- **Config:** `instance.DaemonIdentityConfig` (`internal/instance/config.go`) carries `Kind`
  (`pat` | `github-app`), and for `github-app`: `AppID`, a single `InstallationID`, `PrivateKey`,
  `Slug`. `DaemonIdentityConfig.validate` enforces exactly-one-kind and kind-specific required
  fields, called from `Config.validateDaemonIdentity`.
- **Wiring:** `buildCredentials` (`cmd/goobers/runnerwiring_credentials.go`) is per-gaggle — it
  receives `(gaggleOwner, gaggleName)`. When `DaemonIdentity` is configured it registers one
  resolver ref (`daemonIdentityRefName = "daemon-identity"`) and grants it the standard
  daemon-mutation set (`daemonIdentityCapabilities` in `runnerwiring_executors.go`: `repo:push`,
  `github:issues:write`, `github:pr:write`, `github:pr:review`, `github:branch:delete`,
  `github:pr:merge`), before the explicit `credentials:` loop so per-capability grants still win
  (`credentials.RunnerGrants` last-wins semantics).
- **Minting:** `newDaemonIdentityGitHubAppTokenSource` builds a `githubapp.New` source from the
  single `InstallationID`, down-scoped via `Repositories` to the gaggle's own repo — the MGV-5
  discipline that (correctly) turns the owner mismatch into GitHub's 422.
- **Per-repo auth that already works:** `RepoAuthConfig` on `repos[]` carries its own
  `InstallationID` per entry; `RepoAuthConfig.BotLogin` derives `<slug>[bot]`; #3343 plumbed that
  login through `providers.WithConfiguredLogin` so `AuthenticatedLogin` never needs `GET /user`
  under App auth.
- **Attribution:** `isOwnPullRequest` (`cmd/goobers/prselect.go`) compares the PR author against
  `expectedAuthorLogin` when a daemon identity is configured and its login is resolvable
  (`daemonIdentityAuthorLogin`: App kind requires `Slug`), otherwise falls back to the
  branch-prefix heuristic against the gaggle's configured head prefixes.

The load-bearing observation: **everything downstream of the mint is already per-gaggle and
owner-agnostic.** The only global piece is which installation signs the mint.

---

## 3. Config surface

```yaml
daemonIdentity:
  kind: github-app
  appId: 123456
  privateKey: { file: /secrets/goobersbot.pem }
  slug: goobersbot
  installations:                      # NEW — owner-scoped installation routing
    - owner: Agent-Clubhouse
      installationId: 1111111
    - owner: masra91
      installationId: 2222222
```

- `installations[].owner` — the GitHub owner (org or user) exactly as it appears in
  `repos[].owner`. Compared case-insensitively (GitHub owner names are case-insensitive).
- `installations[].installationId` — `GitHubID`, numeric, same validation as the existing
  top-level field.
- The existing single-installation form remains fully supported for single-owner instances:

```yaml
daemonIdentity:
  kind: github-app
  appId: 123456
  installationId: 1111111
  privateKey: { file: /secrets/goobersbot.pem }
  slug: goobersbot
```

`appId`, `privateKey`, and `slug` stay top-level and singular — one App, one key, one login (D6).
The field is additive and optional; `sigs.k8s.io/yaml` round-trips absent fields unchanged, so an
unconfigured instance's written `instance.yaml` stays byte-identical.

`kind: pat` rejects `installations` exactly as it rejects the other App-only fields today
(`hasGitHubAppFields` gains the new field).

---

## 4. Runtime routing

`buildCredentials` selects the binding whose `owner` matches `gaggleOwner` (case-insensitive) and
hands that `installationId` to `newDaemonIdentityGitHubAppTokenSource`, which needs no other change
— it already receives the gaggle repo name for down-scoping and would now take the resolved
installation ID (or the owner, resolving internally) instead of reading the single global field.

Properties preserved by construction:

- **Down-scoping:** every mint remains scoped to exactly the gaggle's own repo. A shared
  installation still never hands one gaggle a token reaching a sibling gaggle's repo (MGV-5).
- **Precedence:** explicit `credentials:` grants still override the daemon-identity grant per
  capability — the routing change is upstream of `RunnerGrants` and invisible to it.
- **Ref naming:** still one `daemon-identity` ref per gaggle-scoped resolver. Since each gaggle's
  resolver is built independently, the same ref name binds to different installations in different
  gaggles with no namespace change and no cross-gaggle state.
- **No selection at mutation time:** daemon mutations target the gaggle's own repo (additional
  repos are read-only by construction — `AdditionalReadGrants` emits no write capability), so
  owner-at-credential-build-time is exactly as precise as owner-at-mint-time.

An owner with no matching binding cannot occur at runtime: §5 rejects the config at load.

---

## 5. Validation — fail at config load

All checks live in `DaemonIdentityConfig.validate` / `Config.validateDaemonIdentity`, which already
run on every load path (daemon startup, `goobers validate`, one-shot commands). Per D4 and the
operator ruling on #3341, the coverage checks are **errors**, not warnings: every rejected config is
already runtime-fatal today, so no working instance is affected.

Structural (new field, both forms):

1. Exactly one of `installationId` / `installations` for kind `github-app`; `installations`
   non-empty when present; each entry requires non-empty `owner` and a numeric `installationId`;
   duplicate owners (case-insensitive) rejected.
2. `kind: pat` + `installations` rejected (App-only field, existing pattern).

Coverage (the #3341 detonation, made static):

3. **Single-installation form:** if the instance's GitHub-provider repos span more than one
   distinct `owner`, reject — an installation belongs to exactly one owner, so at most one of
   those owners can ever mint. This needs nothing but `repos[].owner`; it is the check that would
   have caught the live config the moment it was written. Message names the uncovered owners and
   points at `installations:`.
4. **`installations` form:** every distinct GitHub-provider `repos[].owner` must have a binding.
   Extra bindings for owners with no configured repo are rejected too — an unused binding is
   either a typo'd owner (the dangerous case: the real owner then also trips check 4's missing
   arm) or dead config.
5. **Cross-check against `repos[].auth` (D5):** for any repo whose `auth` is `github-app` with
   `appId` equal to `daemonIdentity.appId`, the repo's `installationId` must equal the daemon
   identity's binding for that repo's owner (or the single `installationId` on a single-owner
   instance). Two installation IDs for one (App, owner) pair cannot both be right — GitHub allows
   one installation per owner per App — so disagreement is a config error somewhere and is said
   out loud rather than discovered by whichever half fails first.

Non-checks, deliberately: PAT-kind identities (not owner-scoped); repos on other providers (ADO has
no App concept — `daemonIdentity` is GitHub-only surface today and its docs already say so);
whether the declared installation *actually* exists or covers the repo server-side (config-load
validation stays offline; the runtime 422 remains the backstop for server-side drift, now reachable
only through genuine server-side changes rather than expressible-but-wrong config).

Check 3 is independently shippable before the `installations:` field exists and is worth landing
first — it converts the remaining silent-failure window into a config error with a clear message,
even for operators who stay on the workaround (follow-up F1, §8).

---

## 6. Attribution surfaces: self-approval, self-PR classification, label/comment trust

The interaction question raised on #3341: does per-owner routing change any identity-keyed
behavior? The invariant that answers it:

> **Installation choice never changes the acting login.** All installations of one App
> authenticate as the same `<slug>[bot]` account. This design routes *minting scope* only; every
> surface keyed on *who acted* is invariant under it.

Concretely, per surface:

**Self-PR classification (merge-review's "is this ours" gate).** `isOwnPullRequest` compares
`pr.Author` against one `expectedAuthorLogin` resolved by `daemonIdentityAuthorLogin` from `Slug`.
One App ⇒ one slug ⇒ one expected login across all owners; the check works identically on every
repo of the instance, and D6 exists precisely to keep that true. This is also the strongest
argument for the design over the current workaround: with `daemonIdentity` removed, classification
falls back to the branch-prefix heuristic, which couples to `branchNamespace` — a **supported,
gaggle-configurable knob**. An adopter renames their branch namespace, the heuristic stops
matching, and merge-review silently treats the instance's own PRs as foreign: `advisoryMode`
flips on (`prselect`), verdicts are published as non-blocking comments only, no remediation labels
are applied, nothing merges, nothing errors. Drift-shaped, not crash-shaped. Restoring a working
`daemonIdentity` on multi-owner instances closes that window; that is the attribution cost the
workaround has been paying since 2026-08-20.

**Self-approval.** GitHub categorically refuses a native review by the PR's own author
(`providers.IsSelfReviewError`; `apply-verdict` soft-skips to the comment/label verdict handoff,
and GitHub would not count a self-approval toward branch protection anyway). Because installations
share the App's login, this refusal fires — or doesn't — identically whichever installation minted
the token. Per-owner bindings therefore neither create nor remove any self-approval case: an
instance whose repos' `auth` uses the same App as `daemonIdentity` still authors and reviews as one
account and still degrades to the handoff, exactly as documented on #870. A *distinct* reviewing
identity still requires a distinct App or PAT (via `credentials:` override for
`github:pr:review`) — orthogonal to this design and unchanged by it.

**Label and trusted-comment machinery.** Claim markers, verdict authorship, handoffs, and
merge-review status-comment reconciliation compare authorship against the configured bot login
(#3343, `providers.WithConfiguredLogin` / `RepoAuthConfig.BotLogin`). Labels merge-review applies
(remediation labels, `run-aborted`, opt-out labels) and the verdict-label/comment handoff that
`merge-pr` reads are written and later trusted under that same login. Same login on every owner ⇒
every trust comparison behaves identically on both sides of the owner boundary. The one
sharp edge is inherited from #3343 rather than created here: for kind `github-app`, **`Slug`
is what makes all of the above an identity check** — without it, attribution silently falls back
to the heuristic (`daemonIdentityAuthorLogin` returns empty). With multi-owner routing making App
daemon identities viable on these instances, an App identity without `Slug` stops being a
forward-compat curiosity and becomes a live misconfiguration; validation should warn on it
(warning, not error: it mints and authenticates correctly today, and existing configs must keep
validating). Folded into F2.

---

## 7. Migration from a single `daemonIdentity`

- **Single-owner instances: no action.** The single-`installationId` form stays valid
  indefinitely; check 3 (§5) passes trivially with one owner. This is not a deprecation.
- **Multi-owner instances currently running the workaround** (no `daemonIdentity`): re-add it in
  the `installations:` form. Attribution reverts from heuristic to identity on the next daemon
  reload; no state, journal, or label migration exists because nothing durable encodes the
  installation ID — tokens are minted per run and discarded.
- **Multi-owner instances with a single-installation config** (the broken-by-construction shape):
  currently failing at first cross-owner mint; after F1 they fail at config load with a message
  naming the fix. The rewrite is mechanical:
  `installationId: X` → `installations: [{owner: <sole-covered-owner>, installationId: X}]` plus
  one entry per additional owner.
- **Schema/docs:** `api/schemas/` instance schema and `docs/guides/github-token-scopes.md` /
  `docs/guides/arbitrary-repo-onboarding.md` gain the new field alongside the existing one
  (part of F2; regenerated collateral rides the implementing PR per repo convention).

Rollback is symmetric: the `installations:` form degrades to removing `daemonIdentity` (today's
workaround) at any time, since the field is provisioning-only.

---

## 8. Follow-up issues

Filed alongside this document, gated as listed. This PR implements none of them.

| # | Scope | Gate |
| --- | --- | --- |
| #3414 (F1) | Fail-at-config-load coverage checks for the **existing** single-installation form (§5 checks 3 and 5, single-owner arm): multi-owner repos + single `installationId` → load error; same-App `repos[].auth` disagreement → load error. No new config surface; independently shippable and worth landing first | This design merged |
| #3415 (F2) | `installations:` per-owner bindings: config field + structural validation (§5 checks 1–2, 4, and the `installations`-form arm of 5), routing in `buildCredentials` / `newDaemonIdentityGitHubAppTokenSource` (§4), missing-`Slug` warning (§6), schema + guide updates (§7). Regression test shape: two-owner instance config, assert per-gaggle resolvers mint from the owner-matched installation and that the §5 rejections fire | #3414 (its checks become the `installations`-form arms) |
| — | Per-owner *different Apps* (multi-login attribution) — **not filed**: no observed need, and it invalidates the single-`expectedAuthorLogin` model (D6). File only if a real instance cannot install one App on every owner it targets | — |

---

## 9. Open questions

| # | Question |
| --- | --- |
| Q1 | Should check 5 (§5, same-App cross-check) be an error or a warning? This doc says error — two IDs for one (App, owner) cannot both be right, so one half of the config is already broken — but unlike checks 3/4 the broken half might be `repos[].auth` rather than `daemonIdentity`, and the error message must not presume which. Ruling folded into F1's review |
| Q2 | Is rejecting *extra* bindings (§5 check 4) too strict for a config shared across instances with different repo subsets? The fail-closed argument (typo'd owner) currently wins; relax to a warning if the shared-config pattern materializes |
