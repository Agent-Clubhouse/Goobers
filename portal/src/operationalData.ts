import { useCallback, useEffect, useRef, useState } from "react";
import { MalformedResponseError } from "./api/errors";
import type {
  DaemonClient,
  Gaggle,
  Goober,
  Health,
  Instance,
  RunPhase,
  RunSummary,
  WorkflowSummary,
} from "./api/types";
import type { QueryState } from "./api/queryState";
import { useLiveData } from "./liveData";

const PAGE_LIMIT = 100;
const attentionPhases = new Set<RunPhase>(["escalated", "failed"]);
const recentPhases = new Set<RunPhase>(["aborted", "completed"]);

export interface GaggleInventory {
  gaggle: Gaggle;
  goobers: Goober[];
  workflows: WorkflowSummary[];
}

export interface OperationalSnapshot {
  health: Health;
  instance: Instance;
  inventories: GaggleInventory[];
  runs: RunSummary[];
}

export interface OperationalRunGroups {
  active: RunSummary[];
  attention: RunSummary[];
  recent: RunSummary[];
}

export interface OperationalSnapshotQuery {
  retry: () => void;
  state: QueryState<OperationalSnapshot>;
}

export function useOperationalSnapshot(client: DaemonClient): OperationalSnapshotQuery {
  const [state, setState] = useState<QueryState<OperationalSnapshot>>({ status: "loading" });
  const request = useRef<AbortController | undefined>(undefined);
  const { freshness, isFresh, subscribe } = useLiveData();

  const refresh = useCallback(() => {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setState((current) =>
      current.status === "ready" || current.status === "stale"
        ? { status: "stale", data: current.data }
        : { status: "loading" },
    );

    return loadOperationalSnapshot(client, controller.signal).then(
      (data) => {
        if (!controller.signal.aborted) {
          setState(isFresh() ? { status: "ready", data } : { status: "stale", data });
        }
        return true;
      },
      (error: unknown) => {
        if (!controller.signal.aborted) {
          const queryError =
            error instanceof Error ? error : new Error("Unable to read daemon data.");
          setState((current) =>
            current.status === "ready" || current.status === "stale"
              ? { status: "stale", data: current.data, error: queryError }
              : { status: "error", error: queryError },
          );
        }
        return false;
      },
    );
  }, [client, isFresh]);

  useEffect(
    () => subscribe(["instance", "workflow", "run"], refresh),
    [refresh, subscribe],
  );

  useEffect(() => {
    setState((current) => {
      if (freshness !== "connected" && current.status === "ready") {
        return { status: "stale", data: current.data };
      }
      if (freshness === "connected" && current.status === "stale" && !current.error) {
        return { status: "ready", data: current.data };
      }
      return current;
    });
  }, [freshness]);

  useEffect(() => () => request.current?.abort(), []);

  return { retry: refresh, state };
}

export async function loadOperationalSnapshot(
  client: DaemonClient,
  signal?: AbortSignal,
): Promise<OperationalSnapshot> {
  const options = { signal };
  const [health, instance, gaggles, runs] = await Promise.all([
    client.getHealth(options),
    client.getInstance(options),
    collectPages((cursor) => client.listGaggles({ cursor, limit: PAGE_LIMIT }, options)),
    collectRuns(client, signal),
  ]);

  const inventories = await Promise.all(
    gaggles.map(async (gaggle) => {
      const [goobers, workflows] = await Promise.all([
        collectPages((cursor) =>
          client.listGoobers(gaggle.name, { cursor, limit: PAGE_LIMIT }, options),
        ),
        collectPages((cursor) =>
          client.listWorkflows(gaggle.name, { cursor, limit: PAGE_LIMIT }, options),
        ),
      ]);
      return { gaggle, goobers, workflows };
    }),
  );

  return { health, instance, inventories, runs: sortRuns(runs) };
}

export function groupOperationalRuns(runs: RunSummary[]): OperationalRunGroups {
  return {
    attention: runs.filter((run) => attentionPhases.has(run.phase)),
    active: runs.filter((run) => run.phase === "running"),
    recent: runs.filter((run) => recentPhases.has(run.phase)),
  };
}

export function latestWorkflowOutcome(
  runs: RunSummary[],
  gaggle: string,
  workflow: string,
): RunSummary | undefined {
  return runs.find(
    (run) =>
      run.gaggle === gaggle &&
      run.workflow === workflow &&
      run.phase !== "running" &&
      run.terminal,
  );
}

async function collectPages<T>(
  request: (cursor?: string) => Promise<{ items: T[]; page: { hasMore: boolean; nextCursor: string } }>,
): Promise<T[]> {
  const items: T[] = [];
  const seenCursors = new Set<string>();
  let cursor: string | undefined;

  for (;;) {
    const response = await request(cursor);
    items.push(...response.items);
    if (!response.page.hasMore) {
      return items;
    }
    cursor = nextCursor(response.page.nextCursor, seenCursors);
  }
}

async function collectRuns(client: DaemonClient, signal?: AbortSignal): Promise<RunSummary[]> {
  const runs: RunSummary[] = [];
  const seenCursors = new Set<string>();
  let cursor: string | undefined;

  for (;;) {
    const response = await client.listRuns(
      { cursor, limit: PAGE_LIMIT },
      { signal },
    );
    runs.push(...response.runs);
    if (!response.nextCursor) {
      return runs;
    }
    cursor = nextCursor(response.nextCursor, seenCursors);
  }
}

function nextCursor(value: string, seen: Set<string>): string {
  if (!value || seen.has(value)) {
    throw new MalformedResponseError("The daemon returned an invalid pagination cursor.");
  }
  seen.add(value);
  return value;
}

function sortRuns(runs: RunSummary[]): RunSummary[] {
  return [...runs].sort(
    (left, right) =>
      Date.parse(right.finishedAt ?? right.startedAt) -
        Date.parse(left.finishedAt ?? left.startedAt) ||
      right.id.localeCompare(left.id),
  );
}
