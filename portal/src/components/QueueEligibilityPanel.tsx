import { useState } from "react";
import { assertQueueEligibility } from "../api/queueEligibility";
import type { DaemonClient, QueueEligibilityView } from "../api/types";
import { dataCacheKey } from "../dataCache";
import { useLiveQuery } from "../liveQuery";
import { formatTimestamp } from "../runDetailData";

const PAGE_SIZE = 50;

export function QueueEligibilityPanel({ client, gaggle, workflow }: { client: DaemonClient; gaggle: string; workflow: string }) {
  const [visible, setVisible] = useState(PAGE_SIZE);
  const query = useLiveQuery<QueueEligibilityView>({
    cacheKey: dataCacheKey("queue-eligibility", gaggle, workflow),
    dependencies: [{ model: "run", gaggle, workflow }, { model: "workflow", gaggle, workflow }],
    models: ["run", "workflow"],
    scope: { gaggle, workflow },
    load: async (signal) => {
      const value = await client.getWorkflowQueueEligibility(gaggle, workflow, { signal });
      assertQueueEligibility(value, gaggle, workflow);
      return value;
    },
    errorMessage: "Unable to read queue eligibility.",
  });
  const value = query.state.status === "ready" || query.state.status === "stale" ? query.state.data : undefined;
  const report = value?.report;
  return (
    <section aria-label="PR queue eligibility" className="inspector-section">
      <h2>PR queue eligibility</h2>
      <p>Historical selection evidence—not permission to claim or merge.</p>
      {query.state.status === "loading" && <p role="status">Loading queue observation…</p>}
      {query.state.status === "error" && <p role="alert">Queue observation unavailable: {query.state.error.message}</p>}
      {query.state.status === "stale" && <p role="status">Cached observation may be stale; refresh has not completed.</p>}
      {value && value.status !== "observed" && <p>{value.problem ?? "Queue observation unavailable."}</p>}
      {report && value?.status === "observed" && <>
        <p>Observed {formatTimestamp(report.observedAt)} in run <code>{value.sourceRunId}</code>.</p>
        {value.readState?.completeness === "partial" && <p role="status">The read projection is incomplete.</p>}
        {value.readState && <p>Projection lag: {Math.round(value.readState.lagSeconds)} seconds.</p>}
        {value.readState && value.readState.degraded.length > 0 && <p role="status">Projection warnings: {value.readState.degraded.join(", ")}. Freshness may be uncertain even when the queue snapshot is complete.</p>}
        {!report.completeSnapshot && <p role="status">Partial provider snapshot; this does not describe the whole queue.</p>}
        <p>{report.matchingItems} matching PRs; {report.omittedItems} omitted by the report bound.</p>
        {report.matchingItems === 0 && <p>No matching PRs were observed in this snapshot.</p>}
        {report.items.length > 0 && <table aria-label="Per-PR eligibility">
          <thead><tr><th>PR</th><th>Policy</th><th>Claim observation</th><th>Next steps</th></tr></thead>
          <tbody>{report.items.slice(0, visible).map((item) => <tr key={item.number}>
            <td>#{item.number}</td>
            <td>{item.eligible ? "Eligible under policy" : item.reason ?? "Excluded"}</td>
            <td>
              <div>{item.claim.state.replaceAll("-", " ")}</div>
              {item.claim.ownerRunId && <div>Owner: <code>{item.claim.ownerRunId}</code></div>}
              {item.claim.expiresAt && <div>Lease expiry: {formatTimestamp(item.claim.expiresAt)}</div>}
              <div>Provider claimed label: {item.claim.providerClaimLabel ? "present" : "absent"}</div>
              <div>{item.claim.comparison.replaceAll("-", " ")}</div>
            </td>
            <td><p>{item.nextStep}</p><p>{item.claim.nextStep}</p></td>
          </tr>)}</tbody>
        </table>}
        {visible < report.items.length && <button type="button" onClick={() => setVisible((count) => count + PAGE_SIZE)}>Show more PRs</button>}
      </>}
      <button type="button" onClick={query.retry}>Refresh queue observation</button>
    </section>
  );
}
