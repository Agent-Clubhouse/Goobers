# Cancellable daemon drain serialization

The daemon owns the process-local reconciliation cursors and its sweep ordering.
Their existing mutex belongs to this command owner. Replacing it with a small,
zero-value guard makes lock acquisition respect the drain caller's cancellation
without changing the active owner's context, custody, or execution lifetime.

The additional command code is specific to this drain orchestration. There is
no new public queue schema or reusable scheduler policy to extract. Tests use
the real durable-trigger entry point with an active, blocked dispatch and verify
that cancelling a waiting pass preserves the active start and later admission.

This is a prerequisite for bounded drain passes. It does not claim wall-clock
bounding of an active pass or of a provider that ignores its context.
