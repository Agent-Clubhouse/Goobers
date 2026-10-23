# Control-role journal plane composition (#7818)

`goobers-cloud control` composes the journal plane from the already-merged
PostgreSQL journal backend and the shared live-journal writer. The new
constructor only wires those owners together over the controller database and
lease: the backend, writer, fencing and conformance suites live in
internal/cloud/journalpg and internal/livejournal, and nothing here adds a
second writer, lifecycle or storage path. The constructor refuses a missing
object store so a pod-executed agentic stage can never silently lose its span.
It lives in its own file so the manifest in controlassembly.go keeps one line
per seam as the remaining planes land.

The control role also gains the required `--object-store-url` /
`--object-store-container` options and `bindControlObjectStore`, which hands the
controller role the object store the journal's span source (and, next, the
retained config generations) live in. Without it the journal constructor can
never succeed, so the role refuses to boot rather than binding it only when set.
`--check-assembly` now reports a seam as pending, naming the missing option,
when this invocation could not compose it.
