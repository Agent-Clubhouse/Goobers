# Design: Versioning & Releases — DSL compatibility, tagged builds, feature matrix

> Status: **Draft for review — not implemented** · Area prefix: `VER` (new), `REL` (new) · Milestone: **Versioning & Releases** (#12)
> Requirements: [`docs/requirements/config-as-code.md`](../requirements/config-as-code.md) (CFG-Q5), [`docs/requirements/workflow.md`](../requirements/workflow.md) (WF-016)
> Architecture: [`docs/ARCHITECTURE.md`](../ARCHITECTURE.md)
> Related issues: #279 (`--version`), #33 (packaging/release binaries), #252 (one validation path), #150 (Goober.spec.model)

## 1. Why this exists

As the DSL grows, consumers outside this repo need two guarantees we do not offer today:

1. **Language stability** — a clear contract for what the DSL supports, what is in preview,
   what is deprecated (still works, warns) and what is removed (errors), and a rule for what
   constitutes a non-breaking vs breaking change. Without this, every DSL addition risks
   silently breaking existing config, and authors have no way to know what version supports what.
2. **Consumable releases** — tagged GitHub releases with changelogs and build artifacts so a
   consumer can *run the application without cloning and building the repo*, pinned to a known
   feature set.

This doc turns brain-dump items **1 (DSL versioning)** and **2 (release process)** into design
decisions and dispatchable issues, and specifies the **warning-surfacing plumbing** (item 8) that
both depend on.

## 2. Current state (grounded)

- Every config object carries `apiVersion: goobers.dev/v1alpha1`, enforced as a JSON-Schema
  **`const`** (`api/schemas/workflow.schema.json:10`, and identically in goober/gaggle/manifest
  schemas). Any other value is a hard error. There is **no** `dslVersion`/`schemaVersion`,
  feature-flag, or deprecation field anywhere (`api/v1alpha1/groupversion_info.go:21`).
- CFG-Q5 (`docs/requirements/config-as-code.md`) already flags "version compatibility between the
  platform release and the config definitions" as **unimplemented build-time design**.
- A per-workflow monotonic `Definition.Version int` exists (`internal/workflow/machine.go:43`) but
  is a *run-pinning* integer (WF-016), **not** a language version — and is hardcoded `Version: 1`
  today (`api/validate/validate.go:454`). Keep these two concepts strictly separate.
- The validator has a `Severity` (`Error`/`Warning`) + `Issue` model
  (`api/validate/validate.go:27`). Coded warnings are retained and surfaced by
  `goobers up` and `goobers status`; feature-registry and model-fallback warning
  producers remain tracked by #428 and #150.
- No release automation, changelog, or `--version` flag (#279). `cmd/` builds are `go build` only.

## 3. Design

### 3.1 DSL version model (VER)

Introduce an explicit **DSL feature-support level** decoupled from `apiVersion` (which stays the
Kubernetes-style *group/version* of the resource shape and only bumps on a full CRD revision).

- **Capability/feature registry**: a single in-code registry of named DSL features (e.g.
  `trigger.signal`, `gate.evaluator.human`, `task.retry.backoff`, `goober.spec.model`), each with a
  **support level**: `preview | ga | deprecated | removed`, plus the app version it entered each
  level. This is the source of truth for validation *and* the published feature matrix (§3.3).
- **Validation semantics** (extends `api/validate` + `internal/workflow/compile.go`):
  - `removed` feature used → **error** (fail closed), message names the feature and last-supporting version.
  - `deprecated` feature used → **warning** (still compiles/runs), message names the replacement + removal target.
  - `preview` feature used → **info/warning** (works, flagged unstable), gated behind the explicit
    instance Manifest annotation `goobers.dev/allow-preview-features: "true"` so preview usage is
    never accidental.
  - `ga` → silent.
- **Non-breaking vs breaking policy** (documented contract, enforced by CI test over the registry):
  - *Non-breaking* (allowed within an `apiVersion`): adding optional fields, adding enum values,
    adding new stage/gate kinds, relaxing constraints, promoting `preview→ga`.
  - *Breaking* (requires `deprecated→removed` cycle across ≥1 minor release, or an `apiVersion` bump):
    removing/renaming a field, tightening a constraint, changing a default, changing semantics.
  - A feature must live ≥1 released minor in `deprecated` before it may become `removed`.

### 3.2 Release process (REL)

- **`goobers --version`** (#279): embed version + git SHA + build date via `-ldflags`.
- **Tagged releases**: semver tags (`vMAJOR.MINOR.PATCH`) drive a GitHub Actions release workflow
  that builds multi-platform binaries (darwin/linux, arm64/amd64), generates a changelog from
  Conventional-Commit history + a curated release note, and attaches artifacts to the GitHub Release.
- **Consumable without cloning**: publish binaries (and optionally a container image + Homebrew tap —
  ties to #33) so a consumer runs `goobers` from a downloaded artifact.
- **Version ↔ DSL linkage**: each release records the DSL feature-registry snapshot it ships;
  the release notes include the feature-matrix delta (newly GA, newly deprecated, removed).

### 3.3 Feature-support matrix (VER/REL)

A generated, versioned matrix (feature × support-level × since-version), emitted from the §3.1
registry so it can never drift from what the binary actually enforces:

- `goobers features` CLI subcommand prints the matrix for the running binary.
- The release workflow renders the same data to `docs/` (e.g. `docs/feature-matrix.md`) and to the
  GitHub Release notes, showing current-version state and version history.

### 3.4 Warning-surfacing plumbing (item 8 — shared dependency)

The DSL-deprecation and model-fallback (#150/§7 of that work) warnings are worthless if the daemon
drops them. This milestone owns the CLI half of the plumbing; the dashboard half is consumed in
the Dashboard milestone (#14).

- Stop discarding the report at `cmd/goobers/up.go:83`; print warnings on `goobers up` startup.
- Surface active config warnings in `goobers status` (which today reads only run state — it must
  also re-run/read validation and show deprecation/preview/fallback notices).
- Give warnings a stable, machine-readable code namespace (e.g. `VER001` deprecated-feature,
  `MODEL002` model-fallback) so the dashboard and logs can render them uniformly.

The CLI warning codes are stable within their namespace:

| Code | Meaning |
|---|---|
| `VER001` | Deprecated DSL feature |
| `VER002` | Preview DSL feature |
| `VER003` | Compatibility notice |
| `VER004` | Removed DSL feature |
| `MODEL002` | Model fallback |

#### Compatibility registry

The compatibility registry also tracks accepted-but-inert fields. At V0,
`task.expectedOutputs` is **declared-not-enforced** and
`task.run.image` is not honored by the local runner; declaring either emits
`VER003` rather than failing validation. Enforcing `expectedOutputs` remains a
later contract change once shipped declarations are trustworthy. The code and
file/gaggle provenance are carried on the `--json`/API surface; `goobers
validate`'s human output prints these workflow compatibility notices as
`WARNING  <scope>: <explanation>`, without the code.

Human output from both commands is `WARNING <code> <scope>: <explanation>`.
`goobers status --json` emits one object with `warnings` and `runs` arrays:

```json
{
  "warnings": [
    {
      "code": "VER001",
      "severity": "warning",
      "scope": "gaggles/example/workflows/legacy.yaml Workflow/legacy",
      "explanation": "feature x is deprecated; use feature y"
    }
  ],
  "runs": []
}
```

Warnings are ordered by `scope`, then `code`, then `explanation`; runs retain
their existing chronological order. Warning-free configuration emits an empty
`warnings` array and no human warning lines.

## 4. Issue breakdown (milestone #12)

- **[EPIC]** Versioning & Releases.
- VER-1: DSL feature-support registry (feature → level → since-version) as single source of truth.
- VER-2: Validator/compiler enforcement — error on `removed`, warn on `deprecated`, opt-in for `preview`.
- VER-3: Non-breaking vs breaking-change policy doc + CI guard test over the registry.
- VER-4: `goobers features` + generated `docs/feature-matrix.md`.
- REL-1: `goobers --version` with ldflags build metadata (folds #279).
- REL-2: Tagged-release GitHub Actions workflow → multi-platform artifacts + changelog (coordinates with #33).
- REL-3: Release notes include feature-matrix delta; document the support policy for consumers.
- ITEM8-CLI: Surface validator warnings on `goobers up` + `goobers status` with coded messages.

## 5. Open questions

- Resolved by VER-2: preview opt-in is instance-level through the Manifest annotation
  `goobers.dev/allow-preview-features: "true"`.
- Do we adopt strict SemVer for the app binary and treat the DSL feature-registry as the compatibility
  authority, or version the DSL independently (e.g. a `dslVersion` label consumers can pin)? Leaning:
  app SemVer + registry-as-authority; revisit if consumers need to pin DSL independently of the binary.
