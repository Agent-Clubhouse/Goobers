# HA-P3-06 PostgreSQL child custody

The cloud composition root adds one line binding its existing object store to
its PostgreSQL acceptance store. Child custody keeps digest-backed payloads in
that store. The implementation and policy stay in internal/cloud/acceptance;
the local binary does not grow and no new command file is added.
