# Explicit DSL 2.1 migration CLI

`fix` gains permission-expansion acknowledgment and rollback flags, plus a thin
adapter that plans a complete workflow cohort before writing any source. This
belongs in the command package because it connects the existing instance loader,
CLI diagnostics and unified-diff rendering. Source transformation, structured
permission changes, bounded backup persistence, path validation and rollback are
implemented in `internal/dslmigrate`. The CLI does not duplicate those mechanisms.
