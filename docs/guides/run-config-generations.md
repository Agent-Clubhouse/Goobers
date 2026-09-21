# Configuration generations for admitted runs

A newly admitted run retains an immutable, content-addressed copy of its
config-as-code tree. Its journal and engine input carry `configGeneration`.
Workflow definitions, goober instructions, assets, shared goobers, and provider
references come from that generation for later stages and crash recovery.
Reloading an unrelated schedule therefore does not invalidate an already
admitted run. New admissions use the newly applied configuration.

CLI stages verify the retained tree and the existing applied-config digest
before executing. A missing or modified generation is an error; execution does
not substitute the currently mounted configuration. Runs created before this
field existed retain their previous compatibility behavior.

## Storage and distributed execution

The instance stores extracted generations under `config-generations/` and
publishes their archives in a private `config-generations/` namespace beneath
its blob-store directory. Workers need access to the same fleet blob-store
volume via `--blob-store`. A freshly started worker fetches the admitted archive,
verifies its digest and originating instance identity, and builds the same
execution configuration without relying on in-memory reload history.

The artifact HTTP plane does not expose full configuration archives. An agentic
stage pod receives only its own goober's execution kit through that plane.
`instance.yaml` and credential values are not captured. Credential references
remain pinned while credential resolution stays live. Removing a merge task or
its merge capability from current configuration revokes merge authority even
for an older generation; unavailable or invalid current configuration refuses
merge authority. This check is separate from execution-generation verification.

## Retention limits

Each catalogue retains at most 64 generations and 256 MiB, including archive
copies and extracted files. Each archive permits at most 4,096 entries, 8 MiB
per file, 32 MiB of file contents, and 48 MiB encoded. Admission or reload refuses
when its generation exceeds these bounds or all reclaimable space is protected.

Every retained run journal protects its generation, including paused, failed,
and terminal journals that can still be continued. Process leases also protect
generations between definition publication and journal creation, and protect
worker caches while attempts use them. Reclamation checks durable journal
ownership again after acquiring an exclusive lease.

A standalone engine start has external history without a local journal-retention
signal. Such generations have a durable external-owner marker and are not
automatically reclaimed. The same finite limits apply; exceeding them refuses
new admission. Do not delete these archives while external engine histories can
still resume or retry. Process-owned generations remain protected until orderly
shutdown; restarting releases those process leases, while retained journal and
external-history ownership still applies.

## Operational settings boundary

This pins the applied config-as-code generation, not an entire machine image.
Existing durable run controls remain pinned separately. Startup-only settings
from `instance.yaml`, harness executable installations, and credential stores
retain their existing operational lifecycle. Changing those settings can still
require a restart or make an admitted configuration unavailable; they are not
silently serialized into execution archives. Configured credentials and merge
revocation are deliberately evaluated using current authority.

Remote merge commands consult the daemon's authenticated journal plane again
immediately before calling the provider land operation. The daemon derives the
workflow and generation from the run journal, checks the admitted stage's grant,
and checks current operator configuration. A worker's stale mounted tree or a
portable stage pod without configuration cannot substitute for that check.
Worker CLI children receive only a run-scoped journal bearer, never the parent
worker's privileged token. Workers need the configured signing key to mint this
bearer; unauthenticated development mode remains restricted to literal HTTP
loopback addresses. Missing authority or a failed check refuses the merge.

Merge-capable agentic harnesses receive the same scoped authority context for
nested Goobers CLI calls. Agentic pods receive only the journal-plane pair;
non-merge agentic stages receive neither. Codex's tool-shell environment retains
this narrow runtime context while continuing to exclude provider/model secrets.

Immutable generation metadata is independent of merge authority: ordinary
local/worker agentic subprocesses and Codex tool shells also receive the pinned
configuration directory, generation and instance identity. The built-in
`mcp-io` server consumes its per-invocation context file and materialized inputs,
not the live instance configuration. Portable agentic pods consume their pinned
execution kit; commands requiring a complete instance configuration still need
an instance-backed execution path.

Cross-platform extraction preserves the original archive digest. Verification
compares the retained tree against that manifest, refusing added, missing,
changed or symlinked paths. Unix hosts check all archived permission bits;
Windows checks the file read-only attribute that Go can represent, while keeping
the original Unix modes in the immutable manifest. This applies both when a
worker/recovery path loads a generation and when a nested CLI verifies its pin.

Asset bundles also retain their validated logical source modes, so their goober
fingerprints remain stable after extraction on a different operating system.
Missing mode metadata is synthesized inside the bounded archive before hashing;
existing metadata must match the captured asset contents. Capture never writes
metadata into the operator's source tree.
