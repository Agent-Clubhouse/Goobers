# Managed object sharing and external reference stores

Goobers can share git objects between the managed mirrors on one node. This guide
explains what that feature is, how it differs from pinned workspaces, sparse
checkout and partial clone, and where the safe boundary sits for pointing Goobers
at an object store you already own.

## The three things people confuse

| Mechanism | What it changes | Setting |
|---|---|---|
| Shared object cache | Where a *new* managed mirror gets its objects: borrowed from one node-level full mirror instead of cloned again per gaggle | `workcopies.objectCache: true` in `instance.yaml` |
| Partial clone | What a *new* mirror downloads: a blobless clone that fetches blobs on demand | `workcopies.partialClone: true` |
| Pinned workspace | How a run uses a checkout: one long-lived, serialized workspace instead of a fresh worktree per stage | `largeRepo: true` or `workspace.pinned` on a repo (see [large-repo mode](large-repo-mode.md)) |

None of these adopts a checkout you already have. Goobers always creates its own
mirror under the instance's `workcopies/` directory and owns it, including
cleanup.

## The shared object cache (`workcopies.objectCache`)

```yaml
workcopies:
  objectCache: true
```

Behavior, in the terms an operator can verify:

- **The cache is a full mirror.** There is one bare mirror clone per repository
  URL at `<pinnedRoot>/_objects/<repo-key>`, shared by every gaggle on the node
  that targets that repository. It is always a full clone, even when
  `partialClone` is on, because a blobless cache cannot answer an alternates
  lookup for a blob it never fetched.
- **New mirrors only.** A new managed mirror is created with
  `git clone --mirror --reference <cache>`, so its `objects/info/alternates`
  names the cache's `objects` directory. An existing mirror keeps its own full
  object store and is never retrofitted onto the cache, in either direction.
  Turning the option on later does not shrink anything already on disk.
- **The cache is refreshed** (fetched) on the same cadence as the mirror, under a
  cross-process file lock, so gaggles in different daemons do not race on it.
- **The narrow-refresh path does not use the cache.** Mirrors provisioned by the
  heads-and-tags large-repo path are created by `init --bare` plus a narrowed
  fetch, not by a clone, and do not borrow from the cache.
- **No automatic garbage collection.** A cache entry accumulates until an
  operator removes it deliberately. Removal is fail-closed (below).
- **Default off.** With the option unset, mirror creation is unchanged and no
  `_objects` directory is ever created.

### Sparse checkout does not mean less history

Sparse checkout narrows which paths are materialized in a worktree. It does not
narrow which objects are transferred: the mirror, and the shared cache, still hold
full history. A sparse worktree therefore saves working-tree disk and
checkout time, not clone bandwidth. If clone size is the problem, the levers are
partial clone (fewer blobs per mirror) and the shared cache (one transfer per
node instead of per gaggle), and they combine: a partial-clone mirror created
with a cache still borrows from the full cache.

### A synthetic two-mirror example

Two gaggles on one node target the same repository. With `objectCache: true`:

```
<pinnedRoot>/_objects/<repo-key>/            # the full cache (one transfer)
<workcopies-A>/<repo-key>/repo.git/          # mirror for gaggle A
<workcopies-B>/<repo-key>/repo.git/          # mirror for gaggle B
```

Each `repo.git/objects/info/alternates` contains the absolute path of the cache's
`objects` directory. Check the sharing and the integrity of each dependent:

```sh
# 1. Both mirrors reference the same cache.
cat <workcopies-A>/<repo-key>/repo.git/objects/info/alternates
cat <workcopies-B>/<repo-key>/repo.git/objects/info/alternates

# 2. Each mirror stores few objects of its own (most are borrowed).
git -C <workcopies-A>/<repo-key>/repo.git count-objects -v

# 3. Every borrowed object is reachable; this fails if the cache went missing.
git -C <workcopies-A>/<repo-key>/repo.git fsck --connectivity-only
```

Step 3 is the check that matters: a mirror whose cache disappeared still has refs
but cannot resolve the objects behind them.

### Removing a cache safely

`GCObjectCache` (package `internal/worktree`) deletes a repository's cache only if
no mirror under any supplied workcopies root still lists it in its alternates. It
fails closed: any dependent, or any error reading or walking an alternates file,
refuses the deletion. It runs under the same file lock as refresh. It is a
library function with no CLI command today and no background caller, so removal
is a manual operation; nothing treats a missing cache as safe to ignore.

## External reference stores: what is and is not supported

Operators sometimes have a large read-only reference clone already on the node and
want Goobers to borrow from it instead of paying for a second copy. **Goobers does
not support this today**, and the managed cache above is the supported way to
share objects. The boundaries below are the contract any future support must meet;
they are listed so that a manual setup does not quietly violate them.

A safe external reference store must:

1. **Be read-only to Goobers.** Goobers never writes to, fetches into, repacks or
   garbage-collects it. It is outside managed cleanup: the worktree reaper, run
   finalization and `GCObjectCache` only ever touch Goobers-owned directories.
2. **Have a verified identity.** Before any mirror borrows from it, its remote URL
   or root commit must match the target repository. Borrowing from the wrong
   repository silently produces mirrors that look healthy and fail later.
3. **Have a declared lifetime relationship with its consumers.** Either consumers
   dissociate at creation (`git repack -a -d` then removing the alternates entry,
   so they own their objects and the store may disappear), or the store is
   explicitly retained for as long as any consumer exists. "Borrowed, and may be
   deleted at any time" is not a safe state: git treats a vanished alternate as
   repository corruption.
4. **Survive concurrent access.** It must not be modified while consumers read it,
   and consumers must not assume it is quiescent.
5. **Never be an operator's working checkout.** A dirty user worktree is not a
   reference store. Its objects change under the consumer, and its owner may run
   `git gc --prune` at any moment.

Not in scope, by design: adopting dirty user worktrees, deleting externally owned
stores, retrofitting existing mirrors, and a cross-node cache service. A
read-only external object source is a separately designed capability; it is not
a configuration shortcut over `objectCache`. Any implementation of it has to test
disappearance of the source, concurrent access, and garbage collection without
corrupting consumers.

## Related

- Large-repo behavior and the pinned workspace: [large-repo mode](large-repo-mode.md).
- Where managed mirrors live and how to relocate them: [instance placement](instance-placement.md).
- Design background: [large-repo execution model](../design/large-repo-execution-model.md).
- The shared cache shipped in #654. Related open work: #4157 (immutable reference
  contract) and #5011 (mirror maintenance).
