# cmd/goobers growth: issue #6331

#6331 adds the opt-in, shadow-only publication leak screen at the `open-pr` and
`file-issues` provider-write boundaries. The model request and settings logic
live in `internal/decisiongate`; only command integration and delivery through
the authenticated daemon-to-stage credential plane belong in `cmd/goobers`.
