# Frozen DSL 1.4 interpreter

`v_current` is frozen now that `v_next` exists. Changes here may only be
contract-preserving interpreter patches; new features, changed defaults, and
other author-visible semantics belong in a copied-forward interpreter. This is
the forward-only maintenance discipline from `docs/design/dsl-version-lifecycle.md`
§3.5 and DVL-9.

The fixtures under `testdata/golden` are the compatibility guard for DSL 1.4.
Their machine and semantic digests must remain unchanged unless a
contract-preserving patch intentionally changes the compiled representation.
