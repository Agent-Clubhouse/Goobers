import type { SourceView, WorkbenchDocumentFile, WorkbenchNodeRef } from "../api/types";
import type { EditableEdge } from "./workbenchMetadataEditing";

export function MetadataRelationshipInput({ gaggle, source, sources, file, edge, change }: { gaggle: string; source: SourceView; sources: SourceView[]; file: WorkbenchDocumentFile; edge: EditableEdge; change: (value: EditableEdge) => void }) {
  const kinds = ["references", "contributes-to"].filter((kind) => source.writeRelationships?.includes(kind));
  return <>
    <label>Relationship kind<select value={edge.kind} onChange={(event) => change({ ...edge, kind: event.target.value as EditableEdge["kind"] })}>{kinds.map((kind) => <option key={kind}>{kind}</option>)}</select></label>
    {file.ref ? <p>From objective: <code>{file.ref.sourceId}</code></p> : <NodeInput label="From" gaggle={gaggle} sources={sources.filter((item) => item.kind === "backlog")} value={edge.from} change={(from) => change({ ...edge, from })} workItemOnly />}
    <NodeInput label="To" gaggle={gaggle} sources={sources.filter((item) => item.kind !== "relationships")} value={edge.to} change={(to) => change({ ...edge, to })} />
    <label>Relationship rationale<textarea maxLength={4096} value={edge.rationale ?? ""} onChange={(event) => { const next = { ...edge }; if (event.target.value) next.rationale = event.target.value; else delete next.rationale; change(next); }} /></label>
    <p>Use the stable source ID shown in the browser. Source references do not grant access to their targets. Native parent, blocker, milestone and PR links use their native source operations.</p>
  </>;
}
function NodeInput({ label, gaggle, sources, value, change, workItemOnly }: { label: string; gaggle: string; sources: SourceView[]; value: WorkbenchNodeRef; change: (ref: WorkbenchNodeRef) => void; workItemOnly?: boolean }) {
  const selected = sources.find((source) => source.bindingId === value.sourceBindingId);
  const kinds = selected?.kind === "documents" ? ["objective-document", "document"] : workItemOnly ? ["work-item"] : ["work-item", "milestone", "pull-request"];
  return <>
    <label>{label} source<select value={selected?.bindingId ?? ""} required onChange={(event) => { const source = sources.find((item) => item.bindingId === event.target.value); change({ gaggleId: gaggle, sourceBindingId: source?.bindingId ?? "", kind: source?.kind === "documents" ? "objective-document" : "work-item", sourceId: "" }); }}><option value="">Select a configured source</option>{sources.map((source) => <option key={source.bindingId} value={source.bindingId}>{source.bindingId}</option>)}</select></label>
    <label>{label} kind<select value={value.kind} onChange={(event) => change({ ...value, kind: event.target.value, sourceId: "" })}>{kinds.map((kind) => <option key={kind}>{kind}</option>)}</select></label>
    <label>{label} stable source ID<input value={value.sourceId} onChange={(event) => change({ ...value, sourceId: event.target.value })} required maxLength={512} /></label>
  </>;
}
