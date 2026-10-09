# HAW-CHD child and recovery journal retention

The existing command-owned telemetry pruning guards now preserve journals for
both recovery inventory tiers and for child-family custody. The queue owns the
reusable, indexed custody query in `internal/triggerqueue`; the command adapter
binds its arguments to the journal's actual gaggle and run identity.

This adds 35 net production lines to existing command files, with no new command
production file. The adapters remain beside the existing trigger-receipt and
recovery-inventory guards, which are shared by normal and interrupted pruning.
Real queue/journal tests verify refusal, reservation rollback, and eventual
release after overflow retirement or child tombstoning. No durable schema or
public feature gate changes are required.
