import type { SessionActor } from "./types";

export interface PRRepairObservation { checker: SessionActor; at: string; matches: boolean; commitId?: string }
export interface PRRepairCommand {
 id: string; sourceBindingId: string;
 state: "accepted" | "attempting" | "unknown" | "confirmed" | "not-applied" | "observed-applied";
 requestDigest: string; operationDigest: string; selectedHeadSha: string; expectedHeadSha: string;
 parentCommandId?: string; runId: string; actor: SessionActor; acceptedAt: string; attemptedAt?: string; completedAt?: string;
 receipt?: { operationDigest: string; outcome: "unknown" | "confirmed" | "not-applied"; mutationAttempted: boolean; providerAcknowledged: boolean; observedMatches: boolean; commitId?: string };
 observations?: PRRepairObservation[]; omittedObservations?: number;
}
