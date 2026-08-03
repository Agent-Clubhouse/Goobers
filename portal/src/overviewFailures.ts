import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { DaemonClient, TelemetryError } from "./api/types";
import { dataCacheKey, type DataCacheDependency } from "./dataCache";
import { useLiveData } from "./liveData";

// One bounded scan of the telemetry error index is enough to label the Overview's
// capped attention list — the failures an operator can act on are recent, and the
// index is the same authoritative coded-reason source the Errors page reads.
const FAILURE_REASON_SCAN_LIMIT = 200;

/** runId -> the most recent coded failure the telemetry index recorded for it. */
export type FailureReasons = Map<string, TelemetryError>;

const EMPTY_REASONS: FailureReasons = new Map();

// useFailureReasons enriches the Overview attention list with the coded "why"
// (e.g. harness.crash — "Harness exited before producing a result envelope") so a
// failed run reads its reason on the home page instead of "Run failed and needs
// investigation." with no clue. It only queries when there are failed runs to
// label, so instances (and unrelated pages) with no failures pay nothing. It is
// strictly best-effort: a telemetry read failure leaves the last-known reasons in
// place and never blanks or breaks the Overview, which owns the run groups.
export function useFailureReasons(
  client: DaemonClient,
  failedRunIds: readonly string[],
): FailureReasons {
  const { cache, subscribe } = useLiveData();
  const cacheKey = dataCacheKey("overview-failure-reasons");
  // Stable identity so the effect only re-runs when the set of failed runs
  // actually changes, not on every Overview render.
  const idsKey = useMemo(() => [...failedRunIds].sort().join(","), [failedRunIds]);
  const [reasons, setReasons] = useState<FailureReasons>(
    () => cache.get<FailureReasons>(cacheKey) ?? EMPTY_REASONS,
  );
  const request = useRef<AbortController | undefined>(undefined);

  const refresh = useCallback(() => {
    request.current?.abort();
    const dependencies: DataCacheDependency[] = [{ model: "instance" }, { model: "run" }];
    const cacheRevision = cache.beginWrite(cacheKey, dependencies);
    const controller = new AbortController();
    request.current = controller;
    return client
      .listTelemetryErrors({ limit: FAILURE_REASON_SCAN_LIMIT }, { signal: controller.signal })
      .then(
        (page) => {
          if (controller.signal.aborted) {
            return true;
          }
          const map: FailureReasons = new Map();
          for (const item of page.items) {
            // Items arrive newest-first; the first occurrence per run is the one
            // that failed it, so later (older) attempts do not overwrite it.
            if (item.runId && !map.has(item.runId)) {
              map.set(item.runId, item);
            }
          }
          cache.set(cacheKey, map, dependencies, cacheRevision);
          setReasons(map);
          return true;
        },
        () => false,
      );
  }, [cache, cacheKey, client]);

  useEffect(() => {
    if (idsKey === "") {
      return;
    }
    const unsubscribe = subscribe(["instance", "run"], (_models, reason) => {
      const cached = reason === "initial" ? cache.get<FailureReasons>(cacheKey) : undefined;
      if (cached) {
        setReasons(cached);
        return true;
      }
      return refresh();
    });
    return () => {
      unsubscribe();
      request.current?.abort();
    };
  }, [cache, cacheKey, idsKey, refresh, subscribe]);

  return reasons;
}
