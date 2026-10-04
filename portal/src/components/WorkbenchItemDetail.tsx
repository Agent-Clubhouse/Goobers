import { useEffect, useState } from "react";
import { WorkbenchBlockerResolution } from "./WorkbenchBlockerResolution";
import { WorkbenchItemEditor } from "./WorkbenchItemEditor";
import type { BacklogItem, DaemonClient, Goober, NativeRelationship, RelationshipCoverageState, SourceView } from "../api/types";

type ItemSelection = Pick<BacklogItem, "ref" | "locator">;

export function WorkbenchItemDetail({ client, gaggle, selected, sources, unavailable, goobers = [] }: { client: DaemonClient; gaggle: string; selected: BacklogItem; sources: SourceView[]; unavailable: () => void; goobers?: Goober[] }) {
  const [item, setItem] = useState<BacklogItem>();
  const [editableItem, setEditableItem] = useState<BacklogItem>();
  const [revision, setRevision] = useState(0);
  const [related, setRelated] = useState<ItemSelection>();
  useEffect(() => {
    const controller = new AbortController();
    setItem(undefined); setRelated(undefined);
    void client.getWorkbenchItem(gaggle, selected.ref.sourceBindingId, { id: selected.locator.id, expectedSourceId: selected.ref.sourceId }, { signal: controller.signal }).then((value) => {
      if (controller.signal.aborted) return;
      if (value.ref.gaggleId !== gaggle || value.ref.sourceBindingId !== selected.ref.sourceBindingId || value.ref.sourceId !== selected.ref.sourceId || value.ref.kind !== "work-item") { unavailable(); return; }
      setItem(value); setEditableItem(value);
    }).catch(() => { if (!controller.signal.aborted) unavailable(); });
    return () => controller.abort();
    // The selection component is keyed by immutable identity; caller callback changes do not restart reads.
  }, [client, gaggle, selected, revision]);
  const source = item && sources.find((entry) => entry.bindingId === item.ref.sourceBindingId);
  return <section className="workbench-detail" aria-label="Backlog item details">
    {!item ? <p role="status">Loading item details…</p> : <>
      <header><h3>{item.title}</h3><button type="button" onClick={() => { setItem(undefined); setRevision((value) => value + 1); }}>Refresh item</button></header>
      <p>#{item.locator.id} · {item.type} · {item.state}</p>
      {item.objective && <p className="workbench-objective"><strong>Objective</strong> · Source classification for organizing work. It does not establish completion or outcome progress.</p>}
      <SourceLink url={item.locator.url} label="Open item in source" />
      <dl><div><dt>Source identity</dt><dd>{item.ref.sourceId}</dd></div>{item.revision && <div><dt>Source revision</dt><dd>{item.revision}</dd></div>}{item.updatedAt && <div><dt>Updated at source</dt><dd><time dateTime={item.updatedAt}>{item.updatedAt}</time></dd></div>}</dl>
      {!!item.labels?.length && <p>Labels: {item.labels.join(", ")}</p>}{!!item.assignees?.length && <p>Assigned to: {item.assignees.join(", ")}</p>}
      <h4>Description</h4><p className="workbench-source-text">{item.description || "No description provided."}</p>
      {item.acceptanceCriteria && <><h4>Acceptance criteria</h4><p className="workbench-source-text">{item.acceptanceCriteria}</p></>}
      <h4>Associated items and links</h4><p>Relationships describe source data. Linked content is loaded only after a separate access check.</p>
      <dl className="workbench-coverage"><div><dt>Parent / child relationships</dt><dd>{coverageLabel(item.relationshipCoverage.parents)}</dd></div><div><dt>Dependencies</dt><dd>{coverageLabel(item.relationshipCoverage.blockers)}</dd></div><div><dt>Milestone membership</dt><dd>{coverageLabel(item.relationshipCoverage.milestones)}</dd></div></dl>
      {!item.relationships?.length && <p>No relationships returned. Check coverage above before assuming there are none.</p>}
      <ul className="workbench-relations">{item.relationships?.map((relation, index) => <Relationship key={index} relationship={relation} gaggle={gaggle} sources={sources} open={setRelated} />)}</ul>
      {related && <WorkbenchRelatedItem key={`${related.ref.sourceBindingId}:${related.ref.sourceId}`} client={client} gaggle={gaggle} selected={related} close={() => setRelated(undefined)} unavailable={unavailable} />}
    </>}
    {item && source && <WorkbenchBlockerResolution client={client} item={item} source={source} goobers={goobers} />}
    {editableItem && <WorkbenchItemEditor client={client} item={editableItem} refreshed={(value) => { setItem(value); setEditableItem(value); }} />}
  </section>;
}
function Relationship({ relationship, gaggle, sources, open }: { relationship: NativeRelationship; gaggle: string; sources: SourceView[]; open: (item: ItemSelection) => void }) {
  const { target } = relationship;
  const verified = target.ref?.gaggleId === gaggle && sources.some((source) => source.bindingId === target.ref?.sourceBindingId);
  const targetRef = target.ref;
  const readable = verified && target.ref?.kind === "work-item" && sources.some((source) => source.bindingId === target.ref?.sourceBindingId && source.kind === "backlog") && target.locator.id;
  return <li><strong>{relationLabel(relationship)}</strong>: {target.kind} {target.locator.id || target.stableId || "(no locator)"}
    <span> · {verified ? "Source membership verified." : "Unverified source link; content not loaded."}</span>
    {readable && targetRef && <button type="button" onClick={() => open({ ref: targetRef, locator: target.locator })}>Load related item {target.locator.id}</button>}
    <SourceLink url={target.locator.url} label="Open source link" />
  </li>;
}
function WorkbenchRelatedItem({ client, gaggle, selected, close, unavailable }: { client: DaemonClient; gaggle: string; selected: ItemSelection; close: () => void; unavailable: () => void }) {
  const [item, setItem] = useState<BacklogItem>();
  useEffect(() => {
    const controller = new AbortController();
    void client.getWorkbenchItem(gaggle, selected.ref.sourceBindingId, { id: selected.locator.id, expectedSourceId: selected.ref.sourceId }, { signal: controller.signal }).then((value) => {
      if (controller.signal.aborted) return;
      if (value.ref.gaggleId !== gaggle || value.ref.sourceBindingId !== selected.ref.sourceBindingId || value.ref.sourceId !== selected.ref.sourceId || value.ref.kind !== "work-item") { unavailable(); return; }
      setItem(value);
    }).catch(() => { if (!controller.signal.aborted) unavailable(); });
    return () => controller.abort();
  }, [client, gaggle, selected]);
  return <aside aria-label="Related item"><button type="button" onClick={close}>Close related item</button>{item ? <><h4>{item.title}</h4><p>{item.type} · {item.state}{item.objective ? " · Objective (source classification)" : ""}</p><p className="workbench-source-text">{item.description || "No description provided."}</p><SourceLink url={item.locator.url} label="Open related item in source" /></> : <p role="status">Loading related item…</p>}</aside>;
}
function relationLabel(relation: NativeRelationship): string {
  switch (relation.kind) {
    case "parent-of": return relation.incoming ? "Parent" : "Child";
    case "blocked-by": return relation.incoming ? "Blocks" : "Blocked by";
    case "milestone-member": return "Milestone";
    case "contributes-to": return relation.incoming ? "Contribution from" : "Contributes to";
    default: return `${relation.kind}${relation.incoming ? " (incoming)" : ""}`;
  }
}
function coverageLabel(value: RelationshipCoverageState): string {
  switch (value) { case "complete": return "Relations enumerated; linked content not loaded."; case "partial": return "Partial relationship coverage."; case "not-loaded": return "Relationships not loaded."; case "unsupported": return "Relationship lookup unsupported by this source."; default: return "Relationship coverage unknown."; }
}
function SourceLink({ url, label }: { url?: string; label: string }) {
  if (!url) return null;
  try { const parsed = new URL(url); if ((parsed.protocol !== "https:" && parsed.protocol !== "http:") || parsed.username || parsed.password) return null; } catch { return null; }
  return <a href={url} target="_blank" rel="noreferrer noopener">{label}</a>;
}
