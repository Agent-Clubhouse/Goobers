import type { InteractiveEvent, PermissionLevel } from "./client";
import type { ApprovalRequest, PermissionOption, Question } from "./ApprovalDock";
import type { DriverSettings } from "./DriverSettings";

function record(value: unknown): Record<string, unknown> { return value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : {}; }
function items(value: unknown): Record<string, unknown>[] { return Array.isArray(value) ? value.map(record) : []; }
function text(value: unknown): string { return typeof value === "string" ? value : ""; }
function strings(value: unknown): string[] { return Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : []; }
const levels = new Set(["plan", "ask", "auto_edit", "auto", "full_access"]);
function level(value: unknown): PermissionLevel | undefined { return typeof value === "string" && levels.has(value) ? value as PermissionLevel : undefined; }
function preview(data: Record<string, unknown>): string[] {
  return items(data.content).flatMap(block => {
    if (block.type === "text") return text(block.text) ? [text(block.text)] : [];
    if (block.type === "terminal") return text(block.command) ? [text(block.command)] : [];
    if (block.type === "diff") {
      if (typeof block.diff === "string") return [block.diff];
      if (typeof block.oldText === "string" && typeof block.newText === "string") return [`${text(block.path)}\nBefore:\n${block.oldText}\nAfter:\n${block.newText}`];
    }
    return [];
  });
}

// Derive pending requests from durable replay. Acceptance does not resolve a
// request; only the matching resolution does. Old turns never regain controls.
export function pendingApprovals(events: InteractiveEvent[], activeTurnId?: string): ApprovalRequest[] {
  if (!activeTurnId) return [];
  const pending = new Map<string, ApprovalRequest>();
  for (const event of events) {
    if (event.turnId !== activeTurnId) continue;
    const data = event.data;
    const id = text(data.requestId);
    if (!id) continue;
    if (event.type === "permission_resolved" || event.type === "question_resolved") { pending.delete(id); continue; }
    if (event.type === "permission_request") {
      const options = items(data.options).flatMap(option => {
        const optionId = text(option.optionId);
        return optionId === "allow_once" || optionId === "reject_once" ? [{optionId, label: text(option.label) || optionId}] as PermissionOption[] : [];
      });
      let content = preview(data);
      if (!content.length && data.toolCallId) {
        content = events.filter(tool => tool.turnId === activeTurnId && tool.data.toolCallId === data.toolCallId).flatMap(tool => preview(tool.data));
      }
      pending.set(id, {kind: "permission", requestId: id, turnId: activeTurnId, sequence: event.sequence, title: text(data.title) || "Permission request", description: text(data.description), preview: content, options});
    } else if (event.type === "question") {
      const questions: Question[] = items(data.questions).filter(question => text(question.questionId) && text(question.prompt)).map(question => ({
        id: text(question.questionId), prompt: text(question.prompt), multiSelect: question.allowMultiple === true, allowFreeText: question.allowFreeText === true,
        options: items(question.options).filter(option => text(option.id) && text(option.label)).map(option => ({id: text(option.id), label: text(option.label), description: text(option.description)})),
      }));
      pending.set(id, {kind: "question", requestId: id, turnId: activeTurnId, sequence: event.sequence, questions});
    } else if (pending.has(id) && event.type.startsWith("command.")) {
      const request = pending.get(id)!;
      request.pending = event.type !== "command.rejected";
      request.failure = event.type === "command.rejected" ? text(data.reason) || "The driver rejected the response. Review the request and try again." : undefined;
    }
  }
  return [...pending.values()].sort((a, b) => a.sequence - b.sequence);
}

export function driverSettings(events: InteractiveEvent[]): DriverSettings {
  const result: DriverSettings = {models: [], efforts: []};
  let catalog: Record<string, unknown>[] = [];
  function confirmedModel(model: string) {
    if (model !== result.model) result.effort = undefined;
    result.model = model;
  }
  for (const event of events) {
    const data = event.data;
    if (event.type === "session_info" || event.type === "session_started") {
      if (Array.isArray(data.models)) catalog = items(data.models);
      if (typeof data.currentModel === "string") confirmedModel(data.currentModel);
      if (typeof data.currentEffort === "string") result.effort = data.currentEffort;
      if (level(data.level)) result.level = level(data.level);
    } else if (event.type === "mode_changed" && level(data.level)) result.level = level(data.level);
    else if (event.type === "model_changed" && typeof data.model === "string") confirmedModel(data.model);
    else if (event.type === "command.delivered" && typeof data.actual === "string") {
      if (data.commandType === "session.set_model") confirmedModel(data.actual);
      if (data.commandType === "session.set_level" && level(data.actual)) result.level = level(data.actual);
      if (data.commandType === "session.set_effort") result.effort = data.actual;
    }
  }
  result.models = catalog.filter(model => text(model.id)).map(model => ({id: text(model.id), label: text(model.label) || text(model.id)}));
  result.efforts = strings(catalog.find(model => model.id === result.model)?.efforts);
  return result;
}
