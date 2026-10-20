import { humanControlHeaders } from "./humanAuth";
import { subscribeHumanStream } from "./humanStream";

export type ExecutionModeId = "codex-local" | "local-tools";
export interface ExecutionModeOption { id: ExecutionModeId; label: string; available: boolean; reason?: string; liveQualified: boolean }
export interface CreateSessionRequest { commandId: string; surfaceId: string; title?: string; executionMode: ExecutionModeId }
export interface Surface {
  id: string; name: string; description: string; mode: "helper" | "workbench";
  enabled: boolean; maxChildren: number; instructions?: string;
  configRevision?: string; gaggle?: string; goober?: string; model?: string; harness?: string;
  tools?: string[]; executionModes?: ExecutionModeId[]; defaultExecutionMode?: ExecutionModeId;
  maxTurnSeconds?: number; leaseSeconds?: number; maxActive?: number; maxActivePerOwner?: number;
}
export interface Turn {
  id: string; text: string; status: string; attempt: number; fence: number;
  startedAt: string; finishedAt?: string; error?: string;
}
export interface Child {
  lease?: Lease; executionKind?: string; readOnly?: boolean; workflowYaml?: string; workflowDigest?: string;
  id: string; prompt: string; status: string; result?: string;
  requestedAt: string; reviewedAt?: string;
}
export interface Change { path: string; status: string; additions: number; deletions: number }
export interface Lease { workerId: string; leaseId: string; fence: number; expiresAt: string; status: string }
export type PermissionLevel = "plan" | "ask" | "auto_edit" | "auto" | "full_access";
export type SwitchTiming = "live" | "per_turn" | "restart" | "none";
export interface ChatCapabilities {
  steer: boolean; queue: "native" | "client" | "none"; interrupt: boolean;
  questions?: boolean; permissionExtras?: {denyMessage?: boolean}; liveLevels?: PermissionLevel[]; modelSwitch?: SwitchTiming; effortSwitch?: SwitchTiming;
}
export interface Session {
  capabilities?: ChatCapabilities;
  lease?: Lease; configDigest?: string; configRevision?: string; members?: {principal: string; role: string}[]; custodyPending?: boolean; stopFencePending?: boolean;
  id: string; owner: string; surfaceId: string; mode: "helper" | "workbench";
  executionMode?: ExecutionModeId; title: string; status: string; sequence: number; fence: number; createdAt: string;
  updatedAt: string; activeTurnId?: string; turns: Turn[]; children: Child[];
  changes: Change[]; diff?: string; workspacePath?: string; recoveryReason?: string;
}
export interface InteractiveEvent {
  sequence: number; sessionId: string; turnId?: string; commandId?: string;
  type: string; time: string; data: Record<string, unknown>;
}
export interface Command {
  commandId: string; type: "message" | "steer" | "stop" | "resume" | "child.request" | "child.review" | "permission.respond" | "question.answer" | "session.set_model" | "session.set_level" | "session.set_effort";
  expectedSequence: number; expectedFence: number; text?: string; turnId?: string;
  requestId?: string; message?: string; optionId?: "allow_once" | "reject_once"; answers?: Record<string, string[]>;
  model?: string; level?: PermissionLevel; effort?: string;
  delivery?: "prompt" | "steer" | "queued"; steeringMode?: "interrupt-and-continue";
  workflowYaml?: string; childId?: string; decision?: "approve" | "accept" | "deny";
}
export interface Receipt {
  requestId?: string; actual?: string;
  commandId: string; sessionId: string; type: string; status: string;
  sequence: number; turnId?: string; childId?: string; delivery?: string; steeringMode?: string;
}
export interface Changes { changes: Change[]; diff: string; workspacePath: string }
export interface Connection { id: string; provider: string; label: string; status: string; resources: string[]; expiresAt?: string }
export interface InteractiveShellStatus { prototype: boolean; name: string; connection: string; worker: string }
export interface RuntimeStatus {
  executionModes?: ExecutionModeOption[]; defaultMode?: ExecutionModeId;
  localActors?: string[]; providerMode: string; executionMode: string; identity: {name: string; subject: string; issuer: string};
  connections: Connection[]; limits: {maxTurnSeconds: number; maxChildren: number};
  capabilities: {liveModel: boolean; liveDelegatedAuth: boolean};
}
export interface StreamSubscription { close(): void }
export type StreamState = "connecting" | "live" | "reconnecting";
export interface InteractiveClient {
  selectIdentity(name: string): Promise<unknown>;
  status(): Promise<RuntimeStatus>;
  connectionAction(id: string, action: "expire" | "reauthenticate" | "revoke"): Promise<unknown>;
  surfaces(): Promise<Surface[]>;
  sessions(): Promise<Session[]>;
  create(input: CreateSessionRequest): Promise<Session>;
  session(id: string): Promise<Session>;
  events(id: string, after: number): Promise<InteractiveEvent[]>;
  changes(id: string): Promise<Changes>;
  command(id: string, input: Command): Promise<Receipt>;
  subscribe(id: string, after: number, onEvent: (event: InteractiveEvent) => void, onState: (state: StreamState) => void): StreamSubscription;
}
export class InteractiveError extends Error {
  constructor(message: string, readonly code: string, readonly status: number) { super(message); }
}
export class HttpInteractiveClient implements InteractiveClient {
  constructor(private readonly base = "/api/v1/interactive", private readonly fetcher: typeof fetch = (...args) => fetch(...args)) {}
  private async request<T>(path: string, body?: unknown): Promise<T> {
    let response: Response;
    try {
      response = await this.fetcher(this.base + path, {
        method: body === undefined ? "GET" : "POST", credentials: "same-origin", redirect: "error",
        signal: typeof AbortSignal.timeout === "function" ? AbortSignal.timeout(30000) : undefined,
        headers: { Accept: "application/json", ...humanControlHeaders(), ...(body === undefined ? {} : { "Content-Type": "application/json" }) },
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      });
    } catch { throw new InteractiveError("The server response was lost. The outcome is unknown; refresh before retrying the same command.", "network", 0); }
    const payload = await response.json().catch(() => undefined);
    if (!response.ok) throw new InteractiveError(payload?.error?.message ?? `Interactive service returned HTTP ${response.status}.`, payload?.error?.code ?? "unavailable", response.status);
    if (payload === undefined) throw new InteractiveError("The server returned an unreadable response. Refresh to check whether the command was accepted.", "invalid_response", 0);
    return payload as T;
  }
  selectIdentity(name: string) { return this.request<unknown>("/identity/select", { name }); }
  status() { return this.request<RuntimeStatus>("/status"); }
  connectionAction(id: string, action: "expire" | "reauthenticate" | "revoke") { return this.request<unknown>(`/connections/${encodeURIComponent(id)}/${action}`, {}); }
  async surfaces() { return (await this.request<{surfaces: Surface[]}>("/surfaces")).surfaces ?? []; }
  async sessions() { return (await this.request<{sessions: Session[]}>("/sessions")).sessions ?? []; }
  create(input: CreateSessionRequest) { return this.request<Session>("/sessions", input); }
  session(id: string) { return this.request<Session>(`/sessions/${encodeURIComponent(id)}`); }
  async events(id: string, after: number) { return (await this.request<{events: InteractiveEvent[]}>(`/sessions/${encodeURIComponent(id)}/events?after=${after}`)).events?.map(normalizeEvent) ?? []; }
  changes(id: string) { return this.request<Changes>(`/sessions/${encodeURIComponent(id)}/changes`); }
  command(id: string, input: Command) { return this.request<Receipt>(`/sessions/${encodeURIComponent(id)}/commands`, input); }
  subscribe(id: string, after: number, onEvent: (event: InteractiveEvent) => void, onState: (state: StreamState) => void): StreamSubscription {
    return subscribeHumanStream(`${this.base}/sessions/${encodeURIComponent(id)}/events`, after, this.fetcher, event => onEvent(normalizeEvent(event)), onState);
  }
}
// Accept the durable host envelope and standalone v2 envelopes at one boundary.
// Host sequence wins over the driver's per-attempt sequence when both exist.
export function normalizeEvent(input: InteractiveEvent | Record<string, unknown>): InteractiveEvent {
  const value = input as unknown as Record<string, unknown>;
  const data = (value.data ?? {}) as Record<string, unknown>;
  const chat = (data.chat ?? (value.v === 2 ? value : undefined)) as Record<string, unknown> | undefined;
  if (!chat) return input as InteractiveEvent;
  const payload = (chat.data ?? {}) as Record<string, unknown>;
  const text = Array.isArray(payload.content) ? payload.content.map(block => block?.type === "text" && typeof block.text === "string" ? block.text : "").join("") : undefined;
  const type = String(chat.type);
  const mapped = type === "agent_message_chunk" ? "assistant.delta" : type === "agent_message" ? "assistant.message"
    : type === "tool_call" || type === "tool_call_update" ? ["completed", "failed", "cancelled"].includes(String(payload.status)) ? "tool.completed" : type === "tool_call" ? "tool.started" : "tool.output" : type;
  return {sequence: Number(value.sequence ?? chat.seq), sessionId: String(value.sessionId ?? chat.sessionId), turnId: (value.turnId ?? chat.turnId) as string | undefined,
    commandId: (value.commandId ?? chat.commandId) as string | undefined, time: typeof value.time === "string" ? value.time : new Date(Number(chat.ts)).toISOString(), type: mapped,
    data: {...payload, ...data, ...(text !== undefined && typeof data.text !== "string" ? {text} : {}), ...(payload.title ? {name: payload.title} : {})}};
}
export function commandId(): string { return crypto.randomUUID(); }
export function mergeEvents(current: InteractiveEvent[], incoming: InteractiveEvent[]): InteractiveEvent[] {
  const bySequence = new Map(current.map(event => [event.sequence, event]));
  for (const event of incoming) bySequence.set(event.sequence, event);
  return [...bySequence.values()].sort((a, b) => a.sequence - b.sequence);
}
export function describeError(error: unknown): string { return error instanceof Error ? error.message : "The request could not be completed."; }
export function isUncertain(error: unknown): boolean { return error instanceof InteractiveError && error.status === 0; }

// A request is persisted before dispatch so reload cannot turn a lost response
// into a fresh command. No credentials or server tokens are stored here.
export function readPending<T>(key: string): T | undefined {
  try { const value = sessionStorage.getItem(`goobers.interactive.${key}`); return value ? JSON.parse(value) as T : undefined; }
  catch { return undefined; }
}
export function writePending(key: string, value: unknown): void {
  try { if (value === undefined) sessionStorage.removeItem(`goobers.interactive.${key}`); else sessionStorage.setItem(`goobers.interactive.${key}`, JSON.stringify(value)); }
  catch { /* Private browsing may disallow storage; in-memory recovery still works. */ }
}
