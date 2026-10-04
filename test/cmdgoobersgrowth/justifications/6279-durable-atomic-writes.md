# cmd/goobers growth: #6279 durable atomic writes

Growth: +2 non-test lines and +0 files in `cmd/goobers`. The issue explicitly
requires migrating the API read cache's former staged-write paths to the shared
durability writer. Those paths had already moved into `internal/apireadstore`, so
the command change documents that existing transactional boundary instead of
adding duplicate filesystem code.

The comment belongs on the command-local cache wiring because that is where the
legacy `writeDisk` and `writeBody` call sites were expected and where future
maintainers need the migration context. The reusable persistence implementation
remains in `internal/apireadstore`; no command behavior or API changes.
