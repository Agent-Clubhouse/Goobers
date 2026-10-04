import { useState } from "react";
import type { WorkbenchDocumentFile, WorkbenchEdge, WorkbenchManifest, WorkbenchNodeRef } from "../api/types";

const relationshipWindow = 20;
export function WorkbenchDocumentDetail({ file }: { file: WorkbenchDocumentFile }) {
  return <section className="workbench-detail" aria-label="Document details">
    <h3>{file.path}</h3>
    {file.provenance && <dl>
      <div><dt>Source commit</dt><dd><code>{file.provenance.commit}</code></dd></div>
      <div><dt>Blob</dt><dd><code>{file.provenance.blobId}</code></dd></div>
      <div><dt>Content SHA-256</dt><dd><code>{file.provenance.contentDigest}</code></dd></div>
    </dl>}
    {file.status !== "available" ? <p>{unavailableMessage(file.status)}</p> : <>
      {file.objective && <div className="workbench-objective">
        <h4>{file.objective.title}</h4><p>Explicit objective · <code>{file.objective.schemaVersion}</code></p>
        <dl><div><dt>Stable objective ID</dt><dd><code>{file.objective.objectiveId}</code></dd></div></dl>
        <p>This identity is declared by the source. It does not establish progress or completion.</p>
        <DocumentEdges edges={file.objective.edges ?? []} />
      </div>}
      {file.manifest ? <>
        <h4>Relationship manifest</h4><p><code>{file.manifest.schemaVersion}</code> · Source-authored relationships and aliases.</p>
        <ManifestAliases aliases={file.manifest.aliases ?? []} />
        <DocumentEdges edges={file.manifest.edges} />
      </> : <>
        <h4>{file.objective ? "Objective source content" : "Markdown source content"}</h4>
        {!file.objective && <p>Reference material; this file does not declare an objective identity.</p>}
        <p>Displayed as plain text. Links and embedded content are not fetched.</p>
        <pre className="workbench-source-text workbench-document-text">{file.body || "(Empty document body)"}</pre>
      </>}
    </>}
  </section>;
}

function unavailableMessage(status: string): string {
  switch (status) {
    case "invalid-source": return "The file was read, but its source metadata is invalid. Content and relationships are unavailable.";
    case "oversized": return "The file exceeds the source read limit. Content and relationships are unavailable.";
    default: return "The configured file could not be read. This does not establish that it was deleted.";
  }
}
function DocumentEdges({ edges }: { edges: WorkbenchEdge[] }) {
  const [offset, setOffset] = useState(0);
  return <section aria-label="Authored relationships">
    <h4>Authored relationships ({edges.length})</h4>
    <p>Direction is preserved from the source. Referenced content is not loaded; each target needs its own access check.</p>
    {edges.length === 0 ? <p>No relationships declared in this file.</p> : <>
      <ol className="workbench-relations" start={offset + 1}>{edges.slice(offset, offset + relationshipWindow).map((edge) => <li key={edge.edgeId}>
        <strong>{edgeLabel(edge.kind)}</strong> · <code>{edge.edgeId}</code>
        <dl><div><dt>From</dt><dd><NodeIdentity value={edge.from} /></dd></div><div><dt>To</dt><dd><NodeIdentity value={edge.to} /></dd></div></dl>
        {edge.rationale && <p>{edge.rationale}</p>}
      </li>)}</ol>
      <WindowButtons label="relationships" offset={offset} count={edges.length} change={setOffset} />
    </>}
  </section>;
}
function ManifestAliases({ aliases }: { aliases: NonNullable<WorkbenchManifest["aliases"]> }) {
  const [offset, setOffset] = useState(0);
  return <section aria-label="Source aliases">
    <h4>Aliases ({aliases.length})</h4><p>Aliases name existing identities; they do not create objectives or grant access.</p>
    {aliases.length === 0 ? <p>No aliases declared.</p> : <>
      <dl>{aliases.slice(offset, offset + relationshipWindow).map((alias) => <div key={alias.name}><dt>{alias.name}</dt><dd><NodeIdentity value={alias.target} /></dd></div>)}</dl>
      <WindowButtons label="aliases" offset={offset} count={aliases.length} change={setOffset} />
    </>}
  </section>;
}
function NodeIdentity({ value }: { value: WorkbenchNodeRef }) { return <code>{value.gaggleId} / {value.sourceBindingId} / {value.kind} / {value.sourceId}</code>; }
function WindowButtons({ label, offset, count, change }: { label: string; offset: number; count: number; change: (value: number) => void }) {
  return <div className="workbench-actions">
    <p>{offset + 1}–{Math.min(offset + relationshipWindow, count)} of {count} {label}.</p>
    {offset > 0 && <button type="button" onClick={() => change(offset - relationshipWindow)}>Previous {label}</button>}
    {offset + relationshipWindow < count && <button type="button" onClick={() => change(offset + relationshipWindow)}>Next {label}</button>}
  </div>;
}
function edgeLabel(kind: WorkbenchEdge["kind"]): string {
  const labels: Record<WorkbenchEdge["kind"], string> = { "parent-of": "Parent of", "blocked-by": "Blocked by", "contributes-to": "Contributes to", references: "References", "milestone-member": "Milestone member", "implemented-by": "Implemented by" };
  return labels[kind] ?? kind;
}
