# Shared Gaggle migration in `goobers fix`

The command must discover the Gaggles used by the selected workflows, resolve
their source files, report dry-run diffs, and coordinate writes. Preflighting
each shared floor before migrating its workflows, then writing that floor only
after those workflow migrations succeed, prevents the command from knowingly
leaving upgraded workflows against a refused legacy floor. This adds command
orchestration and one non-test Go file to `cmd/goobers`.

Reusable YAML parsing, refusal checks, placement conversion, and comment
preservation live in `internal/dslmigrate.MigrateGaggleToV3`. The command package
only adapts the loaded instance, filesystem, output streams, and existing
per-file write behavior to that transformation. Extracting this orchestration
would move command-specific policy without creating a reusable migration API.
