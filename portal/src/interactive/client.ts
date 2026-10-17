export interface Surface {
  id: string; name: string; description: string; mode: "helper" | "workbench";
  enabled: boolean; maxChildren: number; instructions?: string;
}
export interface Turn {
  id: string; text: string; status: string; attempt: number; fence: number;
  startedAt: string; finishedAt?: string; error?: string;
}
export interface Child {
  id: string; prompt: string; status: string; result?: string;
  requestedAt: string; reviewedAt?: string;
}
export interface Change { path: string; status: string; additions: number; deletions: number }
export interface Session {
  id: string; owner: string; surfaceId: string; mode: "helper" | "workbench";
  title: string; status: string; sequence: number; fence: number; createdAt: string;
  updatedAt: string; activeTurnId?: string; turns: Turn[]; children: Child[];
  changes: Change[]; diff?: string; workspacePath?: string; recoveryReason?: string;
}
export interface InteractiveEvent {
  sequence: number; sessionId: string; turnId?: string; commandId?: string;
  type: string; time: string; data: Record<string, unknown>;
}
export interface Command {
  commandId: string; type: "message" | "steer" | "stop" | "resume" | "child.request" | "child.review";
  expectedSequence: number; expectedFence: number; text?: string; turnId?: string;
  childId?: string; decision?: "approve" | "accept" | "deny";
}
export interface Receipt {
  commandId: string; sessionId: string; type: string; status: string;
  sequence: number; turnId?: string; childId?: string;
}
export interface Changes { changes: Change[]; diff: string; workspacePath: string }
export interface Connection { id: string; provider: string; label: string; status: string; resources: string[]; expiresAt?: string }
export interface RuntimeStatus {
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
  create(input: {commandId: string; surfaceId: string; title?: string}): Promise<Session>;
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
        method: body === undefined ? "GET" : "POST", credentials: "same-origin",
        headers: { Accept: "application/json", ...(body === undefined ? {} : { "Content-Type": "application/json" }) },
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
  create(input: {commandId: string; surfaceId: string; title?: string}) { return this.request<Session>("/sessions", input); }
  session(id: string) { return this.request<Session>(`/sessions/${encodeURIComponent(id)}`); }
  async events(id: string, after: number) { return (await this.request<{events: InteractiveEvent[]}>(`/sessions/${encodeURIComponent(id)}/events?after=${after}`)).events ?? []; }
  changes(id: string) { return this.request<Changes>(`/sessions/${encodeURIComponent(id)}/changes`); }
  command(id: string, input: Command) { return this.request<Receipt>(`/sessions/${encodeURIComponent(id)}/commands`, input); }
  subscribe(id: string, after: number, onEvent: (event: InteractiveEvent) => void, onState: (state: StreamState) => void): StreamSubscription {
    if (typeof EventSource === "undefined") { onState("reconnecting"); return { close() {} }; }
    const stream = new EventSource(`${this.base}/sessions/${encodeURIComponent(id)}/events?after=${after}`, { withCredentials: true });
    stream.onopen = () => onState("live");
    stream.onerror = () => onState("reconnecting");
    stream.addEventListener("interactive", event => {
      try { onEvent(JSON.parse((event as MessageEvent<string>).data) as InteractiveEvent); }
      catch { onState("reconnecting"); }
    });
    return { close: () => stream.close() };
  }
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
