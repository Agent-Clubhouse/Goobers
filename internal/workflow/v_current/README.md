# Frozen DSL 1.4 interpreter

`v_current` is frozen now that `v_next` exists. Changes here may only be
contract-preserving interpreter patches; new features, changed defaults, and
other author-visible semantics belong in a copied-forward interpreter. This is
the forward-only maintenance discipline from `docs/design/dsl-version-lifecycle.md`
§3.5 and DVL-9.

The fixtures under `testdata/golden` are the compatibility guard for DSL 1.4.
Their machine and semantic digests must remain unchanged unless a
contract-preserving patch intentionally changes the compiled representation.

A deliberate digest change is enforced, not just documented:
`testdata/golden/PATCH_LOG.json` pins the current `digests.json` content by
sha256 and carries an append-only log of every reviewed patch. Changing
`digests.json` without bumping that pin and appending a describing
`patches[]` entry fails `TestFrozenGoldenChangeRequiresAcknowledgedPatch` —
there is no path to a green frozen-golden change that skips recording why.

## Breaking-rejections wave (pre-v0.2.0, PO-ratified)

One deliberate, PO-ratified contract change was applied to this frozen
interpreter as part of the 1.4 lock's breaking-rejections wave (silent drops
become loud BEFORE the v0.2.0 tags): constructs this version never
implemented are now compile ERRORS instead of silently dropping
(`unknownVersionConstructProblems` in `compile.go`):

- a `parallels:` block (#2738) — it compiled clean but the machine built no
  parallel states, so the block silently never executed;
- `task.workspace` — this version's runner contract reads only
  `run.workspace`, so an author asking for `repo-readonly` silently got the
  WRITABLE repo default.

Both follow `runScriptProblems`' existing rejection pattern ("… is not part
of DSL 1.4; available in 2.0"). The three golden fixtures set none of these
fields, so the compiled digests are unchanged — this narrows what 1.4
ACCEPTS, it does not change what any accepted 1.4 workflow MEANS.
