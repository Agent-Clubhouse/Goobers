# Fleet workflow convergence completion entries (#8345)

`goobers workflow clusters` gained `--snapshot`, `--instance-id`, `--salt` and
`--fleet`. The four added lines in cmd/goobers are the shell-completion model
entries that TestCompletionFlagsMatchHandlerFlagSetsAndSynopsis requires for
every handler flag. Flag parsing, snapshot construction and fleet aggregation
live in internal/workflowcluster; the command layer adds no logic.
