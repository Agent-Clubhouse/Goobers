# HA-P3-07: cloud start custody composition

`cmd/goobers-cloud` adds a controller lease-binding hook and a Temporal starter
factory to its existing database services. These are composition-root wiring:
the factory must pair the configured Temporal client with this instance's actual
PostgreSQL acceptance store and may never fall back to the local starter.
Admission, memo classification, fenced reconciliation, and provisional recovery
remain in internal packages. HA-P3-18 (#7818) supplies the controller host that
binds the acquired lease and invokes this factory; the unbound factory refuses
starts. No new command files or shared growth-baseline changes are needed.
