import { useEffect, useRef, useState } from "react";
import type { BacklogItem, BacklogPage, DaemonClient, Goober, SourceView } from "../api/types";
import { WorkbenchItemDetail } from "./WorkbenchItemDetail";

const windowSize = 50;
export function WorkbenchBrowser({ client, gaggle, source, sources, refresh, goobers = [] }: { client: DaemonClient; gaggle: string; source: SourceView; sources: SourceView[]; refresh: () => void; goobers?: Goober[] }) {
  const [cursor, setCursor] = useState<string>();
  const [loaded, setLoaded] = useState<{ cursor?: string; page: BacklogPage }>();
  const [selected, setSelected] = useState<BacklogItem>();
  const [error, setError] = useState("");
  const target = useRef<string>(undefined);
  const page = loaded?.cursor === cursor ? loaded?.page : undefined;
  useEffect(() => {
    const controller = new AbortController();
    setLoaded(undefined); setSelected(undefined); setError("");
    void client.getWorkbenchItems(gaggle, source.bindingId, { cursor, limit: windowSize }, { signal: controller.signal }).then((value) => {
      if (controller.signal.aborted) return;
      if (target.current && target.current !== value.sourceTargetDigest) { setError("The source changed while browsing. Refresh sources to start again."); return; }
      if (value.items.some((item) => item.ref.gaggleId !== gaggle || item.ref.sourceBindingId !== source.bindingId || item.ref.kind !== "work-item")) { setError("The source returned an item outside this planning scope. Refresh sources to retry."); return; }
      target.current = value.sourceTargetDigest;
      setLoaded({ cursor, page: value });
    }).catch(() => { if (!controller.signal.aborted) setError("Backlog items are unavailable. Check your source access or refresh sources."); });
    return () => controller.abort();
  }, [client, gaggle, source.bindingId, cursor]);
  function navigate(next?: string) { setSelected(undefined); setLoaded(undefined); setError(""); setCursor(next); }
  return <>
    <div className="workbench-actions"><p>One source window at a time, up to {windowSize} items. Counts describe this window.</p><button type="button" onClick={refresh}>Refresh backlog</button></div>
    {error && <p role="status">{error}</p>}
    {!page && !error && <p role="status">Loading backlog items…</p>}
    {page && <>
      <div className="workbench-coverage" role="status"><p>{page.items.length} items shown · {page.candidates} candidates checked · {page.omitted} omitted.</p>
        {page.partial && <p>Partial coverage. {page.reasons?.map(reasonLabel).join("; ") || "More candidates or omitted items remain."}</p>}
        {page.exhausted && <p>End of this provider window. The source can change between requests; this is not a complete snapshot.</p>}
      </div>
      <div className="workbench-layout">
        <nav aria-label="Backlog items"><ul>{page.items.map((item) => <li key={item.ref.sourceId}><button type="button" aria-pressed={selected?.ref.sourceId === item.ref.sourceId} onClick={() => setSelected(item)}><strong>{item.title}</strong><span>#{item.locator.id} · {item.type} · {item.state}{item.objective ? " · Objective" : ""}</span></button></li>)}</ul>{page.items.length === 0 && <p>No items in this window. This does not establish that the source is empty.</p>}
          <div className="workbench-actions">{cursor && <button type="button" onClick={() => navigate()}>First items</button>}{page.nextCursor && <button type="button" onClick={() => navigate(page.nextCursor)}>Next items</button>}</div>
        </nav>
        {selected ? <WorkbenchItemDetail key={`${selected.ref.sourceBindingId}:${selected.ref.sourceId}`} client={client} gaggle={gaggle} selected={selected} sources={sources} goobers={goobers} unavailable={() => { setSelected(undefined); setLoaded(undefined); setError("Source access or identity changed. Refresh sources to reload the backlog."); }} /> : <p>Select an item to load its details and relationships.</p>}
      </div>
    </>}
  </>;
}
function reasonLabel(reason: string): string { return reason.replaceAll("_", " ").replaceAll("-", " "); }
