---
name: goobers-environment-resolver
description: Resolve the effective Goobers binary, release, DSL support, config and instance locations, target repositories, and verified release-matched contract sources before another Goobers repository skill performs work.
---

# Goobers environment resolver

Establish which Goobers release and release-owned contracts govern the current
work. Perform this resolution before authoring configuration, interpreting a
run, or planning an upgrade.

Treat the config repository, initialized instance, Goobers source checkout, and
target application repositories as separate locations. Never infer that one
repository has another role without marker or structured-config evidence.

## Safety boundary

Use read-only filesystem, Git, Goobers, and provider commands. Do not install,
build, clone, fetch, pull, checkout, start a daemon, or run target-repository
scripts. Do not read environment dumps, credential helpers, `.env` files, token
files, secret stores, or paths named by credential references. Do not print raw
Git remote URLs, which may contain user information.

Inspect only caller-supplied paths, the current repository and its ancestors,
the selected instance, the exact config-source path declared by that instance,
and configured target repositories. Missing, inaccessible, or competing paths
are unresolved; they do not authorize a broad search of parent or sibling
directories.

## Resolution procedure

### 1. Record the environment locations

Find the current Git root without reading remotes:

```sh
git -C <start> rev-parse --show-toplevel
```

To determine whether that root is a configured target, use an in-process
repository-identity helper. The helper must capture both stdout and stderr when
invoking Git rather than inherit them: first enumerate remote names, then
capture the fetch URLs for each name. Do not run the URL-producing Git command
directly through a terminal or tool invocation that displays, records, or
returns raw command output.

While the captured bytes remain private to the helper, parse each URL into a
credential-free provider key. Accept GitHub URL forms only as
`github/<owner>/<name>` and Azure DevOps URL forms only as
`ado/<organization>/<project>/<name>`; discard scheme, user information, port,
query, fragment, and a terminal `.git`. Normalize provider host and
provider-defined case-insensitive identity components. The helper may return or
print only normalized provider keys, never raw URLs or parse errors containing
them. If recognized remotes produce zero or multiple distinct identities,
report the current repository identity unresolved. Match a target only when the
entire sanitized provider key equals the key from structured config; a
repository name or owner/name suffix alone is not evidence.

Classify locations by these markers:

| Role | Evidence |
|---|---|
| Checked-in config source | `manifest.yaml` and `gaggles/` |
| Initialized instance | `instance.yaml` and `config/` |
| Installed toolkit | `.goobers/agent-toolkit/manifest.json` and `release.json` |
| Goobers source | `go.mod` declaring `github.com/goobers/goobers`, `cmd/goobers/`, and `docs/ARCHITECTURE.md` |
| Target application | Exact identity appears in structured Goobers config |

Use an explicitly supplied instance or config-source path first. Otherwise,
accept an ancestor instance or config source only when the markers identify
exactly one candidate. Record `<instance>/config` as the active runtime copy and
the instance's `workflowSource` as the config source; do not substitute one for
the other. When `workflowSource` is absent, `<instance>/config` is both the
default config source and active runtime copy; report it as a local directory
rather than leaving the config source unresolved.

Resolve `workflowSource` according to its structured kind:

- For `kind: local-dir`, an absolute `path` identifies exactly that directory.
- For `kind: git` with `path`, report the absolute local repository path and the
  configured `ref` (default `main`). Normalize it to a local branch ref and use
  read-only Git plumbing to require that it resolves to exactly one commit;
  report that full commit. Treat the committed ref, not its working tree, as
  the source:

  ```sh
  git -C <workflowSource.path> rev-parse --verify refs/heads/<ref>^{commit}
  ```

- For `kind: git` with `url`, keep the URL inside the same capture-and-sanitize
  boundary used for repository identity. Report its normalized provider key
  when recognized; otherwise use the credential-free identity
  `git/<lowercase-host[:port]>/<repository-path>`. Also report the configured
  `ref` (default `main`) and its full commit when existing read-only provider
  access can resolve it. Do not read or report its `token` locator or value. The
  materialized active copy remains `<instance>/config`; do not invent a local
  source path for the remote. Failure to inspect a private remote leaves its
  access and commit unresolved without discarding the configured identity/ref.

A relative local `workflowSource.path` is resolved by Goobers from the daemon
process working directory, not from the instance root. Resolve it only when
that working directory is supplied by the caller or authoritative
process/supervisor metadata; otherwise preserve the configured value as
evidence and report the config source unresolved. A present `workflowSource`
with a missing, malformed, or unsupported kind, path, URL, or ref is likewise
unresolved rather than guessed.

Resolve the executable in this order:

1. a caller-supplied executable;
2. executable `bin/goobers` in a selected Goobers source checkout;
3. `goobers` on `PATH`.

Canonicalize the path and record the selection rule. Run the exact selected
binary, preserving command failures as diagnostics:

```sh
<goobers> version --json
<goobers> versions --json
<goobers> config show --json <instance-root>  # only when an instance is selected
```

Classify the binary as `source-built` only when repository `bin/goobers` and
the source checkout identities agree; as `installed-release` only when a
non-repository binary is corroborated by a verified toolkit or exact release
ref; and as `PATH-only` when PATH selected it but no matching release material
is available.

Record the binary version and commit and every entry from
`dslVersions[]`, including `version`, `level`, `unsupportedAfter`,
`replacement`, and the complete `history[]`. Do not expect a `versions[]` array
or a `lifecycle` field. Also record the config source, instance root, and
configured target repositories. Read effective Gaggle target definitions from
the active `<instance>/config` tree, including when the source is Git-backed;
the source location and active materialization are separate evidence.
`config show` does not resolve credential locators; omit those locator fields
from the report anyway.

### 2. Establish an exact release identity

Compare version, commit, and DSL support independently. Ignore one optional
leading `v` only when comparing versions. Treat commits as equal only when one
non-empty value is an unambiguous prefix of the other. Empty values and
placeholders such as `dev` and `unknown` do not prove a match.

Resolve an abbreviated commit instead of comparing prefix text. Require at
least four hexadecimal characters, then resolve it against the selected
repository's object database by requiring
`git rev-parse --disambiguate=<abbrev>` to return exactly one object and
verifying it with `git rev-parse --verify <abbrev>^{commit}`, or resolve it
against the canonical provider repository with
`gh api repos/Agent-Clubhouse/Goobers/commits/<abbrev> --jq .sha`. Accept the
abbreviation only when that lookup returns exactly one full commit and every
other release-identity source resolves to that same commit. An ambiguous,
missing, or inaccessible abbreviation leaves the identity unresolved.

A candidate matches only when every field available on both sides agrees. A
matching version never overrides a commit conflict, and a matching commit never
overrides a version conflict. Report a conflict rather than selecting either
source.

Before selecting any contract source, require these release-owned paths:

```text
docs/ARCHITECTURE.md
docs/requirements/
api/schemas/workflow.schema.json
config-examples/manifest.yaml
internal/capability/capability.go
skills/goobers-environment-resolver/SKILL.md
```

Also require the `SKILL.md` and referenced files for every skill that will run
next. A directory or one representative file does not prove that the complete
contract source exists.

### 3. Select and verify one contract source

Evaluate candidates in this order. Reject an invalid candidate and continue
only to another candidate with the same exact release identity.

#### Matching local source checkout

Read `HEAD` and tags with `git rev-parse HEAD` and `git tag --points-at HEAD`.
When the binary reports a commit, `HEAD` must match it. When an exact release
tag is used as version evidence, that tag must also match the binary version.
Every available comparison must agree: an exact tag match never overrides a
conflicting commit, and a matching commit never overrides a conflicting tag.

Require every contract root and required path to be tracked at `HEAD`, and
reject local modifications under those roots. Parse the full `ls-tree` records,
including mode, type, object ID, and path; `--name-only` is insufficient:

```sh
git -C <source-root> ls-tree -r -z --full-tree HEAD -- \
  docs api/schemas config-examples internal/capability skills
git -C <source-root> status --porcelain -- \
  docs api/schemas config-examples internal/capability skills
```

Require every consumed entry to be a blob and reject mode `120000` anywhere
under the contract roots. This rejects a clean tracked symbolic link before a
later working-tree read can escape the selected source. Also reject any
untracked or modified contract path. After those checks, read only the verified
paths; alternatively, read their bytes directly with
`git cat-file blob HEAD:<path>` and retain the object ID as evidence.

Report the source root, full `HEAD`, exact tag when present, and absolute paths
to its docs, schemas, examples, capability registry, and skills.

#### Matching installed toolkit

The installed candidate root is exactly
`<config-repo>/.goobers/agent-toolkit`. Both `manifest.json` and `release.json`
must exist. When the selected binary provides `agent-kit check`, run:

```sh
<goobers> agent-kit check <config-repo>
```

Parse the command's documented text report; `agent-kit check` has no JSON mode.
Select the toolkit only when the command succeeds, reports `state: current`,
`update available: no`, and `none` for both modified and missing owned files.
Require both its `source binary version`/`source binary commit` and its
`installed source version`/`installed source commit` to match `version --json`
and each other. This command compares the installation with the toolkit
embedded in that exact binary, so it is preferred over manual inspection.

If `agent-kit check` is unavailable, fail safely while checking the installed
manifest:

1. Before opening any toolkit file, use non-following metadata checks to require
   the selected config repository and every descendant component through
   `.goobers/agent-toolkit` to be existing, non-symbolic-link directories.
   Canonicalize both validated roots and require the toolkit root to remain
   beneath the canonical config repository. Require `manifest.json` at that
   exact toolkit root to be a regular, non-symbolic-link file beneath the
   canonical config repository before opening or parsing it.
2. Validate `manifest.json` against the release-matched
   `agent-toolkit-manifest.schema.json`, then parse it. Require `$schema`,
   ownership, adapters, CLI capabilities, the real `dslVersions[].level` and
   `history[]` shape, and every other schema-required field. Reject duplicate
   assets, unknown schema or bundle versions, malformed digests, and unsafe
   paths.
3. Before opening an asset, require its path to start with
   `payload/.goobers/agent-toolkit/`, contain no `..` or backslash segment, and
   resolve beneath the candidate config repository after removing only the
   leading `payload/`. Reject a symbolic link in any path component between the
   config repository and the asset.
4. Require every listed asset to be a regular, non-symbolic-link file. Recompute
   and compare its SHA-256, byte size, and permission mode. Validate every
   asset, not only the paths needed by the next skill.
5. Require `release.json` itself to be in that verified inventory. Then compare
   the manifest and release producer version, commit, and complete DSL support
   matrix with each other and with the binary evidence.
6. Require the complete manifest inventory for every contract tree, not one
   representative file per directory. In particular, parse the
   `docs/requirements/README.md` index and require every local requirements
   document it references to be present in the verified inventory. Require all
   declared adapter and skill paths as well as every contract path listed
   above. If any check cannot be performed, mark the toolkit unresolved; do not
   treat partial validation as intact.

Report the absolute toolkit root and these exact selected locations:

| Contract | Installed location |
|---|---|
| Docs | `<toolkit-root>/docs` |
| Schemas | `<toolkit-root>/api/schemas` |
| Examples | `<toolkit-root>/config-examples` |
| Capability registry | `<toolkit-root>/internal/capability` |
| Skills | `<toolkit-root>/skills` |

If no binary exists, an intact manifest-inventoried toolkit may govern
repository-side authoring. Report `ready without binary`; binary compatibility
and live validation remain unresolved.

#### Matching remote release ref

Use remote contracts only through existing read-only provider authorization.
The canonical release source is `github:Agent-Clubhouse/Goobers`; do not infer a
source repository from the current checkout, a target repository, or provider
defaults.

Resolve the exact release tag, peel annotated tags to a commit, and compare that
commit with the selected binary. Enumerate the tree at the full commit, require
an untruncated result and every contract root, then request required files with
that full commit as `ref`. Parse the Git ref's `object.type` and `object.sha`;
when the type is `tag`, request `git/tags/<object.sha>` until the object type is
`commit`. Parse the recursive tree's `sha`, `truncated`, and complete `tree[]`
entries, requiring each consumed path to be a non-symlink blob. For GitHub, use
requests scoped like:

```sh
gh repo view Agent-Clubhouse/Goobers --json nameWithOwner
gh api repos/Agent-Clubhouse/Goobers/git/ref/tags/<version>
gh api repos/Agent-Clubhouse/Goobers/git/tags/<annotated-tag-object>
gh api 'repos/Agent-Clubhouse/Goobers/git/trees/<full-commit>?recursive=1'
gh api 'repos/Agent-Clubhouse/Goobers/contents/<path>?ref=<full-commit>'
```

For every required file, parse the contents response's `path`, `sha`, `type`,
`encoding`, and `content`; require a file whose object ID matches the tree entry
and whose content decodes successfully. Report the provider repository, tag,
full commit, and exact-ref locations for docs, schemas, examples, capability
registry, and skills. If repository identity, ref identity, tag peeling, tree
completeness, or any required content fetch cannot be proven, remote contracts
are unresolved.

For a known binary release, failure to find an exact verified source is a hard
unresolved result. Never query or link to `main`, silently follow the default
branch, or combine prose, schemas, examples, or capabilities from different
sources.

### 4. Retain target-repository evidence

Build the target set only from structured config (`repos`, Gaggle
`spec.project`, and `spec.additionalRepos`). Form each complete provider key
using all provider-required fields: provider, owner/organization, project for
Azure DevOps, and repository name. Keep the config repository separate unless
its complete key is also explicitly configured as a target. Prefer a local
checkout only when its sanitized Git-derived key exactly matches; otherwise use
existing read-only provider access.

For each target, report repository identity, configured branch, local or
provider-only access, README and applicable agent-guidance paths, and likely
CI/build entry points such as `.github/workflows/`, `Makefile`, `Taskfile*`,
`Justfile`, `go.mod`, or `package.json`. Inspect these files but do not execute
them.

## Required report

Return the report before invoking another Goobers skill:

| Field | Required value and evidence |
|---|---|
| Current repository | Absolute root and classified role, or unresolved |
| Executable | Canonical path, selection rule, and provenance (`source-built`, `installed-release`, `PATH-only`, or unresolved) |
| Binary identity | Version and commit from `version --json` |
| DSL support | Version, level, optional transition fields, and full history from `versions --json` |
| Config source | Kind, absolute local path or exact remote identity, configured ref, and full commit when resolvable |
| Instance | Absolute root and active config path, independently from config source |
| Contract source | Kind, root/provider repository, version, commit, exact tag/ref, and integrity result |
| Contract locations | Separate docs, schemas, examples, capability, and skills locations |
| Targets | Structured identity, branch, access, guidance, and build/CI entry points for each |
| Diagnostics | Every missing, ambiguous, conflicting, inaccessible, or unverifiable value |

Use `high` confidence only for explicit paths, successful machine-readable
commands, verified manifest assets, exact Git identity, or exact provider refs.
Use `unresolved` for missing, ambiguous, conflicting, inaccessible, or partially
verified locations, and state the smallest explicit path, ref, or authorization
needed to resolve each one. Do not hide a component mismatch behind an overall
confidence value.
