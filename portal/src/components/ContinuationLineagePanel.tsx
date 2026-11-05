import { useEffect, useRef, useState } from "react";
import type { DaemonClient, RunLineage } from "../api/types";
import type { Navigate } from "../routing";

export function ContinuationLineagePanel({ client, runId, lineage: initial, navigate }: {
  client: DaemonClient; runId: string; lineage: RunLineage; navigate: Navigate;
}) {
  const [page, setPage] = useState<RunLineage>();
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string>();
  const request = useRef<AbortController | undefined>(undefined);
  useEffect(() => () => request.current?.abort(), []);
  const lineage = page ?? initial;
  async function nextPage() {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setPending(true);
    setError(undefined);
    try {
      const detail = await client.getRun(runId, {
        continuationsAfter: lineage.continuationsNextCursor, signal: controller.signal,
      });
      if (!controller.signal.aborted) setPage(detail.lineage ?? { historicalRepassCount: 0 });
    } catch (cause) {
      if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : "Could not load continuations.");
    } finally {
      if (!controller.signal.aborted) setPending(false);
    }
  }
  return <section aria-labelledby="run-lineage-heading" className="run-lineage">
    <h2 id="run-lineage-heading">Continuation lineage</h2>
    {initial.source && <p>
      Continued from{" "}<button type="button" onClick={() => navigate({ page: "run", id: initial.source!.id })}>{shorten(initial.source.id)}</button>{" "}
      ({initial.source.phase ?? "status unavailable"}) at <strong>{initial.resumeTarget}</strong>
      {initial.workspaceBranch ? ` on ${initial.workspaceBranch}` : ""}. Historical repasses: {initial.historicalRepassCount}{initial.historicalRepassCountPartial ? " (partial history)" : ""}.
    </p>}
    {(initial.injectedInputs?.length ?? 0) > 0 && <p>Injected input: {initial.injectedInputs!.map((input) => input.name).join(", ")}</p>}
    {lineage.continuationsIncomplete && <p role="status">Continuation history is incomplete or unavailable.</p>}
    {(lineage.continuations?.length ?? 0) > 0 && <p>Continued by{" "}{lineage.continuations!.map((run, index) => <span key={run.id}>
      {index > 0 ? ", " : ""}<button type="button" onClick={() => navigate({ page: "run", id: run.id })}>{shorten(run.id)}</button>{" "}({run.phase ?? "status unavailable"})
    </span>)}</p>}
    {lineage.continuationsTruncated && <p>More continuations are available.</p>}
    {page && <button type="button" disabled={pending} onClick={() => setPage(undefined)}>First continuations</button>}
    {lineage.continuationsNextCursor && <button type="button" disabled={pending} onClick={() => void nextPage()}>Next continuations</button>}
    {error && <p role="alert">{error}</p>}
  </section>;
}

function shorten(value: string): string {
  return value.length > 20 ? `${value.slice(0, 10)}…${value.slice(-6)}` : value;
}
