import { useEffect, useState } from "react";
import type { GoobersPortalApi, RunSummary, RunTrace } from "../api/types";

interface RunTraceViewerProps {
  api: GoobersPortalApi;
}

export function RunTraceViewer({ api }: RunTraceViewerProps) {
  const [runs, setRuns] = useState<RunSummary[]>([]);
  const [selectedRunId, setSelectedRunId] = useState<string>();
  const [trace, setTrace] = useState<RunTrace>();

  useEffect(() => {
    let active = true;
    void api.listRuns().then((nextRuns) => {
      if (!active) {
        return;
      }
      setRuns(nextRuns);
      setSelectedRunId((current) => current ?? nextRuns[0]?.id);
    });
    return () => {
      active = false;
    };
  }, [api]);

  useEffect(() => {
    let active = true;
    if (!selectedRunId) {
      setTrace(undefined);
      return () => {
        active = false;
      };
    }

    void api.getRunTrace(selectedRunId).then((nextTrace) => {
      if (active) {
        setTrace(nextTrace);
      }
    });
    return () => {
      active = false;
    };
  }, [api, selectedRunId]);

  return (
    <section className="panel" aria-labelledby="trace-title">
      <div className="panel-heading">
        <p className="eyebrow">Run telemetry</p>
        <h2 id="trace-title">Run and trace viewer</h2>
        <p>Placeholder surface for Goober-run telemetry: runs open into a span timeline.</p>
      </div>

      {runs.length === 0 ? (
        <p className="muted">No runs have been recorded yet.</p>
      ) : (
        <div className="trace-layout">
          <div className="run-list" aria-label="Runs">
            {runs.map((run) => (
              <button
                className={run.id === selectedRunId ? "run-item run-item-selected" : "run-item"}
                key={run.id}
                type="button"
                onClick={() => setSelectedRunId(run.id)}
              >
                <strong>{run.title}</strong>
                <span>
                  {run.status} · {run.spanCount} spans
                </span>
              </button>
            ))}
          </div>

          <ol className="span-timeline" aria-label="Trace spans">
            {trace?.spans.map((span) => (
              <li key={span.id}>
                <span className={`status status-${span.status}`}>{span.status}</span>
                <div>
                  <strong>{span.name}</strong>
                  <span>
                    {span.kind} · {new Date(span.startedAt).toLocaleTimeString()}
                  </span>
                </div>
              </li>
            )) ?? <li className="muted">Select a run to inspect spans.</li>}
          </ol>
        </div>
      )}
    </section>
  );
}
