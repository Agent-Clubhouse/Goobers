# Codex stage transcripts and usage

The Codex stage adapter converts `exec --json` items into the shared transcript
schema. Command execution, MCP calls, file patches and web searches produce
matched tool-call/result records. Repeated item updates or terminal snapshots
produce one call/result pair; repair invocations namespace item IDs so a restarted
CLI's `item_1` does not collide with the initial invocation's item.

Assistant messages are retained on item completion. Other native events remain
inspectable as system records prefixed with `[codex event]`; their JSON payload
is preserved within the output bound. They are not promoted to verified child
agent lifecycle records. Agent telemetry retains its existing partial-fidelity
classification.

Input, output, cache-read, cache-write and reasoning token counters follow the
CLI's cumulative thread snapshots. Initial and resumed snapshots merge once;
the canonical transcript carries one final stage usage record, matching the
stage summary. Omitted measures remain unknown, while reported zeroes remain
zeroes. The CLI does not supply authoritative per-model attribution or USD cost,
so the adapter does not invent either. A configured model name is invocation
metadata, not proof of a provider-reported model breakdown.

Stdout events are consumed independently of retained combined process output.
A small transcript budget therefore does not discard terminal usage merely
because earlier tool output filled the buffer. Finalization preserves a bounded
prompt/assistant/usage floor and marks dropped bytes. A single event still has
a parser size bound; this is not unbounded transcript storage. Metrics and tool
evidence already observed are retained when the process subsequently fails.
