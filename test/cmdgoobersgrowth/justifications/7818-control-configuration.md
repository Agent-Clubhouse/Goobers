# Control-role configuration composition (#7818)

`goobers-cloud control` composes its single configuration owner from a mounted,
read-only instance directory. The new constructors only bind existing owners:
the configgeneration builder (archive and digest), `internal/cloud/config`
(assessment, applier, guard and the directory source), `generationpg` (the
fenced object-store publish) and the generation source. The mounted-directory
loader, its change detection and its refusal handling live in
internal/cloud/config; the command layer adds only the lease-binding adapters
(the activator and generation source must publish and pin under the lease handle
that is live at the moment of use) and the `--config-dir` flag, which fails
closed at boot when the directory is absent.
