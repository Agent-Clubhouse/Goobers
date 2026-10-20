# Pre-lock reference migration corpus

These are the 16 DSL 2.0 reference workflows shipped at commit `605c88f0c`,
before the reference tree moved to DSL 3.0. Keep them as historical inputs to
the mechanical migrator and transition-graph conformance tests; do not synchronize
them with current 3.x workflows. The already-3.1 `quality-spot-check` is excluded.

Current shipped workflow correctness is tested against `reference-workflows/`
by `internal/workflow` and `test/shippedworkflows`. Preserving this older corpus
keeps migration coverage active after no shipped reference pins 2.0.
