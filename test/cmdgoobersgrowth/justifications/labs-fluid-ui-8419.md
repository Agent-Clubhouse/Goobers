# Labs Fluid UI promotion report CLI (#8419)

cmd/goobers gained the `goobers labs fluid-ui report` command group
(`cmd/goobers/labs.go`) plus its registry, synopsis, and completion entries.
The command only wires arguments to `fluidui.RunReportCLI`; event recording,
ranking, rendering, and privacy normalization live in internal/fluidui. The
new HTTP report route is served by the existing Fluid UI service registration,
so `goobers up` and the dashboard need no changes.
