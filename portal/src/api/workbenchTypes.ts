/** Provider-native projections. A locator or link never grants access to its target. */
export interface WorkbenchNodeRef { gaggleId: string; sourceBindingId: string; kind: string; sourceId: string }
export interface SourceLocator { id: string; url?: string }
export interface NativeTarget { ref?: WorkbenchNodeRef; kind: string; stableId?: string; locator: SourceLocator }
export interface NativeRelationship { kind: string; incoming?: boolean; target: NativeTarget }
export type RelationshipCoverageState = "complete" | "partial" | "not-loaded" | "unsupported";
export interface RelationshipCoverage { parents: RelationshipCoverageState; blockers: RelationshipCoverageState; milestones: RelationshipCoverageState }
export interface BacklogItem {
  ref: WorkbenchNodeRef; locator: SourceLocator; revision?: string; revisionSemantics: string;
  type: string; title: string; description?: string; acceptanceCriteria?: string; state: string;
  labels?: string[]; assignees?: string[]; objective: boolean; updatedAt?: string;
  relationships?: NativeRelationship[]; relationshipCoverage: RelationshipCoverage;
}
export interface BacklogPage {
  items: BacklogItem[]; nextCursor?: string; exhausted: boolean; partial: boolean; reasons?: string[];
  candidates: number; omitted: number; sourceTargetDigest: string;
}
export interface BacklogPageRequest { cursor?: string; limit?: number }
export interface BacklogItemRequest { id: string; expectedSourceId?: string }
export interface SourceView {
  bindingId: string; kind: string; provider: string; owner: string; project?: string; repository?: string;
  branch?: string; paths?: string[]; writeFields?: string[]; writeRelationships?: string[];
}
export interface WorkbenchSourcePage { items: SourceView[]; generation: string }
