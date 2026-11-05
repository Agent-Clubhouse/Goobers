# Cloud generation source composition

Issue #8075 adds the cloud composition seams that retain applied config
archives and expose the instance-scoped `GenerationSource` used by child
authority replay. The archive reader, PostgreSQL pin lifecycle, fenced
activator, bounds, and error classification remain under `internal/cloud`;
`cmd/goobers-cloud` growth is limited to constructing those services and
binding them to the controller lease alongside the existing acceptance store.
