import type { ChatCapabilities, InteractiveEvent, Session } from "./client";
// Diff parsing adapted from Agent-Clubhouse/Clubhouse at 3309665c,
// src/renderer/features/agents/chat/view-model.ts (MIT; see CLUBHOUSE-LICENSE.txt).
export interface DiffLine {
  type: 'add' | 'del' | 'ctx' | 'hunk';
  text: string;
  oldNo?: number;
  newNo?: number;
}

export function parseUnifiedDiff(raw: string): DiffLine[] {
  const out: DiffLine[] = [];
  let oldNo = 0;
  let newNo = 0;
  for (const line of raw.split('\n')) {
    if (line.startsWith('---') || line.startsWith('+++') || line.startsWith('diff ') || line.startsWith('index ')) continue;
    if (line.startsWith('@@')) {
      const m = line.match(/@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/);
      if (m) { oldNo = Number(m[1]); newNo = Number(m[2]); }
      out.push({ type: 'hunk', text: line });
    } else if (line.startsWith('+')) {
      out.push({ type: 'add', text: line.slice(1), newNo: newNo++ });
    } else if (line.startsWith('-')) {
      out.push({ type: 'del', text: line.slice(1), oldNo: oldNo++ });
    } else if (line.startsWith(' ')) {
      out.push({ type: 'ctx', text: line.slice(1), oldNo: oldNo++, newNo: newNo++ });
    }
  }
  return out;
}

export function diffStats(lines: DiffLine[]): { added: number; removed: number } {
  let added = 0;
  let removed = 0;
  for (const l of lines) {
    if (l.type === 'add') added++;
    else if (l.type === 'del') removed++;
  }
  return { added, removed };
}


export function fileDiffs(raw: string) {
  return raw.split(/(?=^diff --git )/m).filter(Boolean).map((text, index) => {
    const header = text.match(/^\+\+\+ (?:b\/)?(.+)$/m);
    const old = text.match(/^--- (?:a\/)?(.+)$/m);
    const path = header?.[1] === "/dev/null" ? old?.[1] : header?.[1];
    const lines = parseUnifiedDiff(text);
    return { path: path ?? `Changes ${index + 1}`, raw: text, lines, ...diffStats(lines) };
  });
}

// Clubhouse's ErrorRow/ERROR_HINTS and plainFailure pattern: explain recovery
// first, retain technical diagnostics behind an explicit disclosure.
const ERROR_HINTS: Record<string, string> = {
  auth_required: "Provider access needs authorization. Review the connection, then resume the session.",
  connection_expired: "The provider connection expired. Reauthenticate it, then resume the session.",
  connection_revoked: "Provider access was revoked. Reauthentication cannot restore this grant.",
  authority_expired: "This session's access grant expired. Start a new session with current access.",
  authority_stopped: "This session's access grant has stopped. Start a new session to continue.",
  rate_limited: "The provider is busy. Wait before trying again.",
  auth_fence_pending: "Access cancellation is still pending. Retry stopping the session.",
};
export function plainFailure(data: Record<string, unknown>, fallback = "Provider access needs attention. Review the connection before resuming.") {
  const reason = data.reason ?? data.message ?? data.error;
  const value = typeof reason === "object" && reason ? reason as Record<string, unknown> : data;
  const code = String(value.code ?? data.code ?? "");
  const text = typeof reason === "string" ? reason : typeof value.message === "string" ? value.message : "";
  const hint = ERROR_HINTS[code] ?? Object.entries(ERROR_HINTS).find(([key]) => text.includes(key))?.[1];
  const raw = !text || /^[{[]/.test(text.trim()) || /\b(?:Error:|ENOENT|stack|traceback)\b/.test(text);
  return { text: hint ?? (raw ? fallback : text), detail: JSON.stringify(data, null, 2) };
}

export interface ToolActivity { id: string; name: string; events: InteractiveEvent[]; readOnly: boolean; path?: string }
export function toolActivities(events: InteractiveEvent[]): ToolActivity[] {
  const groups: ToolActivity[] = [];
  const open = new Map<string, ToolActivity>();
  for (const event of events.filter(event => event.type.startsWith("tool."))) {
    const name = String(event.data.name ?? event.data.tool ?? "Tool");
    const explicit = event.data.toolCallId ?? event.data.callId;
    const identity = String(explicit ?? name);
    let group = open.get(identity);
    if (!group || (event.type === "tool.started" && !explicit)) {
      group = {id: String(explicit ?? event.sequence), name, events: [], readOnly: event.data.kind === "read" || /^(repository\.read|read_file|read|glob|grep|search)$/i.test(name), path: typeof event.data.path === "string" ? event.data.path : undefined};
      groups.push(group); open.set(identity, group);
    }
    group.events.push(event);
    if (!group.path && typeof event.data.path === "string") group.path = event.data.path;
    if (event.type === "tool.completed") open.delete(identity);
  }
  return groups;
}

export interface MessageDelivery { commandId: string; text: string; turnId?: string; state: string; delivery?: string }
export function commandMessages(events: InteractiveEvent[], local: MessageDelivery[]): MessageDelivery[] {
  const messages = new Map(local.map(message => [message.commandId, {...message}]));
  for (const event of events) {
    if (!event.commandId) continue;
    const current = messages.get(event.commandId);
    if (event.type === "command.accepted" && event.data.commandType !== "message" && typeof event.data.text === "string" && event.data.text) {
      messages.set(event.commandId, {...current, commandId: event.commandId, text: event.data.text, turnId: event.turnId, state: "Accepted"});
    } else if (event.type === "user_message" && event.data.delivery !== "prompt" && typeof event.data.text === "string") {
      messages.set(event.commandId, {...current, commandId: event.commandId, text: event.data.text, turnId: event.turnId, state: "Delivered", delivery: String(event.data.delivery)});
    } else if (current && /^(steering|command)\.(delivered|rejected)$/.test(event.type)) {
      current.state = event.type.endsWith("delivered") ? "Delivered" : "Rejected";
      current.delivery = typeof event.data.delivery === "string" ? event.data.delivery : current.delivery;
    }
  }
  return [...messages.values()];
}

export type ComposerAction = "message" | "steer" | "interrupt_continue" | "queued";
// Capability precedence follows Clubhouse view-model.composerModes (MIT).
// Unsupported actions stay unavailable rather than becoming a silent queue.
export function composerModes(caps: ChatCapabilities | undefined, running: boolean): { primary?: ComposerAction; secondary?: ComposerAction } {
  if (!running) return {primary: "message"};
  const canQueue = caps?.queue === "native" || caps?.queue === "client";
  if (caps?.steer) return {primary: "steer", secondary: canQueue ? "queued" : undefined};
  if (caps?.interrupt) return {primary: "interrupt_continue", secondary: canQueue ? "queued" : undefined};
  return {primary: canQueue ? "queued" : undefined};
}
export function sessionCapabilities(session: Session | undefined, events: InteractiveEvent[]): ChatCapabilities | undefined {
  if (!session) return undefined;
  for (let i = events.length - 1; i >= 0; i--) {
    if (!["session_started", "session_info"].includes(events[i].type)) continue;
    const caps = events[i].data.capabilities as ChatCapabilities | undefined;
    if (caps) return caps;
  }
  return session.capabilities ?? (session.executionMode === "local-tools" ? {steer: true, queue: "none", interrupt: false} : undefined);
}
export const composerLabels: Record<ComposerAction, string> = {message: "Send message", steer: "Steer", interrupt_continue: "Stop & send", queued: "Queue"};
