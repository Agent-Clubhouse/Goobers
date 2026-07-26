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
  // `${gaggle}/${workflow}` -> most recent terminal run, if any.
  latestOutcomes: Map<string, RunSummary>;
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

type OperationalRefresh = (models?: ReadonlySet<UpdateModel>) => Promise<boolean>;

function useCoalescedOperationalRefresh(
  task: (models: ReadonlySet<UpdateModel> | undefined, signal: AbortSignal) => Promise<boolean>,
): OperationalRefresh {
  const taskRef = useRef(task);
  taskRef.current = task;
  const active = useRef<
    { controller: AbortController; promise: Promise<boolean> } | undefined
  >(undefined);
  const pending = useRef(false);
  const pendingAll = useRef(false);
  const pendingModels = useRef(new Set<UpdateModel>());
  const enabled = useRef(true);
  const lifecycle = useRef(0);

  const refresh = useCallback((models?: ReadonlySet<UpdateModel>) => {
    if (!enabled.current) {
      return Promise.resolve(false);
    }

    pending.current = true;
    if (models === undefined) {
      pendingAll.current = true;
      pendingModels.current.clear();
    } else if (!pendingAll.current) {
      for (const model of models) {
        pendingModels.current.add(model);
      }
    }

    if (active.current) {
      return active.current.promise;
    }

    const operation = {
      controller: new AbortController(),
      lifecycle: lifecycle.current,
      promise: Promise.resolve(false),
      task: taskRef.current,
    };
    const promise = (async () => {
      let refreshed = false;
      while (
        enabled.current &&
        lifecycle.current === operation.lifecycle &&
        pending.current
      ) {
        const controller = new AbortController();
        operation.controller = controller;
        const models = pendingAll.current ? undefined : new Set(pendingModels.current);
        pending.current = false;
        pendingAll.current = false;
        pendingModels.current.clear();
        refreshed = await operation.task(models, controller.signal);
      }
      return refreshed;
    })().finally(() => {
      if (active.current === operation) {
        active.current = undefined;
      }
    });
    operation.promise = promise;
    active.current = operation;
    return promise;
  }, []);

  useEffect(() => {
    enabled.current = true;
    return () => {
      enabled.current = false;
      lifecycle.current += 1;
      pending.current = false;
      pendingAll.current = false;
      pendingModels.current.clear();
      const operation = active.current;
      active.current = undefined;
      operation?.controller.abort();
    };
  }, [task]);

  return refresh;
}

export function useOperationalSnapshot(client: DaemonClient): OperationalSnapshotQuery {
  const [state, setState] = useState<QueryState<OperationalSnapshot>>({ status: "loading" });
  const { freshness, isFresh, subscribe } = useLiveData();

  const performRefresh = useCallback(
    async (_models: ReadonlySet<UpdateModel> | undefined, signal: AbortSignal) => {
      setState((current) =>
        current.status === "ready" || current.status === "stale"
          ? { status: "stale", data: current.data }
          : { status: "loading" },
      );

      try {
        const data = await loadOperationalSnapshot(client, signal);
        if (signal.aborted) {
          return false;
        }
        setState(isFresh() ? { status: "ready", data } : { status: "stale", data });
        return true;
      } catch (error: unknown) {
        if (signal.aborted) {
          return false;
        }
        const queryError =
          error instanceof Error ? error : new Error("Unable to read daemon data.");
        setState((current) =>
          current.status === "ready" || current.status === "stale"
            ? { status: "stale", data: current.data, error: queryError }
            : { status: "error", error: queryError },
        );
        return false;
      }
    },
    [client, isFresh],
  );
  const refresh = useCoalescedOperationalRefresh(performRefresh);

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

  return { retry: () => void refresh(), state };
}

export async function loadOperationalSnapshot(
  client: DaemonClient,
  signal?: AbortSignal,
): Promise<OperationalSnapshot> {
  const options = { signal };
  const [health, instance, gaggles] = await Promise.all([
    client.getHealth(options),
    client.getInstance(options),
    collectPages((cursor) => client.listGaggles({ cursor, limit: PAGE_LIMIT }, options)),
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

  const latestOutcomes = await loadLatestWorkflowOutcomes(client, inventories, options);

  return { health, instance, inventories, latestOutcomes };
}

// The Workflow inventory only ever renders one run per workflow — its most
// recent terminal outcome — so it is fetched with one small, workflow-scoped
// query per workflow rather than paging the daemon's entire run history
// (which used to make this page O(all runs ever) and time out on busy
// instances, the same bug class DASH-12/#1367 fixed for the Overview).
const OUTCOME_LOOKBACK_LIMIT = 5;

async function loadLatestWorkflowOutcomes(
  client: DaemonClient,
  inventories: GaggleInventory[],
  options: { signal?: AbortSignal },
): Promise<Map<string, RunSummary>> {
  const identities = inventories.flatMap((inventory) =>
    inventory.workflows.map((workflow) => workflow.identity),
  );

  const outcomes = await Promise.all(
    identities.map(async (identity) => {
      const { runs } = await client.listRuns(
        { gaggle: identity.gaggle, workflow: identity.name, limit: OUTCOME_LOOKBACK_LIMIT },
        options,
      );
      const outcome = runs.find((run) => run.terminal && run.phase !== "running");
      return outcome ? ([workflowKey(identity.gaggle, identity.name), outcome] as const) : null;
    }),
  );

  return new Map(outcomes.filter((entry): entry is readonly [string, RunSummary] => entry !== null));
}

export function workflowKey(gaggle: string, workflow: string): string {
  return `${gaggle}/${workflow}`;
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
// deliberately does not reuse the OperationalSnapshot above, which has its own
// bounded, workflow-scoped outcome lookup for the Workflows inventory page.

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
  const data = useRef<OperationalOverview | undefined>(undefined);
  const { freshness, isFresh, subscribe } = useLiveData();

  const performLoad = useCallback(
    async (models: ReadonlySet<UpdateModel> | undefined, signal: AbortSignal) => {
      setState((current) =>
        current.status === "ready" || current.status === "stale"
          ? { status: "stale", data: current.data }
          : { status: "loading" },
      );

      try {
        const loaded = await loadOperationalOverview(client, signal, {
          previous: data.current,
          models,
        });
        if (signal.aborted) {
          return false;
        }
        data.current = loaded;
        setState(
          isFresh() ? { status: "ready", data: loaded } : { status: "stale", data: loaded },
        );
        return true;
      } catch (error: unknown) {
        if (signal.aborted) {
          return false;
        }
        const queryError =
          error instanceof Error ? error : new Error("Unable to read daemon data.");
        setState((current) =>
          current.status === "ready" || current.status === "stale"
            ? { status: "stale", data: current.data, error: queryError }
            : { status: "error", error: queryError },
        );
        return false;
      }
    },
    [client, isFresh],
  );
  const load = useCoalescedOperationalRefresh(performLoad);

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
