# Gaggle health daemon controller

Issue #4425 adds daemon-owned gaggle health orchestration that must run outside
workflow scheduling and wake on daemon state transitions. The `cmd/goobers`
adapter owns the resolved instance definitions and daemon lifecycle needed to
compose that reusable controller, publish reloads atomically, and stop it during
shutdown; detector evaluation and durable episode behavior remain in
`internal/gagglehealth`.
