# Scoped verification evidence in `goobers trace` (#8312)

Growth: one call and a small text-rendering helper in `cmd/goobers/trace.go`
display the recorded verification outcomes, scoped repair links, and counts of
unverified or dropped reports in the existing trace summary.

Report decoding, provenance checks, repair matching, persistence, and graph
projection live in `internal/creditgraph` and `internal/executor`. Only the
command's human-readable presentation belongs here; this does not add another
checker or reusable verification implementation to the command package.
