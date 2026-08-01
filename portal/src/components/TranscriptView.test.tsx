import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { TranscriptView } from "./RunStageInspector";

const SCHEMA = "goobers.dev/telemetry/genai-event/v1";

function jsonl(...events: Record<string, unknown>[]): string {
  return events.map((event) => JSON.stringify({ schema: SCHEMA, ...event })).join("\n");
}

const transcript = jsonl(
  { role: "system", model: "gpt-5.6-sol" },
  { role: "user", content: "Implement the claimed issue end to end." },
  {
    role: "assistant",
    tool_call: { id: "call_1", name: "bash", arguments: { command: "go test ./widget" } },
  },
  { role: "tool", content: "ok\tgithub.com/goobers/goobers/widget", tool_call: { id: "call_1", success: true } },
  { role: "assistant", content: "Committed the widget fix." },
  { role: "assistant", usage: { input_tokens: 1467958, output_tokens: 12791, requests: 17 } },
);

function openTranscript() {
  render(<TranscriptView text={transcript} />);
}

describe("transcript rendering", () => {
  // The transcript is the only record of why an agentic stage acted as it did.
  // As raw JSONL the roles were indistinguishable and tool calls were not
  // paired with their results.
  it("renders roles, pairs a tool call with its result, and collapses the result", () => {
    openTranscript();

    expect(screen.getByText("User")).toBeInTheDocument();
    expect(screen.getAllByText("Assistant").length).toBeGreaterThan(0);
    expect(screen.getByText("bash")).toBeInTheDocument();
    // Content renders as text, not escaped JSON.
    expect(screen.getByText("Implement the claimed issue end to end.")).toBeInTheDocument();

    // The result is behind a toggle rather than dumped inline.
    expect(screen.queryByText(/ok\tgithub.com\/goobers/)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Show result" }));
    expect(screen.getByText(/ok\s+github\.com\/goobers\/goobers\/widget/)).toBeInTheDocument();
  });

  // Usage and the concluding message are the two most-wanted points and the
  // hardest to reach by scrolling raw JSONL.
  it("surfaces the usage record without scrolling", () => {
    openTranscript();

    const usage = screen.getByText("Usage").closest("p");
    expect(usage).not.toBeNull();
    expect(within(usage as HTMLElement).getByText(/1,467,958 in/)).toBeInTheDocument();
    expect(within(usage as HTMLElement).getByText(/17 requests/)).toBeInTheDocument();
  });

  it("filters turns by search and restores them when cleared", async () => {
    const user = userEvent.setup();
    openTranscript();

    const search = screen.getByRole("searchbox", { name: "Search transcript" });
    await user.type(search, "widget");

    expect(screen.getByText("bash")).toBeInTheDocument();
    expect(screen.queryByText("Implement the claimed issue end to end.")).not.toBeInTheDocument();

    await user.clear(search);
    expect(screen.getByText("Implement the claimed issue end to end.")).toBeInTheDocument();
  });

  it("reports when nothing matches rather than rendering an empty list", async () => {
    const user = userEvent.setup();
    openTranscript();

    await user.type(screen.getByRole("searchbox", { name: "Search transcript" }), "nonexistent");
    expect(screen.getByRole("status")).toHaveTextContent("No turns match");
  });

  // The rendered view replaces the default, never the evidence.
  it("keeps the raw JSONL one click away", () => {
    openTranscript();

    fireEvent.click(screen.getByRole("button", { name: "Show raw JSONL" }));
    expect(screen.getByText(/"role":"system"/)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Show conversation" }));
    expect(screen.getByText("Committed the widget fix.")).toBeInTheDocument();
  });
});
