# #8093: compose provider cleanup in control

The command package adds about 35 non-test lines, with no new production file,
for a shared control-provider composition and passing its marker releaser to the
claims plane. The factory resolves command-owned database, configuration and
journal seams; terminal hooks reuse that same writer and credential cache.

Claim validation, repository binding, error scrubbing and the cleanup adapter
remain in `internal/cloud/terminalwriter`. No code is added to `cmd/goobers`.
