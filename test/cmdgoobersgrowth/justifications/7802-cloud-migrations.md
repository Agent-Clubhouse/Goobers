# HA-P3 cloud migration composition

Issue #7802 wires `goobers-cloud migrate apply` to the reusable migration
framework. The command-package growth is limited to parsing the direct database
URL, authentication mode, and required backup reference, then constructing and
closing the cloud pools; catalog, lock, compatibility, and migration behavior
remains under `internal/cloud/migrate`.
