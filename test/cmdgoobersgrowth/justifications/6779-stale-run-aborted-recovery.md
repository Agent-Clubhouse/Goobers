# Stale run-aborted recovery

Issue #6779 requires pull-request selection to distinguish trusted Goobers
comments from human review activity and to recognize a current-head
merge-review pass before clearing `goobers:run-aborted`. The added coordination
belongs in `cmd/goobers` because it extends the existing provider-backed
`pr-select` recovery transaction, including its post-removal safety
revalidation; no reusable lower-level primitive is introduced.
