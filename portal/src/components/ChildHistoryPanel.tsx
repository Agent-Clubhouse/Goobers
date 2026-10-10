import { useState } from "react";
import type { ChildHistoryItem, ChildHistoryPage, DaemonClient } from "../api/types";
import { MalformedResponseError } from "../api/errors";
import { dataCacheKey } from "../dataCache";
import { useLiveData } from "../liveData";
import { useLiveQuery } from "../liveQuery";
import type { Navigate } from "../routing";
import { Action } from "../ui/Action";
import { Timestamp } from "../ui/Timestamp";
import { ChildPublicationSummary } from "./ChildPublicationSummary";

const stateLabels: Record<ChildHistoryItem["state"], string> = {
  queued: "Queued", running: "Started", awaiting_human: "Needs human",
  completed: "Completed", failed: "Failed", cancelled: "Cancelled",
};

export function ChildHistoryPanel({ client, runId, gaggle, navigate }: {
  client: DaemonClient; runId: string; gaggle: string; navigate: Navigate;
}) {
  const { cache } = useLiveData();
  const [cursors, setCursors] = useState([""]);
  const cursor = cursors[cursors.length - 1];
  const query = useLiveQuery<ChildHistoryPage & { requestedCursor: string }>({
    cacheKey: dataCacheKey("child-history", runId, cursor),
    dependencies: [{ model: "run", gaggle }], models: ["run"], scope: { gaggle },
    isCurrent: (value) => value.runId === runId && value.gaggle === gaggle && value.requestedCursor === cursor,
    load: async (signal) => {
      const value = await client.listRunChildren(runId, cursor, { signal });
      if (value.runId !== runId || value.gaggle !== gaggle || !Array.isArray(value.items) || value.items.length > 50) {
        throw new MalformedResponseError("The daemon returned mismatched child history.");
      }
      return { ...value, requestedCursor: cursor };
    },
    errorMessage: "Unable to read child history.",
  });
  const value = query.state.status === "ready" || query.state.status === "stale" ? query.state.data : undefined;
  return (
    <section aria-label="Accepted child history" className="run-lineage child-activity">
      <h2>Accepted child history</h2>
      <Action onClick={() => { if (cursor) { cache.remove(dataCacheKey("child-history", runId, "")); setCursors([""]); } else query.retry(); }}>Refresh child history</Action>
      {query.state.status === "loading" && <p role="status">Loading child history…</p>}
      {query.state.status === "error" && <p role="alert">Child history unavailable. {query.state.error.message}</p>}
      {query.state.status === "stale" && <p role="status">This observation may be stale. Refresh to check for changes.</p>}
      {value?.status === "unavailable" && <p>Child history is unavailable from this daemon.</p>}
      {value?.status === "recorded" && <>
        <p>Observed <Timestamp value={value.observedAt} />. “Started” records dispatch; it does not confirm a worker is currently running.</p>
        {value.items.length === 0 && <p>{cursor ? "No further retained children." : "No retained child acceptances."}</p>}
        <ul>{value.items.map((child) => <li key={child.childId}>
          <strong>{child.invocationKey}</strong>{" · "}{stateLabels[child.state] ?? "Unknown state"}
          <details><summary>Stage visit</summary><span className="mono">{child.stageOccurrence}</span></details>
          <div>Accepted <Timestamp value={child.acceptedAt} /> · Updated <Timestamp value={child.updatedAt} /></div>
          {child.cancellationRequested && !child.terminalAt && <p>Cancellation requested; stopping is not yet confirmed.</p>}
          {child.acknowledgedAt && <p>Parent acknowledged the outcome <Timestamp value={child.acknowledgedAt} />.</p>}
          <ChildPublicationSummary publication={child.publication} />
          {child.expiredAt ? <p>Saved child result expired <Timestamp value={child.expiredAt} />. This result is no longer recoverable.</p> :
            child.state !== "queued" && <Action onClick={() => navigate({ page: "run", id: child.runId })} aria-label={`Open child run ${child.runId}`}>Open child run</Action>}
        </li>)}</ul>
        <p>Refresh returns to the first page and includes newly accepted children. Older run details may no longer be available.</p>
        <Action disabled={cursors.length === 1 || query.refreshing} onClick={() => setCursors((current) => current.slice(0, -1))}>Previous children</Action>{" "}
        <Action disabled={!value.nextCursor || query.refreshing} onClick={() => setCursors((current) => [...current, value.nextCursor])}>Next children</Action>
      </>}
    </section>
  );
}
