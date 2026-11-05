# Labs: `workflow validate-child --cluster-model` (#8311)

Growth: one completion-model entry for the new `--cluster-model` file flag on
`goobers workflow validate-child`. The synopsis string gains the flag in place.

Model loading, content-address verification, and novelty scoring live in
`internal/workflowcluster`; flag parsing and report rendering live in
`internal/childworkflow`. Nothing further can move out of `cmd/goobers`.