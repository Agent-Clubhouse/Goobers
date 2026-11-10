# Fleet join projection scopes (#8374)

`goobers fleet join` gained a repeatable `--projection <name>` flag and
`goobers fleet status` now reports the association's active projections. The
added non-test lines in cmd/goobers are the help text for the flag and status
field, the flag declaration and validation call, passing the projections into
`fleet.JoinOptions`, the shell-completion model entries that
TestCompletionFlagsMatchHandlerFlagSetsAndSynopsis requires for every handler
flag, and the JSON field plus text line in the status output. Projection
validation, persistence, the hello handshake, and clearing projections on
revocation live in internal/fleet; the command layer adds no logic beyond
formatting.
