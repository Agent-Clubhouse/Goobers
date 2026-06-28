# QA Review Checklist (M5)

The bar every PR must clear before QA posts **APPROVE**. Applied by Goobers-QA-1 to
M1 (Config/`/api`) and M2 (Providers) PRs; reusable for any mission PR.

A PR gets **CHANGES** if *any* MUST item fails. QA decisions are binary.

## 1. CI is green (MUST)

**Interim gate (effective now — no automated runner yet; PM ratified 2026-06-28):**
QA-reproduced `make ci` green on the PR's **latest/rebased** commit. Author
self-attestation is NOT sufficient — the QA reviewer independently re-runs it and posts
the result + commit SHA in the PR thread.
- [ ] QA personally ran `make ci` on the PR head commit (record the SHA in the verdict).
- [ ] `make ci` green end-to-end: `fmt-check` · `go vet` · `build` (all binaries) ·
      `go test -race` · golangci-lint v2 → **0 issues**.
- [ ] No skipped/`t.Skip` tests hiding failures.
- [ ] Coverage gate green — total coverage ≥ threshold (see `/test/README.md`).
      *(Coverage gate is a QA `make cover-check` extension stacking on the merged skeleton;
      until wired, QA reads coverage from `make cover` and applies the threshold manually.)*
- [ ] No CI/Make step disabled, `continue-on-error`, or commented out to get green.

> **When real CI lands** (ADO pipeline + self-hosted pool, fast-follow): this reverts to
> "required green CI status on the latest commit," and manual `make ci` attestation drops.

## 2. Tests present for new behavior (MUST — block on missing)
- [ ] Every new exported function / code path has a corresponding test.
- [ ] New behavior described in the PR has a test that would fail without the change.
- [ ] Error paths and edge cases are exercised, not just the happy path.
- [ ] Table-driven tests cover boundary + invalid inputs where applicable.
- [ ] For M1: good **and** bad config fixtures (validator must reject the bad ones).
- [ ] For M2: behavior asserted **identically** for GitHub + ADO (shared contract test).

## 3. Correctness vs. spec (MUST)
- [ ] Behavior matches the cited requirement IDs in `docs/requirements/*`.
- [ ] Shared contracts honored exactly: invocation envelope
      `{taskId,workflowId,runId,gaggle,item,goal,repoRef,upstreamOutputs,limits,inputs}`,
      result `{status,outputs,artifacts,summary,metrics,error?}`,
      verdict `{decision,findings[],summary}`.
- [ ] No contradiction of a VISION §8 "Decided" item (flag, don't silently diverge).
- [ ] Field names / types / required-ness match the canonical `/api` definitions
      (Dev-1 owns these; others import — no local redefinition).

## 4. Scope adherence (MUST)
- [ ] Changes are confined to the mission's directory; no edits to others' dirs
      (`/api`, `/providers`, `/infra`, `/portal`, `/test`, `/config-examples`).
- [ ] No unrelated refactors, renames, or drive-by changes bundled in.
- [ ] Diff matches what the mission brief asked for — nothing more, nothing less.

## 5. No regressions (MUST)
- [ ] Existing tests still pass; none deleted/weakened to make the PR green.
- [ ] No public behavior removed unintentionally.
- [ ] Coverage threshold not lowered to sneak the PR through.

## 6. Hygiene (SHOULD — note, don't necessarily block)
- [ ] No secrets, tokens, or real connection strings committed.
- [ ] No stray debug prints, `TODO` for the thing the PR was supposed to do, or commented-out code.
- [ ] Errors wrapped with context; no swallowed errors (`_ =` on a meaningful error).
- [ ] Public types/functions have doc comments.

---

## Verdict format (posted to `#mission-qa-harness` + ping `inbox-goobers-pm`)

```
QA <APPROVE|CHANGES>: <PR title / branch>
Spec: <requirement IDs verified>
CI: <green/red — link or job status on latest commit>
Findings:
  - [MUST]  <blocking issue, file:line, what to change>
  - [SHOULD]<non-blocking suggestion>
Coverage: <pct> (threshold <pct>)
```

**CHANGES** lists exactly what must change and why. **APPROVE** only when every MUST
item is satisfied and CI is green on the latest commit.
