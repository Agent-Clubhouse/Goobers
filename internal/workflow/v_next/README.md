# Frozen DSL 2.0 interpreter

`v_next` is the DSL 2.0 interpreter, and it is frozen: locked as of `v0.2.0`
(#3279) and superseded by `v_3_0`. Changes here may only be
contract-preserving interpreter patches; new features, changed defaults, and
other author-visible semantics belong in a copied-forward interpreter. This is
the forward-only maintenance discipline from
`docs/design/dsl-version-lifecycle.md` §3.5 and DVL-9.

The fixtures under `testdata/golden` are the compatibility guard for DSL 2.0.
Their machine and semantic digests must remain unchanged unless a
contract-preserving patch intentionally changes the compiled representation.

A deliberate digest change is enforced, not just documented:
`testdata/golden/PATCH_LOG.json` pins the current `digests.json` content by
sha256 and carries an append-only log of every reviewed patch. Changing
`digests.json` without bumping that pin and appending a describing
`patches[]` entry fails `TestFrozenGoldenChangeRequiresAcknowledgedPatch` —
there is no path to a green frozen-golden change that skips recording why.

## Retirement trigger

A frozen interpreter package is deleted in exactly one situation: **the
release in which its DSL version transitions to `LevelUnsupported` in
`internal/supportmatrix`.** Nothing earlier qualifies. While the version is
`supported` or `deprecated` a pinned document must still load and run
as-authored (`docs/design/dsl-version-lifecycle.md` §3.3), and the support
window — ≥3 minor releases of loadability, ≥1 released minor spent
`deprecated`, both checked by `ValidateSupportPolicy` — is a promise about
this package's continued existence (§3.3.1, §3.4). Deleting the interpreter
*is* what makes `DVL030` ("the interpreter has been removed") the honest load
result, so the deletion and the matrix transition ship together or the
lifecycle surface lies in one direction or the other.

The precedent is `v_current` (DSL 1.4), deleted under #3507: 1.4 had been
`deprecated` since `v0.1.0` with a published `unsupportedAfter` of `v0.5.0`,
so the release that transitioned it to `LevelUnsupported` at `v0.5.0` is the
release that removed the package. That transition and its append-only history
live in `internal/supportmatrix/supportmatrix.go`; read them there rather than
re-deriving the date. For `v_next` the same rule applies, anchored on DSL
2.0's own eventual transition — 2.0 is `supported` today, so this package is
not a deletion candidate yet.

## What outlives the interpreter

Deleting an interpreter does not delete the way off it:

- **The migration edge stays.** `internal/dslmigrate`'s `edge_1_4_to_2_0.go`
  survived `v_current`'s deletion, because `DVL030` names
  `goobers fix --to 2.0` as the recovery path — an author holding an
  unloadable 1.4 document still needs the mechanical rewrite. The migrator
  edge for a retired version is therefore retained, not removed with its
  interpreter; the same holds for `edge_2_0_to_3_0.go` when 2.0 retires.
- **Migrate-FROM fixtures stay.** A fixture pinned to the retired version as
  the *input* of a migration or upgrade test (for example
  `test/workflowupgrade/one-version.before.yaml`) is deliberate and must not
  be bumped forward; bumping it deletes the test's subject.

## Dead-code exemptions

`test/deadcode` rejects stale entries, so removing this package must remove
its entries from `test/deadcode/exemptions.txt` **in the same change** — a
leftover exemption naming a deleted symbol fails `make deadcode` just as
loudly as an unreviewed unreachable function. The entries scoped to this
package today are `dslVersionLevel`, `CheckGaggleFeatureSupport`,
`LookupFeature`, `newFeatureRegistryAgainstReleased`, and
`validateFeatureRegistryEvolution`; `v_current`'s deletion dropped its four
counterparts the same way.
