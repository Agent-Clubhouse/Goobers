# Labs Fluid UI customization store wiring (#8415)

`goobers up` and the standalone dashboard each gained one line that registers
the per-user Fluid UI customization routes with the instance-rooted store
(`httpapi.WithLabsFluidUI(fluidui.ForInstance(root))`), plus its import.
Validation, storage, undo and authorization live in internal/fluidui and
internal/httpapi; the command layer adds no logic. The capability test map
lists the three new maintenance actions on its existing line.
