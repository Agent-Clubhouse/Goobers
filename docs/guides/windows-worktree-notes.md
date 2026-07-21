# Windows git/worktree audit & policy (#643)

Goobers' execution model leans hard on git worktrees: every provider-chain stage
runs in its own fresh worktree branched off a managed mirror clone, under paths
like `workcopies/<key>/runs/<runId>-<stage>` (see [`internal/worktree`](../../internal/worktree)
and [ARCHITECTURE.md §6](../ARCHITECTURE.md)). Nothing here fails to compile on
Windows, but Windows git has a cluster of behavioral differences that can corrupt
or silently break a run. This page records the audit of those differences, the
policy Goobers adopts for each, and the Windows prerequisites an operator must
satisfy.

This is the companion to the platform-neutral quickstart; it governs the
**managed working copies of target repos** that Goobers provisions. The line-ending
policy for the *Goobers repo itself* lives in the repo-root
[`.gitattributes`](../../.gitattributes).

## Summary of policy

| Concern | Decision | Enforced by |
|---|---|---|
| Line endings (CRLF) | `core.autocrlf=false` on every managed mirror; defer policy to the target repo's `.gitattributes` | `managedGitConfig`, `internal/worktree/manager.go` |
| Long paths (>260) | `core.longpaths=true` on every managed mirror; require the OS long-path setting on Windows | `managedGitConfig` + operator prerequisite |
| Symlinks | Do **not** force `core.symlinks`; detect symlinks git flattened to plain files and surface a per-run warning (never fail) | `Manager.checkSymlinkSupport` → `Worktree.Warnings` → `runner.annotation` journal event |

All three config values are **behavior-identical on darwin/linux**, where git's own
defaults already match (`core.autocrlf` defaults false; `core.longpaths` is a
no-op off Windows; `core.symlinks` is left at its native default of true). They
change behavior only on Windows, making a managed checkout deterministic there
instead of dependent on the Git-for-Windows installer's ambient config.

## 1. Long paths — path-length budget

Win32 imposes a 260-character `MAX_PATH` limit unless long-path support is enabled.
The worst-case managed worktree path Goobers constructs is:

```
<instanceRoot>/gaggles/<gaggle>/workcopies/<16-hex-key>/runs/<runId>-<stage>/<repo-internal-path>
```

Fixed overhead Goobers adds beyond the instance root and gaggle name (gaggle-scoped
layout):

| Segment | Chars |
|---|---|
| `/gaggles/` | 9 |
| `<gaggle>` | variable (k8s namespace ≤ 63) |
| `/workcopies/` | 12 |
| `<16-hex-key>` (repo-key, `repoKey`) | 16 |
| `/runs/` | 6 |
| `<runId>-<stage>` | 49 = 32 (`engine.RunID`, hex of 16 bytes) + 1 + 16 (longest shipped stage name, `park-needs-human`) |
| **Goobers overhead (excl. instanceRoot + gaggle)** | **92** |

Worked examples (worktree root, before the repo's own internal path):

- Gaggle-scoped, `instanceRoot=C:\goobers` (10), `gaggle=acme-web` (8): **110 chars** → **149 chars of headroom** to `MAX_PATH` for the repo's deepest committed path.
- Non-gaggle layout, `instanceRoot=C:\goobers` (10): **93 chars** → **166 chars of headroom**.

149–166 characters is enough for most repositories but **not** guaranteed for deep
trees (nested Java/monorepo package paths, the harness scratch dir
`.goobers/context/…`, etc.), so the budget cannot be assumed safe on Windows
without mitigation.

**Mitigation — config, not a naming change.** The run-id (32 hex) and stage leaf
are already compact and carry debugging value; shortening them would save little
and cost traceability. Instead:

1. Goobers sets **`core.longpaths=true`** on every managed mirror (automatic; see
   `managedGitConfig`). Worktrees inherit it, so `git worktree add` and every
   later git operation honor it. No-op off Windows.
2. On Windows the operator must **also** enable the OS long-path setting — `core.longpaths`
   alone is not sufficient for every git subprocess:
   - Registry: `HKLM\SYSTEM\CurrentControlSet\Control\FileSystem\LongPathsEnabled = 1` (DWORD), **or**
   - Group Policy: *Computer Configuration → Administrative Templates → System → Filesystem → Enable Win32 long paths*.
3. Recommended: keep the instance root short on Windows (e.g. `C:\goobers`) to
   preserve headroom.

## 2. Symlinks

`core.symlinks=false` is the Windows default — symlink creation needs Developer
Mode or elevation — so a repo containing symlinks checks them out as **plain text
files whose contents are the link target path**. To an agent (and to `git status`)
that looks like ordinary content, so a run against such a repo can silently
produce wrong edits and diffs.

**Decision:** Goobers does **not** force `core.symlinks` (forcing it false would
change darwin/linux behavior, which must stay identical, and forcing it true on
Windows would fail without privilege). Instead, after provisioning a worktree on a
symlink-fallback platform, `Manager.checkSymlinkSupport` scans the index for
symlink entries (git mode `120000`) that did not materialize as real symlinks and
records a per-run warning on `Worktree.Warnings`. The runner journals it as a
`runner.annotation` event (conformance-excluded, operator-visible). The run is
**not** failed — a repo's symlinks are often incidental to the change at hand —
but the degradation never passes silently. On darwin/linux the scan never runs
(symlinks materialize natively) and no warning is produced.

## 3. CRLF / line endings

The Git-for-Windows installer commonly sets `core.autocrlf=true` globally, which
rewrites line endings on checkout/checkin and would give a managed working copy
**phantom whole-file diffs** the moment it is provisioned.

**Decision:** Goobers pins **`core.autocrlf=false`** on every managed mirror, so
no line-ending rewriting happens regardless of the host's ambient config, and all
line-ending policy is deferred to the **target repo's own `.gitattributes`**
(the git-recommended approach). Target repos are advised to carry a
`.gitattributes` (e.g. `* text=auto eol=lf`) for a consistent cross-platform
checkout. The Goobers repo itself does — see the repo-root `.gitattributes`.

**Invariant protected:** a checkout followed by no edits must show an empty
`git status` on all platforms. This is asserted by
`TestManager_Create_CleanStatusInvariant` on every CI matrix leg.

## 4. Git version floor (Windows)

| Feature Goobers relies on | Minimum git |
|---|---|
| `git worktree add`/`remove`/`prune` | 2.17 (already the documented Linux floor) |
| `core.longpaths` support | 2.7 (Git-for-Windows has shipped it far longer) |
| Deterministic per-invocation `-c` config (`safe.bareRepository`) | 2.31 |

Practically, ship a **current Git-for-Windows (≥ 2.40)**: it bundles a long-path-aware
git binary and the worktree fixes, and matches the versions Goobers is exercised on
elsewhere. The exact version of each Windows CI run should be recorded into the
validation evidence artifact the same way `test/linuxvalidate` records it for Linux.

## 5. Windows prerequisites checklist (for the future Windows quickstart)

- [ ] Git-for-Windows ≥ 2.40 on `PATH`.
- [ ] OS long-path support enabled (`LongPathsEnabled=1` registry **or** group policy).
- [ ] Short instance root (e.g. `C:\goobers`) to preserve path headroom.
- [ ] Aware that target repos containing symlinks check out as plain files
      (Developer Mode enables real symlinks); watch for `worktree.warnings`
      `runner.annotation` events.
- [ ] Target repos carry a `.gitattributes` for deterministic line endings.

## Remaining activation work

These behaviors are covered by tests that run today on the ubuntu/macos matrix legs
and exercise the Windows-fallback path deterministically via injection
(`internal/worktree/windows_audit_test.go`). Two things are needed to run them as
**live** Windows cargo:

- **P6/#633** — the Windows CI matrix leg does not exist yet (the advisory
  `windows-smoke` job was removed pending it). When it lands, the existing tests
  run on it unchanged and additionally assert the real (non-injected) behavior.
- **Windows cross-compilation of `internal/worktree`** — the package's transitive
  deps (`internal/gooberassets`, `internal/platform/proc`) do not yet cross-compile
  to `GOOS=windows` (tracked by the P1–P4 abstraction work, #620–#627, and #1090's
  chain). The worktree audit code added here introduces no new Windows-incompatible
  imports — it is platform-neutral Go dispatching on `runtime.GOOS` — so it will
  cross-compile as soon as those land.
