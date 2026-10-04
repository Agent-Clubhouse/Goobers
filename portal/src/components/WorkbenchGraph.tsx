import { useEffect, useId, useMemo, useState } from "react";
import type { DaemonClient, SourceView, WorkbenchGraph as Graph } from "../api/types";

type Node = Graph["nodes"][number];
type Edge = Graph["edges"][number];
type Ref = Node["observations"][number]["ref"];
const refKey = (ref: Ref) => JSON.stringify([ref.gaggleId, ref.sourceBindingId, ref.kind, ref.sourceId]);
const nodeTitle = (node: Node) => node.conflict ? "Conflicting source observations" : node.observations[0]?.title || "Untitled source item";

export function WorkbenchGraph({ client, gaggle, generation, sources }: { client: DaemonClient; gaggle: string; generation: string; sources: SourceView[] }) {
  const [request, setRequest] = useState(0);
  const [loaded, setLoaded] = useState<{ client: DaemonClient; gaggle: string; sources: SourceView[]; generation: string; request: number; graph: Graph }>();
  const [error, setError] = useState("");
  const graph = loaded?.client === client && loaded.gaggle === gaggle && loaded.sources === sources && loaded.generation === generation && loaded.request === request ? loaded.graph : undefined;
  useEffect(() => {
    setLoaded(undefined); setError("");
    if (request === 0) return;
    const controller = new AbortController();
    void client.getWorkbenchGraph(gaggle, { signal: controller.signal }).then((value) => {
      if (controller.signal.aborted) return;
      const bindings = new Set(sources.map((source) => source.bindingId));
      if (value.gaggleId !== gaggle || value.generation !== generation || value.nodes.length > 10_000 || value.edges.length > 50_000 || value.nodes.some((node) => node.observations.some((observation) => observation.ref.gaggleId !== gaggle || !bindings.has(observation.ref.sourceBindingId)))) {
        setError("Planning sources changed. Refresh sources before loading relationships again."); return;
      }
      setLoaded({ client, gaggle, sources, generation, request, graph: value });
    }).catch(() => { if (!controller.signal.aborted) setError("Relationships are unavailable. Check source access or refresh sources and try again."); });
    return () => controller.abort();
  }, [client, gaggle, generation, sources, request]);
  return <section className="workbench-map" aria-label="Objective relationships">
    <div className="workbench-actions"><div><h3>Objectives and relationships</h3><p>Explore the links declared in your backlog and repository sources.</p></div><button type="button" onClick={() => setRequest((value) => value + 1)}>{request ? "Refresh relationships" : "Load relationships"}</button></div>
    {error && <p role="status">{error}</p>}
    {request > 0 && !graph && !error && <p role="status">Loading source relationships…</p>}
    {graph && <GraphView key={`${generation}:${request}`} graph={graph} />}
  </section>;
}

function GraphView({ graph }: { graph: Graph }) {
  const [focus, setFocus] = useState<string>();
  const [objectivesOnly, setObjectivesOnly] = useState(true);
  const [page, setPage] = useState(0);
  const [kind, setKind] = useState("");
  const [relationPage, setRelationPage] = useState(0);
  const items = graph.nodes.filter((node) => !objectivesOnly || node.observations.some((observation) => observation.objective));
  const byRef = useMemo(() => new Map(graph.nodes.flatMap((node) => node.observations.map((observation) => [refKey(observation.ref), node] as const))), [graph.nodes]);
  const selected = graph.nodes.find((node) => node.key === focus);
  const endpointNode = (endpoint: Edge["from"]) => endpoint.ref ? byRef.get(refKey(endpoint.ref)) : undefined;
  const related = selected ? graph.edges.filter((edge) => (!kind || edge.kind === kind) && (endpointNode(edge.from)?.key === selected.key || endpointNode(edge.to)?.key === selected.key)) : [];
  function select(key: string) { setFocus(key); setRelationPage(0); }
  function endpointLabel(endpoint: Edge["from"]) {
    const node = endpointNode(endpoint);
    return node ? nodeTitle(node) : endpoint.ref ? `${endpoint.ref.sourceBindingId} / ${endpoint.ref.sourceId}` : endpoint.native?.locator.id || "Unread source item";
  }
  return <>
    <p role="status">{graph.nodes.length} source items · {graph.edges.length} declared relationships · {graph.conflicts.length} conflicts.{graph.partial ? " Partial coverage: some sources or relationships are incomplete." : " All configured source windows were read."}</p>
    <details><summary>Source coverage</summary><ul>{graph.sources.map((source) => <li key={source.sourceBindingId}>{source.sourceBindingId}: {source.status.replaceAll("-", " ")}{source.reasons?.length ? ` · ${source.reasons.map((reason) => reason.replaceAll("-", " ")).join(", ")}` : ""}</li>)}</ul><p>This view organizes observed links; it does not calculate delivery progress.</p></details>
    <label className="workbench-map-filter"><input type="checkbox" checked={objectivesOnly} onChange={(event) => { setObjectivesOnly(event.target.checked); setPage(0); }} /> Show objectives only</label>
    <nav aria-label="Planning items"><ul className="workbench-map-items">{items.slice(page * 20, (page + 1) * 20).map((node) => <li key={node.key}><button type="button" aria-pressed={focus === node.key} onClick={() => select(node.key)}>{nodeTitle(node)}{node.conflict ? " · conflict" : ""}</button></li>)}</ul>{items.length === 0 && <p>No {objectivesOnly ? "objectives" : "items"} in the loaded source windows.</p>}
      {page > 0 && <button type="button" onClick={() => setPage(page - 1)}>Previous items</button>}{(page + 1) * 20 < items.length && <button type="button" onClick={() => setPage(page + 1)}>More planning items</button>}
    </nav>
    {selected ? <section aria-label="Selected objective relationships">
      <h4>{nodeTitle(selected)}</h4>
      {selected.conflict ? <p role="status">Multiple source observations conflict. Review each source before resolving this item.</p> : <p>{selected.observations[0]?.ref.sourceBindingId} · {selected.observations[0]?.ref.sourceId}</p>}
      <label>Relationship type<select value={kind} onChange={(event) => { setKind(event.target.value); setRelationPage(0); }}><option value="">All declared types</option>{Array.from(new Set(graph.edges.map((edge) => edge.kind))).sort().map((value) => <option key={value} value={value}>{value.replaceAll("-", " ")}</option>)}</select></label>
      {!selected.conflict && <RelationshipDiagram selected={selected} edges={related} lookup={endpointNode} select={select} />}
      {selected.conflict && <ul>{selected.observations.map((observation, index) => <li key={index}>{observation.title} · {observation.ref.sourceBindingId} · {observation.path || observation.locator.id} · revision {observation.revision}</li>)}</ul>}
      <ul className="workbench-map-relations">{related.slice(relationPage * 20, (relationPage + 1) * 20).map((edge) => <li key={edge.key}><strong>{endpointLabel(edge.from)}</strong> → {edge.kind.replaceAll("-", " ")} → <strong>{endpointLabel(edge.to)}</strong>{edge.conflict ? " · source ownership conflict" : (!edge.from.resolved || !edge.to.resolved) ? " · linked content not loaded" : ""}{edge.rationale && <p>{edge.rationale}</p>}<small>Declared by {edge.owner.sourceBindingId}{edge.owner.path ? ` / ${edge.owner.path}` : ""}</small></li>)}</ul>
      {related.length === 0 && <p>No matching relationships in the loaded source windows.</p>}
      {relationPage > 0 && <button type="button" onClick={() => setRelationPage(relationPage - 1)}>Previous relationships</button>}{(relationPage + 1) * 20 < related.length && <button type="button" onClick={() => setRelationPage(relationPage + 1)}>More relationships</button>}
    </section> : <p>Select an objective or item to explore its incoming and outgoing links.</p>}
  </>;
}

function RelationshipDiagram({ selected, edges, lookup, select }: { selected: Node; edges: Edge[]; lookup: (endpoint: Edge["from"]) => Node | undefined; select: (key: string) => void }) {
  const marker = useId();
  const incoming = new Map<string, Node>(); const outgoing = new Map<string, Node>();
  const safe = edges.filter((edge) => !edge.conflict && edge.from.resolved && edge.to.resolved && !lookup(edge.from)?.conflict && !lookup(edge.to)?.conflict);
  for (const edge of safe) {
    const from = lookup(edge.from), to = lookup(edge.to);
    if (from && from.key !== selected.key && incoming.size < 10) incoming.set(from.key, from);
    if (to && to.key !== selected.key && outgoing.size < 10) outgoing.set(to.key, to);
  }
  const height = Math.max(180, Math.max(incoming.size, outgoing.size) * 60 + 40);
  const positions = new Map<string, { node: Node; x: number; y: number }>([[selected.key, { node: selected, x: 450, y: height / 2 }]]);
  for (const [nodes, x] of [[incoming, 135], [outgoing, 765]] as const) Array.from(nodes.values()).forEach((node, index) => { if (!positions.has(node.key)) positions.set(node.key, { node, x, y: 50 + index * 60 }); });
  return <div className="workbench-map-diagram"><svg viewBox={`0 0 900 ${height}`} role="group" aria-label="Declared relationship directions">
    <defs><marker id={marker} markerWidth="8" markerHeight="8" refX="7" refY="4" orient="auto"><path d="M0 0L8 4L0 8Z" fill="currentColor" /></marker></defs>
    {safe.map((edge) => { const from = positions.get(lookup(edge.from)?.key || ""), to = positions.get(lookup(edge.to)?.key || ""); return from && to ? <line key={edge.key} x1={from.x + (from.x < to.x ? 110 : -110)} y1={from.y} x2={to.x + (from.x < to.x ? -116 : 116)} y2={to.y} markerEnd={`url(#${marker})`}><title>{edge.kind.replaceAll("-", " ")}</title></line> : null; })}
    {Array.from(positions.values()).map(({ node, x, y }) => <g key={node.key} role="button" tabIndex={0} aria-label={`Explore ${nodeTitle(node)}`} onClick={() => select(node.key)} onKeyDown={(event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); select(node.key); } }} transform={`translate(${x},${y})`}><rect x="-110" y="-23" width="220" height="46" rx="7" className={node.key === selected.key ? "selected" : ""} /><text textAnchor="middle" dominantBaseline="middle">{nodeTitle(node).length > 27 ? `${nodeTitle(node).slice(0, 26)}…` : nodeTitle(node)}</text><title>{nodeTitle(node)}</title></g>)}
  </svg><p>Arrows follow the source's declared direction. The map shows up to 20 linked items; the list includes unresolved and conflicting links.</p></div>;
}
