# Repository document browser installation

The existing daemon workbench adapter now installs the repository source factory
and document read route alongside native backlog reads. Provider verification,
literal path and commit bounds, source authorization and cache behavior remain in
the existing internal packages. This small composition uses the same scheduler
directory and redaction registry as other interactive reads; it introduces no new
command entry point or credential resolver. No growth baseline is changed.
