import type { DaemonClient, TelemetryError } from "./api/types";

// One bounded scan of the telemetry error index is enough to label the Overview's
// capped attention list — the failures an operator can act on are recent, and the
// index is the same authoritative coded-reason source the Errors page reads.
const FAILURE_REASON_SCAN_LIMIT = 200;

/** runId -> the most recent coded failure the telemetry index recorded for it. */
export type FailureReasons = Map<string, TelemetryError>;

export async function loadFailureReasons(
  client: DaemonClient,
  signal?: AbortSignal,
): Promise<FailureReasons> {
  const page = await client.listTelemetryErrors(
    { limit: FAILURE_REASON_SCAN_LIMIT },
    { signal },
  );
  const reasons: FailureReasons = new Map();
  for (const item of page.items) {
    // Items arrive newest-first, so older errors for the same run do not
    // overwrite the failure that operators currently need to understand.
    if (item.runId && !reasons.has(item.runId)) {
      reasons.set(item.runId, item);
    }
  }
  return reasons;
}
