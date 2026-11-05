# #8085 (d): probe control's canary through the pods' blob endpoint

`cmd/goobers-cloud` grows by about 12 lines.
- The worker's stage dispatch builds a `dispatcher.BlobClient` for the endpoint stage pods are handed (`GOOBERS_BLOB_ENDPOINT`), with a worker-blob bearer. The canary probe reads through that client instead of the worker's own store.
- `probedChildAuthority` takes that plane, through a small interface, instead of the worker seams.

The probe logic stays in `childadmissionclient.ProbedAuthority`. This is wiring that only the worker's composition root can do, because it alone holds both the pods' endpoint and the signing key.
