import type { WorkbenchNodeRef } from "./workbenchTypes";

/** Source-authored direction and identity; references do not grant target access. */
export interface WorkbenchEdge {
  edgeId: string;
  kind: "parent-of" | "blocked-by" | "contributes-to" | "references" | "milestone-member" | "implemented-by";
  from: WorkbenchNodeRef;
  to: WorkbenchNodeRef;
  rationale?: string;
}
export interface WorkbenchObjective {
  schemaVersion: "objectives/v1";
  objectiveId: string;
  title: string;
  edges?: WorkbenchEdge[];
}
export interface WorkbenchManifest {
  schemaVersion: "relationships/v1";
  aliases?: Array<{ name: string; target: WorkbenchNodeRef }>;
  edges: WorkbenchEdge[];
}
export interface WorkbenchDocumentFile {
  path: string;
  status: "available" | "unavailable" | "invalid-source" | "oversized";
  provenance?: { commit: string; blobId: string; contentDigest: string; etag?: string };
  ref?: WorkbenchNodeRef;
  body?: string;
  objective?: WorkbenchObjective;
  manifest?: WorkbenchManifest;
}
export interface WorkbenchDocumentPage {
  sourceBindingId: string;
  repository: { provider: "github" | "ado"; owner: string; project?: string; name: string };
  branch: string;
  commit: string;
  sourceTargetDigest: string;
  files: WorkbenchDocumentFile[];
  startOffset: number;
  totalPaths: number;
  nextCursor?: string;
  exhausted: boolean;
  coverage: "complete" | "partial";
  reasons?: string[];
}
/** Only continuation and page size are selectable; paths and revision are configured. */
export interface WorkbenchDocumentPageRequest { cursor?: string; limit?: number }
