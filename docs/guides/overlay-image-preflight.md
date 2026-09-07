# Consumer overlay and image preflight

`goobers doctor --k8s --overlay-dir ./cluster/goobers-system` adds two
consumer-side checks to the cluster report (#4298). Without an overlay, both
rows explicitly report unchecked/WARN. Supplying an empty or unreadable tree
fails; inspecting zero pin sites or image requirements is never a pass.

```sh
goobers doctor --k8s --overlay-dir ./cluster/goobers-system \
  --image-runtime docker --image-tools git,gh,node \
  --image-ca ./internal-root.pem --timeout 2m --report json
```

Use trusted overlays and images. Rendering invokes `kubectl kustomize`, which
may fetch remote bases. The default image policy pulls from the registry using
the runtime's existing authentication, then resolves an immutable image ID and
uses that ID for all probes. No registry login or cluster mutation is performed.
The probes execute temporary network-isolated containers without host mounts;
Linux probes additionally use a read-only root filesystem, dropped capabilities,
and no-new-privileges. Each owned container is force-removed afterwards, including
after cancellation. Pulls may update the local image cache. `--timeout` bounds
each render, pull, inspection, and probe, not the whole report.

For explicit offline artifact inspection use `--image-pull-policy never`.
This examines cached images only and says so in the report; it does **not**
prove what the registry's current tag points to. The default `always` policy
never falls back to a cached image after an authentication or network failure.
Offline image inspection does not make remote-base rendering or ancestry checks
offline.

## Evidence checked

- Pin agreement follows local kustomization resources, bases, components,
  patches, and YAML generator inputs, including embedded instance configuration.
  Remote Goobers base refs, Goobers image replacements and runner host tags must
  name one full 40-hex upstream commit. Image identity suffixes such as
  `-windows` remain distinct. Comments and unrelated files are not pin evidence.
  This catches the recorded 140-commit base/image drift.
- Rendered workload containers/initContainers and configured runner-host images
  are inspected. The actual `goobers --version --json` commit must equal the pin.
  Declared command executables and `/usr/local/bin/` argument paths must exist
  and be executable. This covers the missing `gocache-trim` sidecar incident.
- `--image-tools` supplies the consumer's additional required PATH tools,
  applied to every inspected image; bare container command names are also checked.
  An absent tool inventory is explicitly unchecked, not an inferred guarantee.
- `--image-ca` supplies exactly one currently valid self-signed CA PEM. Linux
  checks it against the image's `SSL_CERT_FILE` bundle (or the conventional
  `/etc/ssl/certs/ca-certificates.crt`); Windows checks the exact root-store
  certificate. An omitted CA is explicitly unchecked. This checks the specified
  trust store, not every application's private trust configuration.
- A non-off `GOOBERS_MEMORY_HIGH_WATER` requires the corrected memory-gate
  source commit from #3967. A bounded `gh api` compare must prove ancestry;
  unavailable or indeterminate evidence fails. The image's own version stamp
  is checked separately: source ancestry alone is not artifact evidence.
- Configured `GOCACHE_ORPHAN_ROOT`/`GOCACHE_ORPHAN_AGE_MIN` on a
  `/usr/local/bin/gocache-trim` invocation must appear in the image's script.
  This is a script-content compatibility check, not proof of reaping behavior.

Inputs are bounded to 512 files, 4 MiB per file, 32 MiB total, 16 embedded-YAML
levels, 128 images, 64 KiB for the CA, and 4 MiB per command output stream.
Unsupported aliases or malformed relevant inputs fail rather than disappear
from the checked set. An image OS the selected runtime cannot execute remains
unverified/failing; a Linux probe is not evidence for a Windows image.
