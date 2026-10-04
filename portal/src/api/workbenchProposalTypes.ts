import type { WorkbenchEdge } from "./workbenchDocumentTypes";

export interface MetadataRevision { commit: string; blobId: string; contentDigest: string }
export type MetadataChangeRequest = {
  path: string;
  expected: MetadataRevision;
} & ({ field: "title" | "description"; value: string; relationship?: never; objective?: never; alias?: never } | {
  relationship: { action: "add" | "remove"; edge: WorkbenchEdge & { kind: "references" | "contributes-to" } };
  field?: never; value?: never; objective?: never; alias?: never;
} | {
  objective: { objectiveId: string; title: string };
  field?: never; value?: never; relationship?: never; alias?: never;
} | {
  alias: { action: "add" | "remove"; alias: { name: string; target: WorkbenchEdge["to"] } };
  field?: never; value?: never; relationship?: never; objective?: never;
});
export type MetadataObjectiveRequest = Extract<MetadataChangeRequest, { objective: unknown }>;
export type MetadataAliasRequest = Extract<MetadataChangeRequest, { alias: unknown }>;
export interface MetadataPreview {
  path: string;
  expected: MetadataRevision;
  targetDigest: string;
  operationDigest: string;
  proposedContentDigest: string;
  changed: boolean;
  before: string;
  after: string;
}
export interface MetadataProposalPR { id: string; number: number; url: string }
export interface MetadataProposalPhase {
  name: "tree" | "commit" | "branch" | "pull-request";
  outcome: "pending" | "acknowledged" | "unknown" | "not-attempted";
  claimedAt: string;
  finishedAt?: string;
  treeId?: string;
  commitId?: string;
  pullRequest?: MetadataProposalPR;
}
export interface MetadataProposalObservation {
  phase: number;
  at: string;
  found: boolean;
  matches: boolean;
  treeId?: string;
  commitId?: string;
  pullRequest?: MetadataProposalPR;
}
export interface MetadataProposalCommand {
  id: string;
  gaggle: string;
  sourceBindingId: string;
  actor: { issuer: string; subject: string };
  path: string;
  state: "accepted" | "prepared" | "attempting" | "unknown" | "blocked" | "confirmed" | "observed" | "not-applied";
  duplicate: boolean;
  requestDigest: string;
  operationDigest: string;
  expected: MetadataRevision;
  proposedContentDigest?: string;
  branch?: string;
  acceptedAt: string;
  completedAt?: string;
  phases: MetadataProposalPhase[];
  observations: MetadataProposalObservation[];
  omittedObservations: number;
  nextAction: string;
}
