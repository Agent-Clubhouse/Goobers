# Windows Pod Restrictions

> Status: **implemented — reference worker shape; the *worker* shape it
> describes is superseded as the execution substrate**
> Delivered-by: #3619

> **⚠️ Read this first (#4240).** This note was written against the resident
> `goobers worker` Deployment, which `goobernetes-architecture.md` §10
> supersedes as the execution substrate: mode-3 stages run in **fresh
> dispatcher-created pods**, one per stage attempt, not inside a resident
> worker. The Windows *admission and filesystem facts* below remain correct and
> load-bearing — they are properties of Windows pods under Pod Security
> Admission, not of the worker — and the dispatcher applies the same rules when
> it renders a Windows stage pod.
>
> What is **not** settled by this note is whether
> `deploy/reference/goobers-system/worker-windows-deployment.yaml` should remain
> as a control-plane utility, be kept as a legacy reference, or be retired. That
> disposition is an open item on
> [#4240](https://github.com/Agent-Clubhouse/Goobers/issues/4240) and is
> deliberately not decided here.
>
> The authoritative statement of what a Windows runner may declare and what is
> enforceable on it is
> [`goobernetes-restrictions.md`](goobernetes-restrictions.md) §D4 and its §9
> matrix — in particular that `readOnlyRootFilesystem` is silently inert on
> Windows, so `fs:readonly-except-workspace` is **undeclarable** on a Windows
> runner and refused at instance load, at validate (CAP005), and at pod render
> (#3619).

This note records the Windows-specific parts of the reference worker in
`deploy/reference/goobers-system/worker-windows-deployment.yaml`. It is an
opt-in worker for a Windows node pool; the Linux worker remains the default.

## Admission and filesystem field table

Windows pods are not the Linux worker with a node selector. The following
fields have different meanings or are unavailable:

| Field | Windows behavior | Reference shape |
|---|---|---|
| `securityContext.runAsNonRoot` | Evaluated by Pod Security Admission; it cannot reliably describe a Windows identity and causes a `restricted` namespace admission failure (AKS measurement, Infra ledger L-124). | Omitted; the namespace uses `enforce: baseline` with `warn` and `audit` still `restricted`. |
| `runAsUser`, `runAsGroup`, `fsGroup` | Linux-only identity and group fields. | Omitted. |
| `windowsOptions.runAsUserName` | Selects the Windows container identity. | `ContainerUser` is set on the init and worker containers. |
| `windowsOptions.hostProcess` | Controls host-process access. | Explicitly `false` at pod and container scope. |
| `readOnlyRootFilesystem` | Accepted by the API server but silently inert on Windows (the fail-open direction). | Not used as the filesystem control. |
| writable worker state | Must be writable by the selected Windows identity. | The init container binds the instance root with NTFS ACLs using `icacls`. |

The `baseline` enforcement label is not a relaxation of the Windows worker's
identity contract. It is required because PSA `restricted` rejects the pod at
admission, before a Pending pod exists to inspect. Keeping `warn` and `audit`
at `restricted` preserves the tripwire for accidental Linux-style fields.

## Image and startup constraints

If a Windows Dockerfile uses backtick continuations, `# escape=\`` must be
**line 1**. Otherwise Docker parses the continuations incorrectly even though
the file can look valid in review. The image's Windows base tag must match the
node-pool version; the reference production measurement uses `ltsc2022`.

Windows images are approximately 2.4 GB and measured cold pull/build startup
to the first stage at 6m53s and 7m37s. Those measurements fit within the
45-minute `DefaultWindowsScheduleToStart` budget, but the budget must remain
finite: a missing Windows worker should fail with the selected queue named,
not wait indefinitely. Warm pools and image publishing are separate concerns.
