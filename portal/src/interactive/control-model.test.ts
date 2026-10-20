import { describe, expect, it } from "vitest";
import { driverSettings, pendingApprovals } from "./control-model";
import type { InteractiveEvent } from "./client";
const event = (sequence: number, type: string, data: Record<string, unknown>, turnId = "turn"): InteractiveEvent => ({sequence, type, data, sessionId: "s", turnId, time: "2026-01-01T00:00:00Z"});
const permission = event(1, "permission_request", {requestId: "p", title: "Run tests", content: [{type: "text", text: "npm test"}], options: [{optionId: "allow_once", label: "Allow once"}, {optionId: "reject_once", label: "Deny"}, {optionId: "allow_always", label: "Always"}]});
describe("control replay", () => {
  it("retains accepted requests until resolution and excludes ended turns", () => {
    const accepted = event(2, "command.accepted", {requestId: "p", commandType: "permission.respond"});
    const requests = pendingApprovals([permission, accepted], "turn");
    expect(requests).toHaveLength(1);
    expect(requests[0]).toMatchObject({pending: true, options: [{optionId: "allow_once"}, {optionId: "reject_once"}]});
    expect(pendingApprovals([permission, accepted], "another")).toEqual([]);
    expect(pendingApprovals([permission, accepted, event(3, "permission_resolved", {requestId: "p", by: "timeout"})], "turn")).toEqual([]);
  });
  it("unlocks a rejected response and uses only linked concrete tool content", () => {
    const request = {...permission, data: {...permission.data, content: [], toolCallId: "tool-1"}};
    const tool = event(0, "tool.started", {toolCallId: "tool-1", content: [{type: "terminal", command: "npm test"}]});
    expect(pendingApprovals([tool, request, event(3, "command.rejected", {requestId: "p", reason: "Request expired"})], "turn")[0]).toMatchObject({preview: ["npm test"], pending: false, failure: "Request expired"});
  });
  it("normalizes canonical questions without inventing choices", () => {
    const question = event(1, "question", {requestId: "q", questions: [{questionId: "one", prompt: "Choose checks", options: [{id: "unit", label: "Unit"}], allowMultiple: true, allowFreeText: false}]});
    expect(pendingApprovals([question], "turn")[0]).toMatchObject({questions: [{id: "one", multiSelect: true, allowFreeText: false, options: [{id: "unit", label: "Unit"}]}]});
  });
  it("updates settings only from confirmed values and preserves default effort", () => {
    const info = event(1, "session_info", {currentModel: "a", currentEffort: "high", models: [{id: "a", label: "A", efforts: ["low", "high"]}, {id: "b", label: "B", efforts: ["medium"]}]});
    expect(driverSettings([info, event(2, "command.accepted", {commandType: "session.set_model", model: "b"})]).model).toBe("a");
    expect(driverSettings([info, event(3, "command.delivered", {commandType: "session.set_model", actual: "b"})])).toMatchObject({model: "b", effort: undefined, efforts: ["medium"]});
    expect(driverSettings([info, event(3, "command.delivered", {commandType: "session.set_model", actual: "b"}), event(4, "command.delivered", {commandType: "session.set_effort", actual: ""})])).toMatchObject({model: "b", effort: "", efforts: ["medium"]});
  });
  it("accepts the Codex/Copilot modelId and Claude model confirmation payloads", () => {
    expect(driverSettings([event(1, "model_changed", {modelId: "codex", effort: "", effortIsDefault: true})])).toMatchObject({model: "codex", effort: ""});
    expect(driverSettings([event(1, "model_changed", {modelId: "copilot"})])).toMatchObject({model: "copilot", effort: undefined});
    expect(driverSettings([event(1, "model_changed", {model: "claude"})])).toMatchObject({model: "claude", effort: undefined});
  });
});
