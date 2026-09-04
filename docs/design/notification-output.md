# Notification output boundary

> Status: **historical** — implemented under #2569, then deleted unused in #4233;
> the schemas remain, the `internal/notification` implementation does not.

Notification decisions are rendered before dispatch and cross the runner boundary
as `goobers.dev/notification/request/v1`. The request carries stable notification,
incident, and event IDs; severity and transition; exact title, body, and optional
speech text; structured facts and evidence references; source run/workflow/stage;
requested registered sink kinds; expiry; and an idempotency key. It contains no
provider configuration or credentials. Canonical schemas live in
`api/schemas/notification-{request,receipt}.schema.json`.

Sink implementations register with the dispatcher's sink registry; workflows
name sink kinds but never construct transports. `Dispatcher` validates payload
limits, records the request, and passes the request unchanged to each sink.
Execution has a per-attempt timeout, caller cancellation, at most five attempts,
and an explicit `require-all` or `require-any` partial-delivery policy. A request
past its expiry is suppressed. The terminal sink is a credential-free development
transport, and the recording sink is the hermetic test transport. A timed-out
attempt is not retried because a non-cooperative sink may still complete its
external side effect after the dispatcher deadline. The recorder atomically
checks durable delivery state and journals an attempt-start receipt before
invoking a sink, so concurrent dispatchers cannot both claim delivery. Timed-out
outcomes are marked unresolved. While that attempt remains unresolved, later
dispatches with the same idempotency key and sink are durably suppressed across
process restarts. Its eventual outcome is recorded before the barrier is
released, so a late successful delivery cannot be repeated.

Each attempt journals a pending `goobers.dev/notification/receipt/v1` before
invocation and then its delivered/failed outcome; each pre-delivery suppression
also produces a receipt. Receipts carry sink kind/version, attempt, timestamps,
optional external reference, and bounded, redacted error detail. Requests and
receipts append to the source run journal as
`notification.requested` and `notification.delivery.receipt`; journal scrubbing
runs before persistence. The dispatcher and run journal receive the same scrubber,
including the registry fed every resolver-issued credential, so returned errors
and durable records apply identical exact-value and pattern redaction. A delivered
receipt keyed by a stable SHA-256 digest of the idempotency key and sink kind
suppresses later delivery after an in-process retry or journal recovery without
depending on scrubbed plaintext. Skipped and failed receipts are never
represented as successful delivery. Sink kinds and versions are canonical
identifiers without surrounding whitespace. Transport references longer than
2,048 characters are omitted from otherwise successful receipts so persisted
receipts remain schema-valid without making an already completed delivery
retryable.
