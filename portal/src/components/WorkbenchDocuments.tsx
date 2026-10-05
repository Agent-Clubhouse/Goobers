import { useEffect, useRef, useState } from "react";
import { DaemonApiError } from "../api/errors";
import type { DaemonClient, SourceView, WorkbenchDocumentPage } from "../api/types";
import { MetadataProposalEditor } from "./MetadataProposalEditor";
import { WorkbenchDocumentDetail } from "./WorkbenchDocumentDetail";

const windowSize = 8;
export function WorkbenchDocuments({ client, gaggle, source, sources, refresh }: { client: DaemonClient; gaggle: string; source: SourceView; sources: SourceView[]; refresh: () => void }) {
  const [cursor, setCursor] = useState<string>();
  const [loaded, setLoaded] = useState<{ client: DaemonClient; source: SourceView; gaggle: string; cursor?: string; page: WorkbenchDocumentPage }>();
  const [selectedPath, setSelectedPath] = useState<string>();
  const [error, setError] = useState("");
  const pins = useRef<{ target: string; commit: string }>(undefined);
  const page = loaded?.client === client && loaded.source === source && loaded.gaggle === gaggle && loaded.cursor === cursor ? loaded.page : undefined;
  useEffect(() => {
    const controller = new AbortController();
    setLoaded(undefined); setSelectedPath(undefined); setError("");
    void client.getWorkbenchDocuments(gaggle, source.bindingId, { cursor, limit: windowSize }, { signal: controller.signal }).then((value) => {
      if (controller.signal.aborted) return;
      if (!matchesSource(value, source, gaggle)) { setError("The document source identity changed. Refresh documents to start again."); return; }
      if (pins.current && (pins.current.target !== value.sourceTargetDigest || pins.current.commit !== value.commit)) { setError("The source or branch changed while browsing. Refresh documents to start again."); return; }
      pins.current = { target: value.sourceTargetDigest, commit: value.commit };
      setLoaded({ client, source, gaggle, cursor, page: value });
    }).catch((reason: unknown) => {
      if (controller.signal.aborted) return;
      setError(reason instanceof DaemonApiError && reason.status === 409
        ? "The source or branch changed while browsing. Refresh documents to start again."
        : "Documents are unavailable. Check your source read access or refresh documents.");
    });
    return () => controller.abort();
  }, [client, gaggle, source, cursor]);
  const selected = page?.files.find((file) => file.path === selectedPath);
  function next() { setLoaded(undefined); setSelectedPath(undefined); setCursor(page?.nextCursor); }
  return <section aria-label="Repository documents">
    <div className="workbench-actions"><p>Read only explicitly configured files, up to {windowSize} per window. Source configuration does not grant read access.</p><button type="button" onClick={refresh}>Refresh documents</button></div>
    {error && <p role="status">{error}</p>}
    {!page && !error && <p role="status">Loading configured documents…</p>}
    {page && <>
      <div className="workbench-coverage" role="status">
        <p>Branch <code>{page.branch}</code> · Commit <code>{page.commit}</code></p>
        <p>{page.files.length === 0 ? "No files in this window" : `Configured files ${page.startOffset + 1}–${page.startOffset + page.files.length}`} · {page.totalPaths} configured paths.</p>
        <p>{page.coverage === "complete" ? "All configured paths were read at this commit." : "Partial coverage; this window is not a complete source snapshot."} {page.reasons?.map((reason) => reason.replaceAll("-", " ")).join("; ")}</p>
        {page.exhausted && <p>End of configured paths. Unavailable files do not establish deletion.</p>}
      </div>
      <div className="workbench-layout">
        <nav aria-label="Configured documents">
          <ul>{page.files.map((file) => <li key={file.path}><button type="button" aria-pressed={selectedPath === file.path} onClick={() => setSelectedPath(file.path)}><strong>{file.path}</strong><span>{documentStatus(file.status)}</span></button></li>)}</ul>
          {page.nextCursor && <button type="button" onClick={next}>Next files</button>}
        </nav>
        {selected ? <div key={`${page.commit}:${selected.path}`}><WorkbenchDocumentDetail file={selected} /><MetadataProposalEditor client={client} gaggle={gaggle} source={source} sources={sources} file={selected} /></div> : <p>Select a configured file to read its content and source metadata.</p>}
      </div>
    </>}
  </section>;
}

function matchesSource(page: WorkbenchDocumentPage, source: SourceView, gaggle: string): boolean {
  const repository = page.repository;
  const paths = source.paths ?? [];
  if (page.sourceBindingId !== source.bindingId || repository.provider !== source.provider || repository.owner !== source.owner || (repository.project ?? "") !== (source.project ?? "") || repository.name !== source.repository || page.branch !== source.branch || !page.commit || !page.sourceTargetDigest) return false;
  if (!Number.isInteger(page.startOffset) || page.startOffset < 0 || page.totalPaths !== paths.length || page.files.length > windowSize || page.startOffset + page.files.length > paths.length) return false;
  return page.files.every((file, index) => file.path === paths[page.startOffset + index]
    && (file.status !== "available" || !!file.provenance)
    && (!file.provenance || file.provenance.commit === page.commit)
    && (!file.objective || (file.ref?.gaggleId === gaggle && file.ref.sourceBindingId === source.bindingId && file.ref.kind === "objective-document" && file.ref.sourceId === file.objective.objectiveId)));
}
function documentStatus(status: string): string {
  switch (status) {
    case "available": return "Available";
    case "unavailable": return "Unavailable";
    case "invalid-source": return "Invalid source metadata";
    case "oversized": return "Exceeds the source read limit";
    default: return "Unknown read status";
  }
}
