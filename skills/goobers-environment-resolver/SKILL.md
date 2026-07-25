---
name: goobers-environment-resolver
description: Resolve the Goobers release, DSL support matrix, and authoritative local sources before another Goobers repository skill performs work.
---

# Goobers environment resolver

Establish which Goobers release and source material govern the repository.
Perform this resolution before authoring configuration, interpreting a run, or
planning an upgrade.

## Resolution procedure

1. Locate `.goobers/agent-toolkit/release.json` from the config repository
   root. Record its producer version, commit, and DSL versions.
2. If a `goobers` binary is available, run `goobers version --json` and
   `goobers versions --json`. Compare its version, commit, and DSL support
   matrix with `release.json`.
3. If a Goobers source checkout is available, use it only when its tag or
   commit matches the selected binary or toolkit release. Do not assume a
   checkout of the default branch matches an installed release.
4. Select one authoritative source set:
   - a matching source checkout and its validator;
   - otherwise the toolkit's bundled `docs/`, `api/schemas/`,
     `config-examples/`, and `internal/capability/` snapshots.
5. Report the selected release and source set to the calling skill. Surface a
   binary/toolkit mismatch explicitly; never silently combine contracts from
   different releases.

The bundle is sufficient when no source checkout exists. A network lookup or a
link to the default branch is supplementary and must not replace release-owned
material. Do not search for, print, or copy credentials while resolving the
environment.
