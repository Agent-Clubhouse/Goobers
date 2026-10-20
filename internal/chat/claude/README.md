# Claude stream-json

Protocol and normalization are adapted from [Clubhouse](https://github.com/Agent-Clubhouse/Clubhouse),
MIT licensed, commit `3309665c`, `src/main/chat/claude/{stream-transport,normalizer,driver,events}.ts`.
The upstream license is retained in `testdata/LICENSE`.

The nine fixtures in `testdata` are copied unchanged from that commit's
`test/chat-fixtures/claude`. Despite resembling recordings, their headers and
`docs/chat-fixtures.md` explicitly identify them as **hand-made approximations**.
The question answer and subagent shapes include synthesized fields. These tests
exercise protocol compatibility without authenticating or calling a model; they
are not evidence that every shape has been verified against a live CLI.

Only normalized content enters chat events. Tool inputs, account metadata and
unknown raw payloads stay out of diagnostics. Token usage is per turn; USD cost
is the delta of session totals. The first resumed total has no known baseline,
so its cost is omitted. A lower total after a conversation reset starts a new
baseline. Assistant errors are coalesced into one error at the turn result.
