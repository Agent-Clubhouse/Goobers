# HAW-HITL human gate and guidance wiring

This PR adds 14 non-test Go lines and no files to `cmd/goobers`: one composition
helper constructs `internal/intervention.HumanService` with the existing
intervention service, applied gaggle policy, shared operator-message service and
daemon credential scrubber; startup invokes it and reports an initialization
failure. Command parsing, admission, persistence, authorization and HTTP handlers
stay in their existing internal packages. The focused daemon assembly acceptance
test is excluded from the production growth count.
