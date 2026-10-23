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
