# #8188: control serves the stage blob plane over its object store

`cmd/goobers-cloud` grows by about 19 lines of composition:
- a `BlobPlane` controlPlanes field and its assembly row;
- `constructBlobPlane`, which wraps the role's existing object store in `httpapi.WithBlobService`;
- passing it to control's handler together with `httpapi.WithLeaseFreeBlobPlane()`.

The plane itself already exists below `cmd`: the `internal/httpapi` blob handlers, and the `internal/cloud/objstore` store and its GC write protocol. So does the authentication, in `internal/cloud/podauth`. The only new behaviour, the recovery-gate exemption for exactly the blob routes, lives in `internal/httpapi`.

Only the role's composition root can attach its object store to its handler.
