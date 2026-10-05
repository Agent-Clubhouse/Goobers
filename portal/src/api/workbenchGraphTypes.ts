import type { NativeTarget, SourceLocator, WorkbenchNodeRef } from "./workbenchTypes";
import type { WorkbenchEdge } from "./workbenchDocumentTypes";

/** Rebuildable authorized observations. Missing data never implies deletion or progress. */
export interface WorkbenchGraph {
  generation: string;
  gaggleId: string;
  nodes: WorkbenchGraphNode[];
  edges: WorkbenchGraphEdge[];
  documents: WorkbenchGraphDocument[];
  aliases: WorkbenchGraphAlias[];
  sources: WorkbenchGraphCoverage[];
  conflicts: WorkbenchGraphConflict[];
  partial: boolean;
}
export interface WorkbenchGraphProvenance { commit: string; blobId: string; contentDigest: string; etag?: string }
export interface WorkbenchGraphObservation {
  contentDigest: string;
  ref: WorkbenchNodeRef;
  title: string;
  type?: string;
  state?: string;
  revision: string;
  locator: SourceLocator;
  objective: boolean;
  path?: string;
  provenance?: WorkbenchGraphProvenance;
}
export interface WorkbenchGraphNode { key: string; observations: WorkbenchGraphObservation[]; conflict: boolean }
export interface WorkbenchGraphEndpoint { ref?: WorkbenchNodeRef; native?: NativeTarget; resolved: boolean }
export interface WorkbenchGraphOwner {
  kind: "native" | "frontmatter" | "manifest";
  sourceBindingId: string;
  path?: string;
  field?: string;
  object?: WorkbenchNodeRef;
}
export interface WorkbenchGraphEdge {
  key: string;
  edgeId?: string;
  kind: WorkbenchEdge["kind"];
  from: WorkbenchGraphEndpoint;
  to: WorkbenchGraphEndpoint;
  rationale?: string;
  origin: "native" | "authored";
  owner: WorkbenchGraphOwner;
  conflict: boolean;
}
export interface WorkbenchGraphDocument {
  sourceBindingId: string;
  path: string;
  status: "available" | "unavailable" | "invalid-source" | "oversized";
  provenance?: WorkbenchGraphProvenance;
  ref?: WorkbenchNodeRef;
}
export interface WorkbenchGraphAlias { name: string; target: WorkbenchGraphEndpoint; owner: WorkbenchGraphOwner }
export interface WorkbenchGraphCoverage {
  sourceBindingId: string;
  kind: "backlog" | "documents" | "relationships";
  status: "complete" | "partial" | "not-read";
  consistency: "commit-pinned" | "native-non-snapshot";
  sourceTargetDigest?: string;
  commit?: string;
  reasons?: string[];
}
export interface WorkbenchGraphConflict {
  kind: "edge-id-conflict" | "edge-owner-conflict" | "native-observation-conflict" | "objective-location-conflict";
  keys: string[];
}
