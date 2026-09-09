# Rendered configuration mirror

This opt-in overlay replaces a worker ConfigMap seed bridge with one rendered
snapshot on a shared RWX volume. It does not enable the API deployment or change
the [base authentication prerequisites](../README.md).

Set the following in the daemon's existing instance.yaml, then restart it:

```yaml
configMirrorPath: /var/lib/goobers-config-mirror
```

Select an RWX storage class that both node pools can mount. The share must
support atomic file replacement and reliable writer locking. The daemon alone
mounts it writable; worker init containers mount it read-only. Workers receive
no config-repository credential, and the mirrored instance document has its
workflowSource and configMirrorPath removed. This is configuration distribution,
not secret distribution: continue configuring the authenticated credential plane
and worker-specific runtime credentials separately.

Render with `kubectl kustomize deploy/reference/config-mirror`. Apply your own
image, storage, TLS/authentication, queue, namespace, and platform-specific
instance configuration choices before deploying. Remove the old worker config
ConfigMap mounts and copy commands from downstream overlays; do not run both
seed mechanisms. The mirror is not subject to the ConfigMap 1 MiB ceiling;
bounded admission currently permits 10,000 entries, 64 MiB per file, and 1 GiB
of uncompressed content.

Asset source modes travel as verified sibling metadata so Windows does not
recompute a Unix kit identity from its native permission bits. The metadata
binds every asset path, mode, and byte; changing the copied files without a new
seed is refused. Native Windows permissions and executable-file rules remain
unchanged. Keep the metadata with its seeded asset directory.

After accepted daemon reloads, worker-config.zip is atomically replaced. A
seeding worker holds one opened archive for its entire copy, validates the
instance and all referenced instructions/skills, and publishes its private
instance directory only on success. An unavailable mirror leaves the init
container failing and retrying, not a healthy but under-seeded worker.

The daemon retains its RWO journal. Each worker gets a pod-private emptyDir
instance, rather than mounting that journal from another node. Init retries
retain a completed seed and never overwrite a running instance. A newly created
pod seeds the current mirror. This overlay is a boot seed, not a live updater:
drain and recreate workers after configuration changes; the worker's normal
reload loop cannot discover bytes that were never copied into its private tree.
Preserve the base's graceful drain settings and retain old workers as needed
for runs pinned to their configuration. Do not delete an active worker merely
to refresh its seed.
