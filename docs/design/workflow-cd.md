# Design: Workflow CD — GitOps config source for the local daemon

> Status: **Draft for review — not implemented** · Area prefix: `WCD` (new) · Milestone: **Workflow CD** (#15)
> Requirements: [`docs/requirements/config-as-code.md`](../requirements/config-as-code.md), [`docs/requirements/deployment.md`](../requirements/deployment.md), [`docs/requirements/security.md`](../requirements/security.md)
> Architecture: [`docs/ARCHITECTURE.md`](../ARCHITECTURE.md)
> Related issues: #337 (continuous-reconciliation CD daemon), #336 (`goobers apply`), #154 (config hot-reload), #250 (capability enforcement for built-in kinds)

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

## 2. Current state (grounded)

- **Local daemon (tiers 1–2): direct-directory watch, but no CD or reconcile-from-git.**
  `goobers up` polls `<instance-root>/config`, validates changed definitions, and
  atomically reloads them between scheduler ticks. Invalid edits retain the
  last-known-good definitions and are recorded in the instance journal.
- The only "reconcile" in the local path is `localscheduler.Reconcile` reconciling in-memory run counters
  against on-disk run journals at startup — **not** config-vs-git.
- **Tier-3 GitOps exists but is operator/ArgoCD-based:** `cmd/config-sync` renders a config repo into CRs
  for ArgoCD to apply; `cmd/operator` reconciles them (controller-runtime). This is the K8s pattern, not
  an in-daemon watch loop. Item 12 asks for the *local-daemon* analog.
- **Credential seam supports this; wiring doesn't yet.** `internal/credentials` resolves N named token
  refs (`instance.yaml` `Config.Repos[].Token`), and `Grant{Capability, Ref}` can map a capability to a
  *specific* ref — but `cmd/goobers/runnerwiring.go:62` **collapses all capabilities onto `Repos[0]`'s
  single token** (self-described "known simplification"). There is **no capability string** for
  config-repo access in `internal/capability`.
- `#336 goobers apply` and `#337 CD daemon mode` are unbuilt; `#154` supplies
  direct-directory hot reload but not Git reconciliation.

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
