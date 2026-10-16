# Add an audited `goobers pr retry` operator command (#2973)

The command is a thin CLI adapter: flag parsing, dry-run reporting, and wiring
to the existing provider, claim-lease, and remediation-checkpoint packages. It
must live in `cmd/goobers` beside the other `pr` subcommands and the journal,
help, and completion registration it extends. The lifecycle label and
checkpoint semantics reuse the existing internal packages rather than adding
new logic to the command package.
