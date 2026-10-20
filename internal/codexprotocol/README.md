# Codex app-server protocol

Bindings are generated from the stable JSON Schema emitted by `codex-cli
0.163.0`. Install that exact CLI, then run from the repository root:

```
go run ./internal/codexprotocol/generate
go run ./internal/codexprotocol/generate -check
```

The check regenerates into memory and rejects schema/type/method drift or a
CLI version mismatch. The SHA-256 covers the canonical selected request,
response and event schemas plus the complete server method inventories.
Tagged unions remain `json.RawMessage` aliases, preserving native JSON values
instead of encoding them as base64. The transport intentionally omits JSON-RPC's
`jsonrpc` member, as required by app-server's JSONL protocol.

Generation workflow and transport/event ideas are adapted from
[Clubhouse](https://github.com/Agent-Clubhouse/Clubhouse), MIT, commit
`3309665c`: `scripts/generate-codex-app-server-types.mjs` and
`src/main/orchestrators/adapters/{codex-chat-driver,codex-app-server-client,codex-chat-events,ndjson-rpc-base}.ts`.
The original license is preserved in `CLUBHOUSE-LICENSE`.
