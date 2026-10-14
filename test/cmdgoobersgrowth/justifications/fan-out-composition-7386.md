# Render DSL 3.1 maps in `goobers workflow show` (#7386)

`goobers workflow show` prints its text graph in `cmd/goobers/workflow.go`,
beside the existing parallel case and its `parallelConcurrency` helper. A map
fell through to the generic task case, which printed a misleading single
`-> task` edge and no concurrency. The added map case and its
`mapConcurrency` helper mirror the parallel ones; the graph projection itself
stays in `internal/workflow`, so no reusable logic moves into the command
package.
