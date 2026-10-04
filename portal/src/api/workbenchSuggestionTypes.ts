import type { WorkbenchNodeRef } from "./workbenchTypes";
import type { MetadataPreview, MetadataProposalCommand, MetadataRevision } from "./workbenchProposalTypes";

export interface SuggestionSelection { runId: string; sequence: number }
export interface SuggestionArtifact { sequence: number; stageSequence: number; stageId: string; attempt: number; branch: number; name: string; digest: string; bytes: number }
export interface SuggestionInventory { runId: string; artifacts: SuggestionArtifact[]; nextSequence?: number; partial: boolean }
export interface SuggestionEvidence { sourceTargetDigest: string; nativeRevision?: string; nativeLocator?: string; path?: string; repositoryRevision?: MetadataRevision }
export interface SuggestionEndpoint { ref?: WorkbenchNodeRef; evidence?: SuggestionEvidence; creation?: { sourceBindingId: string; requestId: string } }
export interface BoundSuggestion { key: string; proposal: { kind: "references" | "contributes-to" | "parent-of" | "blocked-by" | "milestone-member" | "implemented-by"; from: SuggestionEndpoint; to: SuggestionEndpoint; rationale: string }; origin: { runId: string; stageId: string; attempt: number; artifactPath: string; artifactDigest: string } }
export interface SuggestionCandidate { suggestion: BoundSuggestion; supported: boolean; reason?: string; reviewId?: string; reviewState?: "accepting" | "linked" | "rejected" }
export interface SuggestionBatch { selection: SuggestionSelection; artifact: SuggestionArtifact; candidates: SuggestionCandidate[]; omitted: number }
export interface SuggestionPreviewRequest { selection: SuggestionSelection; key: string }
export interface SuggestionPreview { sourceBindingId: string; preview: MetadataPreview }
export interface SuggestionDecisionRequest { selection: SuggestionSelection; key: string; decision: "accept" | "reject"; reason?: string; expectedOwner?: MetadataRevision; expectedOperationDigest?: string }
export interface SuggestionReview { id: string; suggestion: BoundSuggestion; state: "accepting" | "linked" | "rejected"; decision: "accept" | "reject"; reason?: string; acceptedAt: string; proposal?: MetadataProposalCommand; duplicate: boolean }
