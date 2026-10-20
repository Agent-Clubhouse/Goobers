# Chat fixtures

`internal/chat/chattest.ReadFixture` accepts Clubhouse's version 1 JSONL format.
The first record has `kind:header`, `format:clubhouse-chat-fixture`, `version:1`,
`protocol`, and optional `cli`, `source`, `recordedAt`, `description`, `synthesized`.
Later records have `from:client|cli`, optional millisecond `t`, and exactly one
of `msg`, `raw`, `stderr`, or `exit:{code}`. Notes (`kind:note`) are ignored.

`Fixture.Replay` blocks on its receive callback for client messages, matching
the protocol method (or Claude type/control subtype) and optional partial
`match` object. It remaps client request IDs on responses, including Claude
`request_id`; server-initiated request IDs remain unchanged. Mismatch errors
contain only the entry number, never prompts or RPC parameters. Replay returns
only after every entry is consumed or an error occurs. The caller owns process
lifetime and timing; timestamps are retained but replay does not sleep.

`chattest.Conformance` runs common lifecycle assertions against a fixture-backed
Driver factory. CI must never call real models. FakeDriver offers direct event
injection for host tests; native translation needs separate wire replay tests.

For live captures use a disposable sample workspace and a tiny Copilot prompt.
Record SDK events as `from:cli,msg:<SessionEvent>` with protocol `copilot-sdk`.
Label hand-authored traffic `source:synthetic`, list invented shapes under
`synthesized`, and never pass it off as recorded traffic. Before committing,
remove account identifiers, tokens, connection URLs, environment values, home
paths and repository content not belonging to the public sample. Review every
record; automatic redaction alone is insufficient.

Format and matching design: Clubhouse MIT, commit `3309665c`,
`docs/chat-fixtures.md` and `scripts/chat-fixtures/fixture-format.mjs`.
