import { describe, expect, it } from "vitest";
import { isEmptyTurn, parseTranscript, transcriptSearchMatches } from "./transcript";

const SCHEMA = "goobers.dev/telemetry/genai-event/v1";

function jsonl(...events: Record<string, unknown>[]): string {
  return events.map((event) => JSON.stringify({ schema: SCHEMA, ...event })).join("\n");
}

describe("parseTranscript", () => {
  it("reads roles, content, and model off the canonical event shape", () => {
    const { turns } = parseTranscript(
      jsonl(
        { role: "system", model: "gpt-5.6-sol" },
        { role: "user", content: "implement the issue" },
        { role: "assistant", content: "done", model: "gpt-5.6-sol" },
      ),
    );

    expect(turns.map((turn) => turn.role)).toEqual(["system", "user", "assistant"]);
    expect(turns[1].content).toBe("implement the issue");
    expect(turns[2].model).toBe("gpt-5.6-sol");
  });

  it("folds a tool result into the assistant turn that called it", () => {
    const { turns } = parseTranscript(
      jsonl(
        { role: "assistant", tool_call: { id: "call_1", name: "bash", arguments: { command: "go test ./..." } } },
        { role: "tool", content: "ok\tgithub.com/goobers/goobers", tool_call: { id: "call_1", success: true } },
      ),
    );

    expect(turns).toHaveLength(1);
    expect(turns[0].toolCall?.name).toBe("bash");
    expect(turns[0].toolCall?.arguments).toContain("go test ./...");
    expect(turns[0].toolResult).toBe("ok\tgithub.com/goobers/goobers");
    expect(turns[0].toolCall?.success).toBe(true);
  });

  // A transcript is evidence. A result with no matching call must remain
  // visible rather than being folded into an unrelated turn or dropped.
  it("keeps an unmatched tool result as its own turn", () => {
    const { turns } = parseTranscript(
      jsonl(
        { role: "assistant", content: "thinking" },
        { role: "tool", content: "orphan output", tool_call: { id: "call_missing", success: false } },
      ),
    );

    expect(turns).toHaveLength(2);
    expect(turns[1].role).toBe("tool");
    expect(turns[1].content).toBe("orphan output");
  });

  it("does not let a reused tool id overwrite an already-paired turn", () => {
    const { turns } = parseTranscript(
      jsonl(
        { role: "assistant", tool_call: { id: "call_1", name: "bash" } },
        { role: "tool", content: "first", tool_call: { id: "call_1" } },
        { role: "tool", content: "second", tool_call: { id: "call_1" } },
      ),
    );

    expect(turns[0].toolResult).toBe("first");
    expect(turns).toHaveLength(2);
    expect(turns[1].content).toBe("second");
  });

  it("pairs interleaved concurrent tool calls by id", () => {
    const { turns } = parseTranscript(
      jsonl(
        { role: "assistant", tool_call: { id: "a", name: "read" } },
        { role: "assistant", tool_call: { id: "b", name: "grep" } },
        { role: "tool", content: "grep output", tool_call: { id: "b" } },
        { role: "tool", content: "read output", tool_call: { id: "a" } },
      ),
    );

    expect(turns).toHaveLength(2);
    expect(turns[0].toolCall?.name).toBe("read");
    expect(turns[0].toolResult).toBe("read output");
    expect(turns[1].toolResult).toBe("grep output");
  });

  it("surfaces the usage record", () => {
    const { usage } = parseTranscript(
      jsonl({
        role: "assistant",
        usage: { input_tokens: 1467958, output_tokens: 12791, requests: 17, cost: 0 },
      }),
    );

    expect(usage?.inputTokens).toBe(1467958);
    expect(usage?.outputTokens).toBe(12791);
    expect(usage?.requests).toBe(17);
  });

  // Dropping a line a reader can see in the raw bytes would misrepresent the
  // record, so a malformed line becomes a visible turn instead.
  it("preserves a malformed line rather than skipping it", () => {
    const { turns, malformedLines } = parseTranscript(
      [JSON.stringify({ schema: SCHEMA, role: "user", content: "ok" }), "{not json", "[1,2,3]"].join("\n"),
    );

    expect(malformedLines).toBe(2);
    expect(turns).toHaveLength(3);
    expect(turns[1].malformed).toBe(true);
    expect(turns[1].content).toBe("{not json");
    expect(turns[2].malformed).toBe(true);
  });

  it("ignores blank lines and handles an empty transcript", () => {
    expect(parseTranscript("").turns).toEqual([]);
    expect(parseTranscript("\n\n  \n").turns).toEqual([]);
  });

  it("renders string tool arguments without re-quoting them", () => {
    const { turns } = parseTranscript(
      jsonl({ role: "assistant", tool_call: { id: "x", name: "apply_patch", arguments: "*** Begin Patch" } }),
    );

    expect(turns[0].toolCall?.arguments).toBe("*** Begin Patch");
  });

  it("treats an unrecognized role as unknown rather than dropping the turn", () => {
    const { turns } = parseTranscript(jsonl({ role: "developer", content: "hello" }));

    expect(turns).toHaveLength(1);
    expect(turns[0].role).toBe("unknown");
    expect(turns[0].content).toBe("hello");
  });
});

describe("transcriptSearchMatches", () => {
  const { turns } = parseTranscript(
    jsonl(
      { role: "user", content: "implement the widget" },
      { role: "assistant", tool_call: { id: "1", name: "bash", arguments: { command: "go test ./widget" } } },
      { role: "tool", content: "PASS", tool_call: { id: "1" } },
      { role: "assistant", content: "all green" },
    ),
  );

  it("matches content, tool name, arguments, and result", () => {
    expect(transcriptSearchMatches(turns, "widget")).toEqual([0, 1]);
    expect(transcriptSearchMatches(turns, "bash")).toEqual([1]);
    expect(transcriptSearchMatches(turns, "PASS")).toEqual([1]);
    expect(transcriptSearchMatches(turns, "green")).toEqual([2]);
  });

  it("is case-insensitive and empty for a blank query", () => {
    expect(transcriptSearchMatches(turns, "WIDGET")).toEqual([0, 1]);
    expect(transcriptSearchMatches(turns, "   ")).toEqual([]);
  });
});

describe("isEmptyTurn", () => {
  it("identifies a bare role marker", () => {
    const { turns } = parseTranscript(jsonl({ role: "assistant", model: "gpt-5.6-sol" }));
    expect(isEmptyTurn(turns[0])).toBe(true);
  });

  it("does not treat a turn with content as empty", () => {
    const { turns } = parseTranscript(jsonl({ role: "assistant", content: "hi" }));
    expect(isEmptyTurn(turns[0])).toBe(false);
  });
});
