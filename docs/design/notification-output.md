# Notification output boundary

Notification decisions are rendered before dispatch and cross the runner boundary
as `goobers.dev/notification/request/v1`. The request carries stable notification,
incident, and event IDs; severity and transition; exact title, body, and optional
speech text; structured facts and evidence references; source run/workflow/stage;
requested registered sink kinds; expiry; and an idempotency key. It contains no
provider configuration or credentials. Canonical schemas live in
`api/schemas/notification-{request,receipt}.schema.json`.

Sink implementations register with `internal/notification.Registry`; workflows
name sink kinds but never construct transports. `Dispatcher` validates payload
limits, records the request, and passes the request unchanged to each sink.
Execution has a per-attempt timeout, caller cancellation, at most five attempts,
and an explicit `require-all` or `require-any` partial-delivery policy. A request
past its expiry is suppressed. The terminal sink is a credential-free development
transport, and the recording sink is the hermetic test transport.

Each attempt and each pre-delivery suppression produces a
`goobers.dev/notification/receipt/v1` with sink kind/version, attempt, timestamps,
delivered/failed/skipped status, optional external reference, and bounded,
redacted error detail. Requests and receipts append to the source run journal as
`notification.requested` and `notification.delivery.receipt`; journal scrubbing
runs before persistence. The dispatcher and run journal receive the same scrubber,
including the registry fed every resolver-issued credential, so returned errors
and durable records apply identical exact-value and pattern redaction. A delivered
receipt keyed by `(idempotencyKey, sink kind)` suppresses later delivery after an
in-process retry or journal recovery. Skipped and failed receipts are never
represented as successful delivery.
