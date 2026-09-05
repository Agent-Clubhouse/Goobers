# Windows Pod Restrictions

> Status: **implemented — reference worker shape**

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
