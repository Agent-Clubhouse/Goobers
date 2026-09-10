import { useMemo, useSyncExternalStore } from "react";
import type { RunSummary } from "../api/types";
import { formatDuration } from "../runDetailData";

// One clock for all mounted active rows, with no timer left behind on navigation.
let now = Date.now();
let timer: ReturnType<typeof setInterval> | undefined;
const listeners = new Set<() => void>();
function subscribe(listener: () => void) {
  listeners.add(listener);
  if (timer === undefined) {
    now = Date.now();
    timer = setInterval(() => {
      now = Date.now();
      for (const notify of listeners) notify();
    }, 1000);
  }
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0) {
      clearInterval(timer);
      timer = undefined;
    }
  };
}
const noSubscribe = () => () => {};
const snapshot = () => now;

export function RunTiming({ run }: { run: RunSummary }) {
  const tick = useSyncExternalStore(run.terminal ? noSubscribe : subscribe, snapshot, snapshot);
  const baseline = useMemo(() => ({ at: Date.now(), duration: run.durationMillis }),
    [run.id, run.lastSeq, run.durationMillis]);
  if (run.terminal) return <span className="run-duration">{formatDuration(run.durationMillis)}</span>;

  // Extend the daemon's reported duration, not client-now minus server-start:
  // browser/daemon clock skew must not inflate either elapsed scope.
  const elapsed = Math.max(0, baseline.duration + Math.max(0, tick - baseline.at));
  const observedAt = Date.parse(run.startedAt) + elapsed;
  return (
    <span className="run-timing">
      <strong aria-label="Run elapsed time">{formatDuration(elapsed)}</strong>
      {run.activeStages?.map((stage) => {
        const startedAt = Date.parse(stage.startedAt);
        const label = stage.goober ? stage.goober : stage.name;
        return (
          <span className="row-subtitle" key={`${stage.kind}:${stage.name}:${stage.branch ?? 0}`}
            aria-label={`${stage.goober ? `Goober ${stage.goober}` : `Stage ${stage.name}`} elapsed time`}>
            {label}{stage.branch ? ` · branch ${stage.branch}` : ""} · {Number.isFinite(startedAt) && startedAt > 0 && Number.isFinite(observedAt)
              ? formatDuration(Math.max(0, observedAt - startedAt)) : "elapsed unavailable"}
          </span>
        );
      })}
      {run.activityTruncated && <span className="row-subtitle">Additional stage activity omitted</span>}
    </span>
  );
}
