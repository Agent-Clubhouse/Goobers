# Issue 7899: terminal journal worker identity

The cloud worker composition adds one line to bind its existing worker identity
to the live-journal HTTP emitter. The terminal transition uses a dedicated
worker-only MAC credential; a stage pod's existing bearer cannot authorize it.
The CLI owns the build-stamped worker identity. Minting, transport, refusal
classification and replay-safe journal handling remain in internal packages.
No command snapshot is re-pinned.
