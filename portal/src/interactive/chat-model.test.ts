import { describe, expect, it } from "vitest";
import { commandMessages, composerModes, fileDiffs, plainFailure, toolActivities } from "./chat-model";
import { normalizeEvent, type InteractiveEvent } from "./client";

const event = (sequence: number, type: string, data: Record<string, unknown> = {}): InteractiveEvent => ({sequence, type, data, sessionId: "s", turnId: "t", time: "2026-01-01T00:00:00Z"});

describe("chat presentation", () => {
  it("counts changed lines per file without counting file headers", () => {
    const files = fileDiffs("diff --git a/one b/one\n--- a/one\n+++ b/one\n@@ -1 +1,2 @@\n-old\n+new\n+extra\ndiff --git a/two b/two\n--- a/two\n+++ /dev/null\n@@ -1 +0,0 @@\n-removed\n");
    expect(files.map(({path, added, removed}) => ({path, added, removed}))).toEqual([{path: "one", added: 2, removed: 1}, {path: "two", added: 0, removed: 1}]);
  });
  it("folds lifecycle events by tool ID while preserving repeated legacy calls", () => {
    const tools = toolActivities([event(1, "tool.started", {toolCallId: "a", name: "Read invoice", kind: "read"}), event(2, "tool.started", {toolCallId: "b", name: "Tests"}), event(3, "tool.output", {toolCallId: "a", text: "contents"}), event(4, "tool.completed", {toolCallId: "a"}), event(5, "tool.completed", {toolCallId: "b"}), event(6, "tool.started", {name: "Tests"}), event(7, "tool.completed", {name: "Tests"})]);
    expect(tools.map(tool => tool.events.length)).toEqual([3, 2, 2]);
    expect(tools[0]).toMatchObject({name: "Read invoice", readOnly: true});
  });
  it("replays each steering bubble by command ID and preserves distinct identical text", () => {
    const events = [
      {...event(1, "command.accepted", {commandType: "steer", text: "Focus tests"}), commandId: "a"},
      {...event(2, "steering.delivered"), commandId: "a"},
      {...event(3, "command.accepted", {commandType: "steer", text: "Focus tests"}), commandId: "b"},
      {...event(4, "command.rejected"), commandId: "b"},
    ];
    expect(commandMessages(events, []).map(message => [message.commandId, message.state])).toEqual([["a", "Delivered"], ["b", "Rejected"]]);
  });
  it("shows actual queued delivery without calling acceptance delivered", () => {
    const local = [{commandId: "a", text: "Next", state: "Sending"}];
    const accepted = {...event(1, "command.accepted", {commandType: "steer", text: "Next"}), commandId: "a"};
    expect(commandMessages([accepted], local)[0].state).toBe("Accepted");
    expect(commandMessages([accepted, {...event(2, "command.delivered", {delivery: "queued"}), commandId: "a"}], local)[0]).toMatchObject({state: "Delivered", delivery: "queued"});
  });
  it("maps structured access errors to recovery instructions", () => {
    expect(plainFailure({reason: {code: "connection_expired", message: "raw"}}).text).toContain("Reauthenticate");
    expect(plainFailure({reason: '{"nested":"error"}'}).text).not.toContain("{");
    expect(plainFailure({reason: "A connection needs your attention."}).text).toBe("A connection needs your attention.");
  });
  it("chooses only actions the driver supports", () => {
    expect(composerModes(undefined, false)).toEqual({primary: "message"});
    expect(composerModes(undefined, true)).toEqual({primary: undefined});
    expect(composerModes({steer: true, interrupt: true, queue: "native"}, true)).toEqual({primary: "steer", secondary: "queued"});
    expect(composerModes({steer: false, interrupt: true, queue: "none"}, true)).toEqual({primary: "interrupt_continue", secondary: undefined});
    expect(composerModes({steer: false, interrupt: false, queue: "client"}, true)).toEqual({primary: "queued"});
  });
  it("normalizes v2 while keeping the host replay sequence and safe text", () => {
    const chat = {v: 2, sessionId: "s", turnId: "t", seq: 3, ts: 1000, type: "agent_message_chunk", data: {content: [{type: "text", text: "original"}]}};
    expect(normalizeEvent({...event(15, "assistant.delta", {chat, text: "safe prefix"})})).toMatchObject({sequence: 15, type: "assistant.delta", data: {text: "safe prefix"}});
    expect(normalizeEvent({...chat, commandId: "command"})).toMatchObject({sequence: 3, commandId: "command", time: "1970-01-01T00:00:01.000Z", type: "assistant.delta", data: {text: "original"}});
  });
  it("keeps unknown protocol events visible", () => {
    expect(normalizeEvent({...event(15, "execution.note", {chat: {type: "protocol_unknown", data: {name: "future"}}})}).type).toBe("protocol_unknown");
  });
});
