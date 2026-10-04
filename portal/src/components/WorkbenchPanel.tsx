import { useEffect, useState } from "react";
import type { DaemonClient, WorkbenchSourcePage } from "../api/types";
import { WorkbenchBrowser } from "./WorkbenchBrowser";
import { WorkbenchDocuments } from "./WorkbenchDocuments";
import "../workbench.css";

export function WorkbenchPanel({ client, gaggle }: { client: DaemonClient; gaggle: string }) {
  // A new client or gaggle discards all source data before the next request.
  return <WorkbenchSources key={gaggle} client={client} gaggle={gaggle} />;
}
function WorkbenchSources({ client, gaggle }: { client: DaemonClient; gaggle: string }) {
  const [loaded, setLoaded] = useState<{ client: DaemonClient; revision: number; page: WorkbenchSourcePage }>();
  const [revision, setRevision] = useState(0);
  const [binding, setBinding] = useState("");
  const [error, setError] = useState("");
  const page = loaded?.client === client && loaded.revision === revision ? loaded.page : undefined;
  useEffect(() => {
    const controller = new AbortController();
    setLoaded(undefined); setError("");
    void client.listWorkbenchSources(gaggle, { signal: controller.signal })
      .then((value) => { if (!controller.signal.aborted) setLoaded({ client, revision, page: value }); })
      .catch(() => { if (!controller.signal.aborted) { setBinding(""); setError("Planning sources are unavailable. Check your gaggle access or refresh."); } });
    return () => controller.abort();
  }, [client, gaggle, revision]);
  const source = page?.items.find((item) => item.bindingId === binding);
  return <section className="workbench-panel" aria-label="Backlog and objectives">
    <header><div><h2>Backlog and objectives</h2><p>Browse configured sources and the relationships they declare.</p></div><button type="button" onClick={() => setRevision((value) => value + 1)}>Refresh sources</button></header>
    {!page && !error && <p role="status">Loading planning sources…</p>}
    {error && <p role="status">{error}</p>}
    {page && <>
      {page.items.length === 0 ? <p>No planning sources are configured for this gaggle.</p> : <label>Planning source<select value={source?.bindingId ?? ""} onChange={(event) => setBinding(event.target.value)}><option value="">Select a source</option>{page.items.map((item) => <option key={item.bindingId} value={item.bindingId}>{item.bindingId} · {item.provider} · {item.kind}</option>)}</select></label>}
      {source && <>
        <p className="workbench-source">{source.owner}{source.project ? ` / ${source.project}` : ""}{source.repository ? ` / ${source.repository}` : ""}{source.branch ? ` · ${source.branch}` : ""}</p>
        {(source.paths?.length ?? 0) > 0 && <p>Configured paths: {source.paths?.join(", ")}</p>}
        {source.kind === "backlog" ? <WorkbenchBrowser key={`${page.generation}:${source.bindingId}:${revision}`} client={client} gaggle={gaggle} source={source} sources={page.items} refresh={() => setRevision((value) => value + 1)} /> : source.kind === "documents" || source.kind === "relationships" ? <WorkbenchDocuments key={`${page.generation}:${source.bindingId}:${revision}`} client={client} gaggle={gaggle} source={source} refresh={() => setRevision((value) => value + 1)} /> : <p>This source kind is not supported in this view.</p>}
      </>}
    </>}
  </section>;
}
