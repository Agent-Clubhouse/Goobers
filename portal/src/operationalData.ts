import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type Dispatch,
  type SetStateAction,
} from "react";
import { MalformedResponseError } from "./api/errors";
import type {
  DaemonClient,
  Gaggle,
  Goober,
  Health,
  Instance,
  RunPhase,
  RunSummary,
  UpdateModel,
  WorkflowSummary,
} from "./api/types";
import type { QueryState } from "./api/queryState";
import { useLiveData, type LiveFreshness } from "./liveData";

const PAGE_LIMIT = 100;
const HEALTH_REFRESH_INTERVAL_MS = 5_000;

// The Overview is a bounded triage surface — active work, what needs attention,
// and a short window of recent outcomes — not a history browser
// (docs/design/dashboard.md §4.1, docs/requirements/portal.md PORT-004). Each
// group is sourced from a single server-side phase-filtered page so the load is
// O(1) small requests regardless of journal size (DASH-12). Full history lives
// on the Runs page (DASH-14).
const ACTIVE_RUN_LIMIT = 50;
const ATTENTION_RUN_LIMIT = 20;
const RECENT_OUTCOME_LIMIT = 20;

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

  usePeriodicHealth(client, freshness, setState);

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

// --- Bounded Overview (DASH-12 / DASH-13) --------------------------------
//
// The Overview reads only what it renders: pre-grouped, capped run lists plus
// the small inventory it needs to label runs and detect an empty instance. It
// deliberately does not reuse the full-history OperationalSnapshot above, which
// remains the data source for the Workflows inventory page.

export interface OverviewInventory {
  gaggleCount: number;
  // `${gaggle}/${workflow}` -> workflow display name, for labeling runs.
  workflowNames: Map<string, string>;
}

export interface OperationalOverview {
  health: Health;
  instance: Instance;
  gaggleCount: number;
  workflowNames: Map<string, string>;
  groups: OperationalRunGroups;
}

export interface OperationalOverviewQuery {
  retry: () => void;
  state: QueryState<OperationalOverview>;
}

export interface OverviewLoadOptions {
  previous?: OperationalOverview;
  models?: ReadonlySet<UpdateModel>;
}

export function workflowDisplayName(
  overview: Pick<OperationalOverview, "workflowNames">,
  run: RunSummary,
): string {
  return (
    overview.workflowNames.get(`${run.gaggle}/${run.workflow}`) ??
    `${run.gaggle} / ${run.workflow}`
  );
}

export function useOperationalOverview(client: DaemonClient): OperationalOverviewQuery {
  const [state, setState] = useState<QueryState<OperationalOverview>>({ status: "loading" });
  const request = useRef<AbortController | undefined>(undefined);
  const data = useRef<OperationalOverview | undefined>(undefined);
  const { freshness, isFresh, subscribe } = useLiveData();

  const load = useCallback(
    (models?: ReadonlySet<UpdateModel>) => {
      request.current?.abort();
      const controller = new AbortController();
      request.current = controller;
      setState((current) =>
        current.status === "ready" || current.status === "stale"
          ? { status: "stale", data: current.data }
          : { status: "loading" },
      );

      return loadOperationalOverview(client, controller.signal, {
        previous: data.current,
        models,
      }).then(
        (loaded) => {
          if (!controller.signal.aborted) {
            data.current = loaded;
            setState(isFresh() ? { status: "ready", data: loaded } : { status: "stale", data: loaded });
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
    },
    [client, isFresh],
  );

  useEffect(
    () => subscribe(["instance", "workflow", "run"], (models) => load(models)),
    [load, subscribe],
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

  const rememberOverview = useCallback((overview: OperationalOverview) => {
    data.current = overview;
  }, []);
  usePeriodicHealth(client, freshness, setState, rememberOverview);

  useEffect(() => () => request.current?.abort(), []);

  return { retry: () => void load(), state };
}

function usePeriodicHealth<T extends { health: Health }>(
  client: DaemonClient,
  freshness: LiveFreshness,
  setState: Dispatch<SetStateAction<QueryState<T>>>,
  onUpdate?: (data: T) => void,
): void {
  useEffect(() => {
    if (freshness !== "connected") {
      return;
    }

    let request: AbortController | undefined;
    let healthFailed = false;
    const refreshHealth = () => {
      request?.abort();
      const controller = new AbortController();
      request = controller;
      void client.getHealth({ signal: controller.signal }).then(
        (health) => {
          if (controller.signal.aborted) {
            return;
          }
          const recovered = healthFailed;
          healthFailed = false;
          setState((current) => {
            if (current.status !== "ready" && current.status !== "stale") {
              return current;
            }
            const updated = { ...current.data, health };
            onUpdate?.(updated);
            return current.status === "ready" || recovered
              ? { status: "ready", data: updated }
              : { status: "stale", data: updated, error: current.error };
          });
        },
        (error: unknown) => {
          if (controller.signal.aborted) {
            return;
          }
          healthFailed = true;
          const queryError =
            error instanceof Error ? error : new Error("Unable to read daemon health.");
          setState((current) =>
            current.status === "ready" || current.status === "stale"
              ? { status: "stale", data: current.data, error: queryError }
              : current,
          );
        },
      );
    };
    const timer = window.setInterval(refreshHealth, HEALTH_REFRESH_INTERVAL_MS);
    return () => {
      window.clearInterval(timer);
      request?.abort();
    };
  }, [client, freshness, onUpdate, setState]);
}

export async function loadOperationalOverview(
  client: DaemonClient,
  signal?: AbortSignal,
  options?: OverviewLoadOptions,
): Promise<OperationalOverview> {
  const previous = options?.previous;
  const models = options?.models;
  // Refetch proportional to the change (DASH-13): a run-only invalidation
  // rebuilds the bounded run groups but reuses the cached gaggle/workflow
  // inventory instead of re-paging it.
  const wantInventory =
    previous === undefined || models === undefined || models.has("instance") || models.has("workflow");
  const wantRuns = previous === undefined || models === undefined || models.has("run");
  const requestOptions = { signal };

  const [health, instance, inventory, groups] = await Promise.all([
    client.getHealth(requestOptions),
    client.getInstance(requestOptions),
    wantInventory
      ? loadOverviewInventory(client, signal)
      : Promise.resolve<OverviewInventory>({
          gaggleCount: previous!.gaggleCount,
          workflowNames: previous!.workflowNames,
        }),
    wantRuns ? loadOverviewRunGroups(client, signal) : Promise.resolve(previous!.groups),
  ]);

  return {
    health,
    instance,
    gaggleCount: inventory.gaggleCount,
    workflowNames: inventory.workflowNames,
    groups,
  };
}

async function loadOverviewInventory(
  client: DaemonClient,
  signal?: AbortSignal,
): Promise<OverviewInventory> {
  const gaggles = await collectPages((cursor) =>
    client.listGaggles({ cursor, limit: PAGE_LIMIT }, { signal }),
  );
  const workflowLists = await Promise.all(
    gaggles.map((gaggle) =>
      collectPages((cursor) =>
        client.listWorkflows(gaggle.name, { cursor, limit: PAGE_LIMIT }, { signal }),
      ),
    ),
  );
  const workflowNames = new Map<string, string>();
  for (const workflows of workflowLists) {
    for (const workflow of workflows) {
      workflowNames.set(
        `${workflow.identity.gaggle}/${workflow.identity.name}`,
        workflow.displayName,
      );
    }
  }
  return { gaggleCount: gaggles.length, workflowNames };
}

async function loadOverviewRunGroups(
  client: DaemonClient,
  signal?: AbortSignal,
): Promise<OperationalRunGroups> {
  const byPhase = (phase: RunPhase, limit: number) =>
    client.listRuns({ phase, limit }, { signal });
  const [running, escalated, failed, completed, aborted] = await Promise.all([
    byPhase("running", ACTIVE_RUN_LIMIT),
    byPhase("escalated", ATTENTION_RUN_LIMIT),
    byPhase("failed", ATTENTION_RUN_LIMIT),
    byPhase("completed", RECENT_OUTCOME_LIMIT),
    byPhase("aborted", RECENT_OUTCOME_LIMIT),
  ]);
  return {
    active: sortRuns(running.runs),
    attention: sortRuns([...escalated.runs, ...failed.runs]).slice(0, ATTENTION_RUN_LIMIT),
    recent: sortRuns([...completed.runs, ...aborted.runs]).slice(0, RECENT_OUTCOME_LIMIT),
  };
}
