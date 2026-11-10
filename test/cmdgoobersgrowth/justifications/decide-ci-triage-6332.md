# decide CI failure triage mode

The advisory CI failure triage reuses the existing `goobers decide` stage
command (#6332) as additional flags (`--ci-log`, `--ci-baseline`, `--run-key`,
`--ci-outcome`, `--ci-matrix`) so it needs no new command, registry entry or
man page. The classification, redaction and confusion-matrix logic lives in
internal/decisiongate; cmd/goobers only parses flags, reads the log file,
appends to the ledger and writes the result file, plus the generated help,
synopsis and completion surface the new flags require.
