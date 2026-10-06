# Pilot import boundaries

`architectureimports` enforces the reviewed library-first direction for the
`internal/docchurn`, `internal/contention`, and `internal/prstatus` pilots. It
asks `go list` for each accepted namespace's production and test graph in every
configured build context, including every package below the namespace; it does
not infer imports by searching source text.

Run the checked-in rules from the repository root:

```text
go run ./test/architectureimports -config test/architectureimports/rules.json
```

All three lanes have accepted extraction decisions and are checked by the
required preflight CI group. A missing namespace or any package-discovery error
fails closed. A declined lane must name the maintainer who made that decision
and remains visible in the configuration. If every configured lane is declined,
the tool reports real-package checking as inapplicable while its synthetic
negative tests continue to run.

## Integration coverage evidence

A fresh, uncached atomic coverage run on the integrated tree was recorded on
2026-10-06 with:

```text
go test -count=1 -covermode=atomic ./internal/docchurn ./internal/contention ./internal/prstatus
```

| Accepted library | Statement coverage |
|---|---:|
| `internal/docchurn` | 89.7% |
| `internal/contention` | 100.0% |
| `internal/prstatus` | 85.0% |

These separate pilot measurements are evidence only. The required whole-tree
coverage job continues to own the aggregate 70% threshold and its existing
exclusions; no per-package floor is introduced here.

## Reviewed direction

The executable remains the composition root. Pilot libraries cannot import
`cmd/goobers`, daemon/runner construction packages, or one another. These
restrictions apply transitively and to internal and external tests.

- `docchurn` has the narrow durability/configuration closure needed for its
  watermark behavior. Git process execution, flags, environment/default
  resolution, and output/exit mapping remain adapter concerns.
- `contention` and `prstatus` may directly use `providers`. Their checked-in
  transitive allowlist freezes the reviewed repository-local provider closure;
  a new provider dependency is therefore a reviewed rules change, not an
  automatic expansion of both domains.
- Standard-library and third-party dependencies are governed by their normal
  module and package reviews. This check controls repository-local direction.

Every exception is an exact package prefix with a rationale. To change a
legitimate dependency, update the affected lane's direct and transitive lists
in the same PR as the dependency, explain why the domain owns that dependency,
and have the boundary reviewed. Do not add a broad module-level prefix, skip a
build context, or suppress package-discovery errors.
