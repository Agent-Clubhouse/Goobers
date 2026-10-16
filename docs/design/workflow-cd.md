# Design: Workflow CD — GitOps config source for the local daemon

> Status: **implemented — shipped** (WCD-1..7 #454–#461 and epic #453 closed — workflowSource, GitSource last-known-good reconcile, `goobers apply`, configrepo:read grant, isolation pen-test all live) · Area prefix: `WCD` · Milestone: **Workflow CD** (#15)
> Requirements: [`docs/requirements/config-as-code.md`](../requirements/config-as-code.md), [`docs/requirements/deployment.md`](../requirements/deployment.md), [`docs/requirements/security.md`](../requirements/security.md)
> Architecture: [`docs/ARCHITECTURE.md`](../ARCHITECTURE.md)
> Related issues: #337 (continuous-reconciliation CD daemon), #336 (`goobers apply`), #154 (config hot-reload), #250 (capability enforcement for built-in kinds)
> Delivered-by: #454, #455, #456, #457, #458, #459, #460, #461
> Verified: ff826744e (2026-10-13)

**Proposed extension, not shipped:** [Apply-time gaggle references and portable
cloud adoption](apply-time-gaggle-references.md) defines logical references to
whole gaggle subtrees, dependency locks, skills/instruction expansion and
cloud cutover. This shipped design covers one root `workflowSource`; it does
not itself provide remote includes, filesystem-symlink composition, or live
instance migration.

## 1. Why this exists

Today the local daemon watches workflow/gaggle config in its provisioned local
directory. The PO wants to formalize a **continuous-delivery model** where a
*separate* workflow-config repo is the source of truth, mirroring how ArgoCD
tracks a git repo — but for the local (tier-1/2) daemon, not just the tier-3
operator. This is brain-dump **item 12**.

Core properties the PO specified:

1. A **separate repo** (local path or remote), distinct from the code repo.
2. **`main` is the tracked ref.** The daemon does **not** care what's checked out on disk if the source
   is a local repo — switching branches in that repo's working tree changes nothing; it reconciles against
   `main`'s committed state.
3. **Hooks on `main`, or polling/watch** for workflow-config changes.
4. A **clear, separate integration point** in an instance for the workflow-config location: local (file
   path) or remote (requires a token, just like the GitHub provider).
5. **Isolation**, proven adversarially: the CD integration uses a **separate scoped PAT** with access to
   *only* the CD repo; the code-repo PAT cannot reach the CD repo and the CD PAT cannot reach the code repo.

## 2. Shipped state and extension boundary

- `goobers up` watches materialized definitions by default. A Git
  `workflowSource` continuously reconciles its committed tracked branch.
  `goobers apply` requests an explicit reconcile from the live daemon.
  Invalid candidates retain the last-known-good definitions; cold startup
  without usable definitions fails explicitly.
- `workflowSource` selects one local directory or Git repository, separate
  from product `repos[]`. Git materialization reads root `manifest.yaml` and
  `gaggles/`; there is no `subdir` selector or multiple-source composition.
  A local directory may itself be a selected config subtree.
- Remote Git reads use the dedicated `configrepo:read` credential seam through
  `workflowSource.token` or GitHub App authentication. They do not borrow the
  product-stage token.
- `instance.yaml`, including source provisioning and runner/engine settings,
  is startup-pinned and requires restart. Definition hot reload does not mean
  instance provisioning hot reload or engine-history migration.
- The daemon remains the cloud control plane. `config-sync`/operator rendering
  is a separate delivery caller, not permission to let workers independently
  fetch and choose configuration revisions.
- [Run configuration generations](../guides/run-config-generations.md) pin
  executable definitions for admitted work. The
  [rendered config mirror](../../deploy/reference/config-mirror/README.md)
  seeds new workers only; it is not a live updater. A cloud configuration
  change must remain executable through the generation/kit transport used by
  that deployment.

The original WCD-1..7 design below records the delivered root-source model.
Reference graph resolution, parameter expansion and cloud adoption belong to
the linked draft, not to the shipped WCD acceptance claim.

## 3. Design

### 3.1 Config source abstraction (WCD)

Introduce a **ConfigSource** seam behind the current directory loader:

- `LocalDirSource` — today's behavior (a plain directory); kept as the default/tier-0.
- `GitSource` — points at a repo (local path *or* remote URL) + a **tracked ref (`main`)**. Reconciles
  against the **committed tree of `main`**, explicitly *ignoring the working-tree checkout* (property #2:
  `git` plumbing reads the ref, not the workdir). For remote, clones/fetches into an instance-managed
  mirror under the instance root (never the code worktree).
- Instance config gains a **separate, explicit integration point** for the workflow-config location
  (property #4), e.g. `instance.yaml: workflowSource: {kind: local-dir|git, path|url, ref: main, token: <ref>}`.
  This is distinct from the code `repos:` list.

### 3.2 Reconciliation loop (WCD) — extends #337/#154

- **Watch/poll/hooks** (property #3): support (a) filesystem watch for a local git dir, (b) periodic
  `git fetch` + compare-`main`-HEAD poll for remote, and (c) a webhook/hook entry point for push-driven
  reconcile. Configurable; poll is the always-available floor.
- On a detected `main` change: re-render → **validate** → if valid, atomically swap the running config
  (reuses the compile path); if invalid, **retain last-known-good**, surface a warning (item 8 plumbing),
  and do not tear down running work. This fixes today's "invalid config aborts, no LKG" gap.
- **`goobers apply`** (#336): an explicit one-shot reconcile-now against the tracked ref, for operators
  who don't want to wait for the poll interval.
- In-flight runs stay pinned to the definition version they started on (WF-016); only new runs pick up
  the new config. This is where real registry versioning (Versioning milestone #12) and this milestone meet.

### 3.3 Scoped-credential isolation (WCD) — extends #250

- Add a **capability** for config-repo access (e.g. `configrepo:read`) to `internal/capability`.
- Map it via a **dedicated `Grant`** to the workflow-source token ref — **not** `Repos[0]`. This requires
  unwinding the `runnerwiring.go` single-PAT collapse so the CD source uses its own ref.
- The CD reconciler is the *only* consumer granted `configrepo:read`; stage executors for the code repo
  are never granted it, and vice versa.

### 3.4 Adversarial isolation test (WCD) — property #5

A dedicated test (and a documented pen-test procedure) that provisions **two scoped PATs**:

- `CD_PAT` — access to *only* the workflow-config repo.
- `CODE_PAT` — access to *only* the code repo.

Assertions: the CD reconciler cannot read/write the code repo with `CD_PAT`; code-repo stages cannot
read the CD repo with `CODE_PAT`; a misconfiguration that crosses the streams **fails closed**. This is
both an automated integration test (against real scoped tokens in a CI secret, or recorded fixtures) and
a runbook for a manual adversarial check.

## 4. Issue breakdown (milestone #15)

- **[EPIC]** Workflow CD.
- WCD-1: `ConfigSource` seam + `LocalDirSource` refactor (no behavior change; enables the rest).
- WCD-2: `GitSource` — track committed `main`, ignore working-tree checkout; local-dir + remote(mirror).
- WCD-3: Instance `workflowSource` integration point (local path | remote+token), distinct from `repos:`.
- WCD-4: Reconcile loop — poll (floor) + fs-watch + hook entry; last-known-good on invalid (extends #337/#154).
- WCD-5: `goobers apply` — explicit one-shot reconcile (folds #336).
- WCD-6: `configrepo:read` capability + dedicated Grant; unwind the `Repos[0]` single-PAT collapse (extends #250).
- WCD-7: Adversarial isolation test + pen-test runbook (two scoped PATs, fail-closed cross-access).

## 5. Open questions

- Remote-mirror location & GC policy (where the fetched CD repo lives under the instance root).
- Hook transport for push-driven reconcile — reuse the same localhost API surface (#14) or a separate
  webhook listener? Leaning: fold into the daemon API service so there's one inbound surface.
- Does `main` stay hardcoded or become a configurable `ref`? Leaning configurable, default `main`.
