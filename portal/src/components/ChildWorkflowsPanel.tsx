import { useEffect, useState } from "react";
import type { ChildWorkflowPage, ChildWorkflowSummary, DaemonClient } from "../api/types";
import "../childWorkflows.css";
import { ChildPublications } from "./ChildPublications";

const stateLabels: Record<ChildWorkflowSummary["state"], string> = {
  queued: "Queued", running: "Running", awaiting_human: "Needs a human",
  completed: "Completed", failed: "Failed", cancelled: "Cancelled",
};

export function ChildWorkflowsPanel({ client, runId }: { client: DaemonClient; runId: string }) {
  const [page, setPage] = useState<ChildWorkflowPage>();
  const [after, setAfter] = useState<string>();
  const [refresh, setRefresh] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true); setError("");
    void client.getChildWorkflows(runId, after, { signal: controller.signal }).then((next) => {
      if (!controller.signal.aborted) setPage(next);
    }).catch(() => {
      if (!controller.signal.aborted) {
        setPage(undefined);
        setError("Child workflow information is unavailable. Check your access or try again.");
      }
    }).finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  }, [client, runId, after, refresh]);
  // The RunPage mounts this panel with the run identity as its key. Only the
  // selected page is retained; old run data cannot flash during navigation.
  return <section className="child-workflows-panel" aria-label="Child workflows">
    <div className="child-workflows-heading"><h2>Child workflows</h2><button type="button" disabled={loading} onClick={() => setRefresh((value) => value + 1)}>Refresh children</button></div>
    {loading && <p role="status">Loading child workflows…</p>}
    {error && <p role="status">{error}</p>}
    {page && !loading && <>
      {page.executionHistory && <section aria-label="Child execution history">
        <h3>Execution history</h3>
        <p>A human restart keeps earlier results and resumes this child with new guidance.</p>
        <ol className="child-workflows-list">{page.executionHistory.map((execution) => <li key={execution.runId}>
          <div><strong>{execution.epoch === 0 ? "Original execution" : `Human restart ${execution.epoch}`}</strong><span>{stateLabels[execution.state]}{execution.current ? " · Current" : ""}</span></div>
          {execution.actor && <p>Requested by {execution.actor}{execution.stage ? ` · ${execution.stage}` : ""}</p>}
          <small>{new Date(execution.acceptedAt).toLocaleString()}</small>
          {execution.runAvailable && execution.runId !== runId && <a href={`#/run/${encodeURIComponent(execution.runId)}`}>Open {execution.current ? "current" : "earlier"} execution</a>}
          {execution.runId === runId && <span>Viewing this execution</span>}
          {!execution.runAvailable && <p>The execution journal is not available.</p>}
        </li>)}</ol>
      </section>}
      {page.publicationRunId && page.publicationRunId !== runId && <p>These publication records belong to the original child execution. Checks are recorded in that execution’s history.</p>}
      <ChildPublications client={client} runId={page.publicationRunId ?? runId} publications={page.publications ?? []} available={page.publicationCheckAvailable ?? false} reason={page.publicationCheckReason} refresh={() => setRefresh((value) => value + 1)} />
      {page.parent && <p>Started by <a href={`#/run/${encodeURIComponent(page.parent.runId)}`}>{page.parent.workflow} · parent run</a> for “{page.parent.invocationKey}”.</p>}
      {page.children.length === 0 ? <p>No child workflows on this page.</p> : <>
        <p>Queue status and returned results are shown separately from execution. A completed child may still await its parent’s decision.</p>
        <ul className="child-workflows-list">{page.children.map((child) => <li key={child.childId}>
          <div><strong>{child.workflow || child.invocationKey}</strong><span>{stateLabels[child.state]}</span></div>
          {child.stage && <p>Parent stage: {child.stage} · invocation {child.sequence}</p>}
          {!!child.executionEpoch && <p>Human restart {child.executionEpoch}</p>}
          <p>{childProgress(child)}</p>
          {child.publicationNeedsHuman && <p>Publication needs confirmation. Open the child run to review it.</p>}
          {child.cancellationRequested && <p>{isTerminal(child) ? "Cancellation was requested." : "Cancellation requested; stopping is not yet confirmed."}</p>}
          <small>Accepted {new Date(child.acceptedAt).toLocaleString()} · Updated {new Date(child.updatedAt).toLocaleString()}</small>
          {child.runAvailable && child.runId && <a href={`#/run/${encodeURIComponent(child.runId)}`}>Open child run</a>}
        </li>)}</ul>
      </>}
      <nav aria-label="Child workflow pages">
        {after && <button type="button" onClick={() => setAfter(undefined)}>First page</button>}
        {page.nextCursor && <button type="button" onClick={() => setAfter(page.nextCursor)}>Next children</button>}
      </nav>
    </>}
  </section>;
}

function isTerminal(child: ChildWorkflowSummary) {
  return child.state === "completed" || child.state === "failed" || child.state === "cancelled";
}

function childProgress(child: ChildWorkflowSummary) {
  if (child.expired) return "Detailed result retention has expired.";
  if (child.acknowledged) return "Returned result acknowledged by the parent.";
  if (isTerminal(child)) return "Returned result awaits parent acknowledgement.";
  if (child.state === "queued") return child.runAvailable ? "Execution is recorded; queue confirmation is pending." : "Queued for execution.";
  if (child.state === "awaiting_human") return "A human intervention is required.";
  return "Child execution is in progress.";
}
