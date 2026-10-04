export interface StartQueueCancelInput { requestId: string; reason: string }
export interface StartQueueCancellation { requestId: string; actor: string; reason: string; requestedAt: string; state: "requested" | "cancelled-before-dispatch" | "confirmed" | "already-terminal" }
export interface StartQueueItem {
  acceptanceId: string; gaggle: string; workflow: string; source: "manual" | "schedule" | "backlog" | "event" | "child" | "session" | "human-restart" | "direct-engine" | "signal" | "legacy";
  generation: string; acceptedAt: string; deadline?: string; state: "accepted" | "dispatching" | "dispatched" | "rejected";
  waitingReason?: string; runId?: string; disposition?: "cancelled" | "expired"; cancellation?: StartQueueCancellation;
}
export interface StartQueuePage { gaggle: string; items: StartQueueItem[]; nextCursor?: string }
