import type { MetadataChangeRequest, MetadataPreview, MetadataProposalCommand, MetadataRevision, SourceView, WorkbenchDocumentFile, WorkbenchEdge, WorkbenchNodeRef } from "../api/types";

export type MetadataEditMode = "title" | "description" | "relationship-add" | "relationship-remove" | "assign-objective" | "alias-add" | "alias-remove";
export interface EditableAlias { name: string; target: WorkbenchNodeRef }
export type EditableEdge = WorkbenchEdge & { kind: "references" | "contributes-to" };
export function metadataModes(source: SourceView, file: WorkbenchDocumentFile): MetadataEditMode[] {
  if (file.status !== "available" || !file.provenance) return [];
  const modes: MetadataEditMode[] = [];
  if (source.kind === "documents") {
    if (!file.objective && source.writeMetadata?.includes("assign-objective")) modes.push("assign-objective");
    if (file.objective && source.writeFields?.includes("title")) modes.push("title");
    if (source.writeFields?.includes("description")) modes.push("description");
  }
  if ((file.objective || file.manifest) && source.writeRelationships?.some((kind) => kind === "references" || kind === "contributes-to")) {
    modes.push("relationship-add");
    if (editableEdges(source, file).length > 0) modes.push("relationship-remove");
  }
  if (source.kind === "relationships" && file.manifest && source.writeMetadata?.includes("aliases")) {
    modes.push("alias-add"); if ((file.manifest.aliases?.length ?? 0) > 0) modes.push("alias-remove");
  }
  return modes;
}
export function editableEdges(source: SourceView, file: WorkbenchDocumentFile): EditableEdge[] {
  return (file.objective?.edges ?? file.manifest?.edges ?? []).filter((edge): edge is EditableEdge => (edge.kind === "references" || edge.kind === "contributes-to") && !!source.writeRelationships?.includes(edge.kind));
}
export function metadataFieldText(file: WorkbenchDocumentFile, mode: MetadataEditMode): string { return mode === "assign-objective" ? "" : mode === "title" ? file.objective?.title ?? "" : file.body ?? ""; }
export function sameMetadataRevision(a: MetadataRevision, b: MetadataRevision): boolean { return a.commit === b.commit && a.blobId === b.blobId && a.contentDigest === b.contentDigest; }
export function sameMetadataPreview(value: MetadataPreview, request: MetadataChangeRequest): boolean {
  return value.path === request.path && sameMetadataRevision(value.expected, request.expected) && [value.operationDigest, value.targetDigest, value.proposedContentDigest].every((digest) => /^[a-f0-9]{64}$/.test(digest));
}
export function sameMetadataCommand(value: MetadataProposalCommand, gaggle: string, binding: string, preview: MetadataPreview): boolean {
  return /^workbench-[a-f0-9]{32}$/.test(value.id) && value.gaggle === gaggle && value.sourceBindingId === binding && value.path === preview.path && sameMetadataRevision(value.expected, preview.expected) && value.operationDigest === preview.operationDigest && (!value.proposedContentDigest || value.proposedContentDigest === preview.proposedContentDigest);
}
export function refText(ref: WorkbenchNodeRef): string { return `${ref.sourceBindingId} / ${ref.kind} / ${ref.sourceId}`; }
export function metadataPRLink(raw: string, source: SourceView): string | undefined {
  try {
    const url = new URL(raw);
    if (url.protocol !== "https:" || url.username || url.password || url.port || url.search || url.hash) return;
    const parts = url.pathname.split("/").slice(1).map(decodeURIComponent);
    if (source.provider === "github" && url.hostname === "github.com" && parts.length === 4 && parts[0] === source.owner && parts[1] === source.repository && parts[2] === "pull" && /^[1-9][0-9]*$/.test(parts[3])) return url.href;
    if (source.provider === "ado" && url.hostname === "dev.azure.com" && parts.length === 6 && parts[0] === source.owner && parts[1] === source.project && parts[2] === "_git" && parts[3] === source.repository && parts[4] === "pullrequest" && /^[1-9][0-9]*$/.test(parts[5])) return url.href;
  } catch { /* Unverified links remain plain text. */ }
}
