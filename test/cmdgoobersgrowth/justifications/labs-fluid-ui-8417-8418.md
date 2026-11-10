# Labs Fluid UI requests and customization workflow (#8417, #8418)

The shipped `fluid-ui-customize` workflow needs two deterministic stage steps,
so `cmd/goobers/fluiduirequest.go` adds the `goobers fluid-ui-request` stage
command (`--load`, `--validate`). It is a thin adapter: it reads the stage
environment, calls `internal/fluidui` to load the request binding and to
evaluate the agent's candidate, and writes the stage result file. The same
file holds `labsFluidUIOptions`, which `goobers up` and the dashboard now call
instead of registering the Fluid UI store inline, so both hosts expose the
request routes with one shared line. Request storage, the lifecycle derived
from run journals, candidate validation and unmet-need normalization live in
`internal/fluidui`; the routes and authorization live in `internal/httpapi`.

The remaining growth is registration only: the CLI synopsis, completion model,
built-in stage command and runtime capability tables, and their goldens.
