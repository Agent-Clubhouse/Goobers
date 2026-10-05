import type { BacklogItem } from "./workbenchTypes";

export type BacklogWriteField = "title" | "description" | "state" | "labels" | "assignees";
export interface BacklogPatchInput {
  sourceId: string; expectedRevision: string; field: BacklogWriteField;
  value?: string; values?: string[];
}
export interface BacklogWriteCapabilities {
  fields: BacklogWriteField[]; relationships: string[];
  revisionSemantics: "timestamp-preflight" | "atomic-revision-test";
  maxAssignees: number; controlLabelChanges: boolean;
}
export interface BacklogPatchReceipt {
  operationDigest: string; outcome: "confirmed" | "not-applied" | "unknown";
  revisionSemantics: "timestamp-preflight" | "atomic-revision-test";
  providerAcknowledged: boolean; observedMatches: boolean; observed?: BacklogItem;
}
/** Actor-scoped durable evidence. Matching observation alone does not establish authorship. */
export interface BacklogEditCommand {
  id: string; gaggle: string; sourceBindingId: string; actor: { issuer: string; subject: string };
  itemId: string; sourceId: string; field: BacklogWriteField;
  state: "accepted" | "attempting" | "confirmed" | "not-applied" | "unknown";
  duplicate: boolean; requestDigest: string; operationDigest: string;
  acceptedAt: string; attemptedAt?: string; completedAt?: string;
  receipt?: BacklogPatchReceipt; nextAction: string;
}
