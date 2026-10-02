# Reviewed remediation recovery

Issue #6407 requires merge-review selection to revalidate an aborted pull
request after removing its abort marker and to restore that marker when a newer
abort, unresolved review thread, or human hold appears concurrently. This logic
belongs in `cmd/goobers` because it coordinates provider state transitions in
the existing pull-request selection command; it is not a reusable lower-level
provider or runner primitive.
