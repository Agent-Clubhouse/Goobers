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
  ModelInvalidation,
  RepositoryConnection,
  RunPhase,
  RunSummary,
  UpdateModel,
  WorkflowRunActivity,
  WorkflowSummary,
} from "./api/types";
import type { QueryState } from "./api/queryState";
import {
  dataCacheKey,
  type DataCacheDependency,
  type SessionDataCache,
} from "./dataCache";
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
// Positional companion to the phase queries loadOverviewRunGroups settles, so
// a rejected result can be named in the incomplete-data report (#3658).
const OVERVIEW_RUN_PHASES: readonly RunPhase[] = [
  "running",
  "escalated",
  "failed",
  "completed",
  "aborted",
];
// An escalated/failed run is a candidate for the attention list only while it
// has been touched — stage.started / stage.finished / escalated — in the last
// 24h. Escalation is a permanent terminal phase with no dismissal otherwise,
// so without this window a single old, still-unresolved escalation would sit
// on the list forever until 20 newer ones happened to push it off (#1199).
// This is additive filtering on top of the existing durable dismiss
// (attentionDismissals.ts, #2563), not a replacement for it.
const ATTENTION_RECENCY_WINDOW_MS = 24 * 60 * 60 * 1000;
const OPERATIONAL_OVERVIEW_CACHE_KEY = dataCacheKey("operational-overview");
export const INVENTORY_CACHE_TTL_MS = 5 * 60_000;
const OPERATIONAL_DEPENDENCIES: readonly DataCacheDependency[] = [
  { model: "instance" },
  { model: "workflow" },
  { model: "run" },
];
const INVENTORY_DEPENDENCIES: readonly DataCacheDependency[] = [
  { model: "inventory" },
];
const GAGGLE_ACTIVITY_DEPENDENCIES: readonly DataCacheDependency[] = [
  { model: "run" },
  { model: "workflow" },
];

// Inventory endpoints mix definition data with run-derived counters. Keep the
// definitions reusable while allowing targeted run events to evict the counters.
type GaggleDefinition = Omit<Gaggle, "activeRunCount">;
interface WorkflowDefinitionSummary extends Omit<WorkflowSummary, "concurrency"> {
  concurrency: Omit<WorkflowSummary["concurrency"], "activeRuns">;
}

export interface GaggleInventory {
  gaggle: Gaggle;
  goobers: Goober[];
  workflows: WorkflowSummary[];
  connections: RepositoryConnection[];
}

export interface OperationalSnapshot {
  health: Health;
  instance: Instance;
  inventories: GaggleInventory[];
  runs: RunSummary[];
}

/**
 * Phases whose query failed while sibling phases succeeded (#3658). The groups
 * they feed are rendered from whatever prior data survived, so the page must
 * say the list is incomplete rather than pass it off as an idle instance.
 */
export interface IncompleteRunPhases {
  phases: RunPhase[];
  error: Error;
}

export interface OperationalRunGroups {
  active: RunSummary[];
  attention: RunSummary[];
  recent: RunSummary[];
  incomplete?: IncompleteRunPhases;
}

/** One-line, actionable description of a partially unreadable run list (#3658). */
export function incompleteRunPhasesMessage(incomplete: IncompleteRunPhases): string {
  const label = incomplete.phases.length === 1 ? "phase" : "phases";
  return `Run activity for the ${incomplete.phases.join(", ")} ${label} could not be read just now (${incomplete.error.message}), so these groups may be incomplete or out of date.`;
}

export interface OperationalSnapshotQuery {
  retry: () => void;
  state: QueryState<OperationalSnapshot>;
}

export interface OperationalScope {
  gaggle?: string;
  workflow?: string;
}

export interface SnapshotLoadOptions {
  cache?: SessionDataCache;
  previous?: OperationalSnapshot;
  models?: ReadonlySet<UpdateModel>;
  scope?: OperationalScope;
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

export function useOperationalSnapshot(
  client: DaemonClient,
  scope: OperationalScope = {},
): OperationalSnapshotQuery {
  const { cache, freshness, isFresh, subscribe } = useLiveData();
  const gaggle = scope.gaggle;
  const workflow = scope.workflow;
  const cacheKey = operationalSnapshotCacheKey(scope);
  const dependencies = operationalSnapshotDependencies(scope);
  const cached = cache.get<OperationalSnapshot>(cacheKey);
  const [state, setState] = useState<QueryState<OperationalSnapshot>>(() => {
    return cached ? { status: "ready", data: cached } : { status: "loading" };
  });
  const data = useRef<OperationalSnapshot | undefined>(cached);

  const performRefresh = useCallback(
    async (models: ReadonlySet<UpdateModel> | undefined, signal: AbortSignal) => {
      const cacheRevision = cache.beginWrite(cacheKey, dependencies);
      setState((current) =>
        current.status === "ready" || current.status === "stale"
          ? { status: "stale", data: current.data }
          : { status: "loading" },
      );

      try {
        const loaded = await loadOperationalSnapshot(client, signal, {
          cache,
          previous: data.current,
          models,
          scope: { gaggle, workflow },
        });
        if (signal.aborted) {
          return false;
        }
        data.current = loaded;
        cache.set(
          cacheKey,
          loaded,
          dependencies,
          cacheRevision,
        );
        setState(isFresh() ? { status: "ready", data: loaded } : { status: "stale", data: loaded });
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
    [cache, cacheKey, client, gaggle, isFresh, workflow],
  );
  const refresh = useCoalescedOperationalRefresh(performRefresh);

  useEffect(
    () =>
      subscribe(
        ["instance", "workflow", "run"],
        (models, reason, invalidations) => {
          const cached =
            reason === "initial"
              ? cache.get<OperationalSnapshot>(cacheKey)
              : undefined;
          if (cached) {
            data.current = cached;
            setState(
              isFresh() ? { status: "ready", data: cached } : { status: "stale", data: cached },
            );
            return true;
          }
          return refresh(operationalSnapshotModels(models, invalidations));
        },
        { gaggle, workflow },
      ),
    [cache, cacheKey, gaggle, isFresh, refresh, subscribe, workflow],
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

  const rememberSnapshot = useCallback((snapshot: OperationalSnapshot) => {
    data.current = snapshot;
  }, []);
  usePeriodicHealth(client, freshness, setState, rememberSnapshot);

  return {
    retry: () => {
      cache.remove(cacheKey);
      void refresh();
    },
    state,
  };
}

function operationalSnapshotModels(
  models: ReadonlySet<UpdateModel>,
  invalidations: readonly ModelInvalidation[] | undefined,
): ReadonlySet<UpdateModel> {
  if (
    invalidations?.length &&
    invalidations.every(
      (invalidation) =>
        invalidation.models.includes("run") &&
        invalidation.runIds !== undefined &&
        invalidation.runIds.length > 0,
    )
  ) {
    return new Set(["run"]);
  }
  return models;
}

export async function loadOperationalSnapshot(
  client: DaemonClient,
  signal?: AbortSignal,
  options?: SnapshotLoadOptions,
): Promise<OperationalSnapshot> {
  const previous = options?.previous;
  const models = options?.models;
  const wantInventory =
    previous === undefined || models === undefined || models.has("instance") || models.has("workflow");
  const wantRuns = previous === undefined || models === undefined || models.has("run");
  const requestOptions = { signal };
  const [health, instance, inventories, workflowSnapshot] = await Promise.all([
    client.getHealth(requestOptions),
    client.getInstance(requestOptions),
    wantInventory
      ? loadOperationalInventory(client, options?.cache, signal, options?.scope)
      : Promise.resolve(previous!.inventories),
    wantRuns
      ? loadWorkflowOutcomes(client, options?.scope, signal)
      : Promise.resolve({ runs: previous!.runs, activity: undefined }),
  ]);

  return {
    health,
    instance,
    inventories: workflowSnapshot.activity
      ? applyWorkflowActivity(inventories, workflowSnapshot.activity)
      : inventories,
    runs: sortRuns(workflowSnapshot.runs),
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

async function loadWorkflowOutcomes(
  client: DaemonClient,
  scope?: OperationalScope,
  signal?: AbortSignal,
): Promise<{ runs: RunSummary[]; activity: WorkflowRunActivity[] | undefined }> {
  const response = await client.listRuns(
    {
      gaggle: scope?.gaggle,
      workflow: scope?.workflow,
      latestPerWorkflow: true,
    },
    { signal },
  );
  return { runs: response.runs, activity: response.workflowActivity ?? [] };
}

function applyWorkflowActivity(
  inventories: GaggleInventory[],
  activity: WorkflowRunActivity[],
): GaggleInventory[] {
  const activeRuns = new Map(
    activity.map((item) => [`${item.gaggle}\u0000${item.workflow}`, item.activeRuns]),
  );
  const gaggleActiveRuns = new Map<string, number>();
  for (const item of activity) {
    gaggleActiveRuns.set(
      item.gaggle,
      (gaggleActiveRuns.get(item.gaggle) ?? 0) + item.activeRuns,
    );
  }
  return inventories.map((inventory) => {
    const workflows = inventory.workflows.map((workflow) => {
      const workflowActiveRuns =
        activeRuns.get(
          `${workflow.identity.gaggle}\u0000${workflow.identity.name}`,
        ) ?? 0;
      return {
        ...workflow,
        concurrency: {
          ...workflow.concurrency,
          activeRuns: workflowActiveRuns,
        },
      };
    });
    return {
      ...inventory,
      gaggle: {
        ...inventory.gaggle,
        activeRunCount: gaggleActiveRuns.get(inventory.gaggle.name) ?? 0,
      },
      workflows,
    };
  });
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

// The attention list orders by last journal activity, not start/finish
// (#1199): an escalation that started long ago but was just touched is the
// most urgent thing on the instance, and the least recent by sortRuns' axis.
function sortRunsByActivity(runs: RunSummary[]): RunSummary[] {
  return [...runs].sort(
    (left, right) =>
      Date.parse(right.lastActivityAt) - Date.parse(left.lastActivityAt) ||
      right.id.localeCompare(left.id),
  );
}

// --- Bounded Overview (DASH-12 / DASH-13) --------------------------------
//
// The Overview reads only what it renders: pre-grouped, capped run lists plus
// the small inventory it needs to label runs and detect an empty instance. It
// deliberately does not reuse the workflow-scoped OperationalSnapshot above,
// which remains the data source for the Workflows inventory page.

export interface OverviewInventory {
  gaggleCount: number;
  // `${gaggle}/${workflow}` -> workflow display name, for labeling runs.
  workflowNames: Map<string, string>;
}

/**
 * Sections that failed to load while the rest of the Overview stayed valid.
 * A section listed here is rendering either its previous value or an empty
 * placeholder, and the page is expected to say so.
 */
export interface OverviewSectionErrors {
  inventory?: Error;
  runs?: Error;
}

export interface OperationalOverview {
  health: Health;
  instance: Instance;
  gaggleCount: number;
  workflowNames: Map<string, string>;
  groups: OperationalRunGroups;
  // Present only when part of the Overview could not be read. Everything else
  // on the object is still authoritative and renderable (#1709).
  sectionErrors?: OverviewSectionErrors;
}

export interface OperationalOverviewQuery {
  retry: () => void;
  state: QueryState<OperationalOverview>;
}

export interface OverviewLoadOptions {
  cache?: SessionDataCache;
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
  const { cache, freshness, isFresh, subscribe } = useLiveData();
  const cached = cache.get<OperationalOverview>(OPERATIONAL_OVERVIEW_CACHE_KEY);
  const [state, setState] = useState<QueryState<OperationalOverview>>(() =>
    cached ? { status: "ready", data: cached } : { status: "loading" },
  );
  const data = useRef<OperationalOverview | undefined>(cached);

  const performLoad = useCallback(
    async (models: ReadonlySet<UpdateModel> | undefined, signal: AbortSignal) => {
      const cacheRevision = cache.beginWrite(
        OPERATIONAL_OVERVIEW_CACHE_KEY,
        OPERATIONAL_DEPENDENCIES,
      );
      setState((current) =>
        current.status === "ready" || current.status === "stale"
          ? { status: "stale", data: current.data }
          : { status: "loading" },
      );

      try {
        const loaded = await loadOperationalOverview(client, signal, {
          cache,
          previous: data.current,
          models,
        });
        if (signal.aborted) {
          return false;
        }
        data.current = loaded;
        cache.set(
          OPERATIONAL_OVERVIEW_CACHE_KEY,
          loaded,
          OPERATIONAL_DEPENDENCIES,
          cacheRevision,
        );
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
    [cache, client, isFresh],
  );
  const load = useCoalescedOperationalRefresh(performLoad);

  useEffect(
    () =>
      subscribe(["instance", "workflow", "run"], (models, reason) => {
        const cached =
          reason === "initial"
            ? cache.get<OperationalOverview>(OPERATIONAL_OVERVIEW_CACHE_KEY)
            : undefined;
        if (cached) {
          data.current = cached;
          setState(
            isFresh() ? { status: "ready", data: cached } : { status: "stale", data: cached },
          );
          return true;
        }
        return load(models);
      }),
    [cache, isFresh, load, subscribe],
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

  return {
    retry: () => {
      cache.remove(OPERATIONAL_OVERVIEW_CACHE_KEY);
      void load();
    },
    state,
  };
}

// --- Gaggle list (sidebar nav / switcher) --------------------------------
//
// A small, definition-only projection of the gaggle inventory: just enough to
// render "which gaggles exist" chrome (sidebar nav, in-page switcher) without
// paying for every gaggle's goobers/workflows/connections the way the
// unscoped OperationalSnapshot does.

export interface GaggleSummary {
  name: string;
  displayName: string;
  status: Gaggle["status"];
}

export interface GaggleListQuery {
  retry: () => void;
  state: QueryState<GaggleSummary[]>;
}

const GAGGLE_LIST_CACHE_KEY = dataCacheKey("gaggle-list");
const GAGGLE_LIST_DEPENDENCIES: readonly DataCacheDependency[] = [
  { model: "instance" },
  { model: "workflow" },
];

export function useGaggleList(client: DaemonClient): GaggleListQuery {
  const { cache, freshness, isFresh, subscribe } = useLiveData();
  const cached = cache.get<GaggleSummary[]>(GAGGLE_LIST_CACHE_KEY);
  const [state, setState] = useState<QueryState<GaggleSummary[]>>(() =>
    cached ? { status: "ready", data: cached } : { status: "loading" },
  );
  const data = useRef<GaggleSummary[] | undefined>(cached);

  const performLoad = useCallback(
    async (_models: ReadonlySet<UpdateModel> | undefined, signal: AbortSignal) => {
      const cacheRevision = cache.beginWrite(GAGGLE_LIST_CACHE_KEY, GAGGLE_LIST_DEPENDENCIES);
      setState((current) =>
        current.status === "ready" || current.status === "stale"
          ? { status: "stale", data: current.data }
          : { status: "loading" },
      );

      try {
        const gaggles = await loadGaggleDefinitions(client, cache, signal);
        const loaded = gaggles.map((gaggle) => ({
          name: gaggle.name,
          displayName: gaggle.displayName,
          status: gaggle.status,
        }));
        if (signal.aborted) {
          return false;
        }
        data.current = loaded;
        cache.set(GAGGLE_LIST_CACHE_KEY, loaded, GAGGLE_LIST_DEPENDENCIES, cacheRevision);
        setState(isFresh() ? { status: "ready", data: loaded } : { status: "stale", data: loaded });
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
    [cache, client, isFresh],
  );
  const load = useCoalescedOperationalRefresh(performLoad);

  useEffect(
    () =>
      subscribe(["instance", "workflow"], (models, reason) => {
        const cached =
          reason === "initial" ? cache.get<GaggleSummary[]>(GAGGLE_LIST_CACHE_KEY) : undefined;
        if (cached) {
          data.current = cached;
          setState(
            isFresh() ? { status: "ready", data: cached } : { status: "stale", data: cached },
          );
          return true;
        }
        return load(models);
      }),
    [cache, isFresh, load, subscribe],
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

  return {
    retry: () => {
      cache.remove(GAGGLE_LIST_CACHE_KEY);
      void load();
    },
    state,
  };
}

// --- Gaggle-scoped activity (GagglePage "what's happening now") ----------
//
// GagglePage's OperationalSnapshot only carries the latest run per workflow
// (enough for a per-workflow status badge). Surfacing "what's running now"
// and "recent outcomes" for the gaggle as a whole needs the same
// active/attention/recent grouping the Overview uses, scoped to one gaggle
// instead of the whole instance.

export interface GaggleActivity {
  active: RunSummary[];
  recent: RunSummary[];
  incomplete?: IncompleteRunPhases;
}

export interface GaggleActivityQuery {
  retry: () => void;
  state: QueryState<GaggleActivity>;
}

const GAGGLE_ACTIVE_RUN_LIMIT = 20;
const GAGGLE_RECENT_OUTCOME_LIMIT = 10;

function gaggleActivityCacheKey(gaggleName: string): string {
  return dataCacheKey("gaggle-activity", gaggleName);
}

export function useGaggleActivity(client: DaemonClient, gaggleName: string): GaggleActivityQuery {
  const { cache, freshness, isFresh, subscribe } = useLiveData();
  const cacheKey = gaggleActivityCacheKey(gaggleName);
  const cached = cache.get<GaggleActivity>(cacheKey);
  const [state, setState] = useState<QueryState<GaggleActivity>>(() =>
    cached ? { status: "ready", data: cached } : { status: "loading" },
  );
  const data = useRef<GaggleActivity | undefined>(cached);

  const performLoad = useCallback(
    async (_models: ReadonlySet<UpdateModel> | undefined, signal: AbortSignal) => {
      const cacheRevision = cache.beginWrite(cacheKey, GAGGLE_ACTIVITY_DEPENDENCIES);
      setState((current) =>
        current.status === "ready" || current.status === "stale"
          ? { status: "stale", data: current.data }
          : { status: "loading" },
      );

      try {
        const loaded = await loadGaggleActivity(client, gaggleName, signal, data.current);
        if (signal.aborted) {
          return false;
        }
        data.current = loaded;
        cache.set(cacheKey, loaded, GAGGLE_ACTIVITY_DEPENDENCIES, cacheRevision);
        setState(isFresh() ? { status: "ready", data: loaded } : { status: "stale", data: loaded });
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
    [cache, cacheKey, client, gaggleName, isFresh],
  );
  const load = useCoalescedOperationalRefresh(performLoad);

  useEffect(
    () =>
      subscribe(
        ["instance", "workflow", "run"],
        (models, reason) => {
          const cached = reason === "initial" ? cache.get<GaggleActivity>(cacheKey) : undefined;
          if (cached) {
            data.current = cached;
            setState(
              isFresh() ? { status: "ready", data: cached } : { status: "stale", data: cached },
            );
            return true;
          }
          return load(models);
        },
        { gaggle: gaggleName },
      ),
    [cache, cacheKey, gaggleName, isFresh, load, subscribe],
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

  return {
    retry: () => {
      cache.remove(cacheKey);
      void load();
    },
    state,
  };
}

async function loadGaggleActivity(
  client: DaemonClient,
  gaggleName: string,
  signal?: AbortSignal,
  previous?: GaggleActivity,
): Promise<GaggleActivity> {
  const byPhase = (phase: RunPhase, limit: number) =>
    client.listRuns({ gaggle: gaggleName, phase, limit }, { signal });
  // Mirrors loadOverviewRunGroups' per-phase settling (#1709): one slow phase
  // must not blank the other groups on this gaggle's page.
  const phases = OVERVIEW_RUN_PHASES;
  const settled = await Promise.allSettled([
    byPhase("running", GAGGLE_ACTIVE_RUN_LIMIT),
    byPhase("escalated", GAGGLE_RECENT_OUTCOME_LIMIT),
    byPhase("failed", GAGGLE_RECENT_OUTCOME_LIMIT),
    byPhase("completed", GAGGLE_RECENT_OUTCOME_LIMIT),
    byPhase("aborted", GAGGLE_RECENT_OUTCOME_LIMIT),
  ]);
  const firstError = settled.find((result) => result.status === "rejected");
  if (firstError && settled.every((result) => result.status === "rejected")) {
    throw settledError(firstError) ?? new Error("Unable to read runs.");
  }
  const previousRuns = previous ? [...previous.active, ...previous.recent] : [];
  const [running, escalated, failed, completed, aborted] = settled.map((result, index) =>
    resolvePhaseRuns(result, phases[index], previousRuns),
  );
  const incomplete = incompletePhases(settled, phases);
  return {
    active: sortRuns(running),
    recent: sortRuns([...escalated, ...failed, ...completed, ...aborted]).slice(
      0,
      GAGGLE_RECENT_OUTCOME_LIMIT,
    ),
    ...(incomplete ? { incomplete } : {}),
  };
}

/**
 * A phase whose query failed falls back to the runs the caller already had for
 * that phase: dropping them silently is what let the UI claim an instance had
 * no active or failed runs when the phase was merely unreadable (#3658).
 */
function resolvePhaseRuns(
  result: PromiseSettledResult<{ runs: RunSummary[] }>,
  phase: RunPhase,
  previousRuns: readonly RunSummary[],
): RunSummary[] {
  const runs = settledValue(result)?.runs;
  if (runs) {
    return runs;
  }
  return previousRuns.filter((run) => run.phase === phase);
}

function incompletePhases(
  settled: readonly PromiseSettledResult<unknown>[],
  phases: readonly RunPhase[],
): IncompleteRunPhases | undefined {
  const rejected = phases.filter((_, index) => settled[index].status === "rejected");
  if (rejected.length === 0) {
    return undefined;
  }
  const error =
    settledError(settled.find((result) => result.status === "rejected")!) ??
    new Error("Unable to read runs.");
  return { phases: rejected, error };
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

  // Settle the four sections independently. They are separately renderable, and
  // failing the whole Overview because one of them is slow is what turned a
  // degraded daemon into a blank "Daemon unavailable" page: during #1708 the
  // run queries timed out while health, instance and the inventory had all
  // returned 200, and the operator saw none of it. Health and instance are
  // precisely what say *what* is wrong, so they must survive a run-list
  // timeout (#1709).
  const [health, instance, inventory, groups] = await Promise.allSettled([
    client.getHealth(requestOptions),
    client.getInstance(requestOptions),
    wantInventory
      ? loadOverviewInventory(client, options?.cache, signal)
      : Promise.resolve<OverviewInventory>({
          gaggleCount: previous!.gaggleCount,
          workflowNames: previous!.workflowNames,
        }),
    wantRuns ? loadOverviewRunGroups(client, signal, previous?.groups) : Promise.resolve(previous!.groups),
  ]);

  // An aborted request is not a degraded section — the caller is discarding this
  // load entirely — so fail fast rather than reporting every section as broken.
  if (signal?.aborted) {
    throw settledError(health) ?? new Error("Overview load was aborted.");
  }

  const resolvedHealth = settledValue(health) ?? previous?.health;
  const resolvedInstance = settledValue(instance) ?? previous?.instance;
  // With neither a fresh nor a previous value for the identity reads there is
  // nothing to render, so this genuinely is a page-level failure.
  if (resolvedHealth === undefined || resolvedInstance === undefined) {
    throw (
      settledError(health) ??
      settledError(instance) ??
      new Error("Unable to read daemon data.")
    );
  }

  const resolvedInventory = settledValue(inventory) ?? {
    gaggleCount: previous?.gaggleCount ?? 0,
    workflowNames: previous?.workflowNames ?? new Map<string, string>(),
  };
  const resolvedGroups = settledValue(groups) ??
    previous?.groups ?? { active: [], attention: [], recent: [] };

  const sectionErrors: OverviewSectionErrors = {};
  const inventoryError = settledError(inventory);
  const runsError = settledError(groups);
  if (inventoryError) {
    sectionErrors.inventory = inventoryError;
  }
  if (runsError) {
    sectionErrors.runs = runsError;
  }

  return {
    health: resolvedHealth,
    instance: resolvedInstance,
    gaggleCount: resolvedInventory.gaggleCount,
    workflowNames: resolvedInventory.workflowNames,
    groups: resolvedGroups,
    ...(inventoryError || runsError ? { sectionErrors } : {}),
  };
}

function settledValue<T>(result: PromiseSettledResult<T>): T | undefined {
  return result.status === "fulfilled" ? result.value : undefined;
}

function settledError(result: PromiseSettledResult<unknown>): Error | undefined {
  if (result.status !== "rejected") {
    return undefined;
  }
  return result.reason instanceof Error
    ? result.reason
    : new Error("Unable to read daemon data.");
}

async function loadOverviewInventory(
  client: DaemonClient,
  cache?: SessionDataCache,
  signal?: AbortSignal,
): Promise<OverviewInventory> {
  const gaggles = await loadGaggleDefinitions(client, cache, signal);
  const workflowLists = await Promise.all(
    gaggles.map((gaggle) => loadWorkflowDefinitions(client, gaggle.name, cache, signal)),
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

async function loadOperationalInventory(
  client: DaemonClient,
  cache?: SessionDataCache,
  signal?: AbortSignal,
  scope?: OperationalScope,
): Promise<GaggleInventory[]> {
  const gaggles = await loadGaggles(client, cache, signal);
  const scopedGaggles = scope?.gaggle
    ? gaggles.filter((gaggle) => gaggle.name === scope.gaggle)
    : gaggles;
  return Promise.all(
    scopedGaggles.map(async (gaggle) => {
      const [goobers, workflows, connections] = await Promise.all([
        loadGoobers(client, gaggle.name, cache, signal),
        loadWorkflows(client, gaggle.name, cache, signal),
        loadConnections(client, gaggle.name, cache, signal),
      ]);
      return {
        gaggle,
        goobers,
        workflows: scope?.workflow
          ? workflows.filter((item) => item.identity.name === scope.workflow)
          : workflows,
        connections,
      };
    }),
  );
}

function operationalSnapshotCacheKey(scope: OperationalScope): string {
  return dataCacheKey(
    "operational-snapshot",
    scope.gaggle ?? "",
    scope.workflow ?? "",
  );
}

function operationalSnapshotDependencies(
  scope: OperationalScope,
): readonly DataCacheDependency[] {
  return [
    { model: "instance" },
    { model: "workflow", gaggle: scope.gaggle, workflow: scope.workflow },
    { model: "run", gaggle: scope.gaggle, workflow: scope.workflow },
  ];
}

function loadGaggles(
  client: DaemonClient,
  cache?: SessionDataCache,
  signal?: AbortSignal,
): Promise<Gaggle[]> {
  const load = (requestSignal?: AbortSignal) =>
    collectPages((cursor) =>
      client.listGaggles({ cursor, limit: PAGE_LIMIT }, { signal: requestSignal }),
    );
  if (!cache) {
    return load(signal);
  }

  return Promise.all([
    loadInventoryProjection(
      cache,
      ["gaggles"],
      "definitions",
      INVENTORY_DEPENDENCIES,
      GAGGLE_ACTIVITY_DEPENDENCIES,
      () => load(),
      (gaggles) => gaggles.map(gaggleDefinition),
    ),
    loadInventoryProjection(
      cache,
      ["gaggles"],
      "activity",
      GAGGLE_ACTIVITY_DEPENDENCIES,
      GAGGLE_ACTIVITY_DEPENDENCIES,
      () => load(),
      (gaggles) => new Map(gaggles.map((gaggle) => [gaggle.name, gaggle.activeRunCount])),
    ),
  ]).then(([definitions, activeRuns]) =>
    definitions.map((gaggle) => ({
      ...gaggle,
      activeRunCount: activeRuns.get(gaggle.name) ?? 0,
    })),
  );
}

function loadGaggleDefinitions(
  client: DaemonClient,
  cache?: SessionDataCache,
  signal?: AbortSignal,
): Promise<GaggleDefinition[]> {
  const load = (requestSignal?: AbortSignal) =>
    collectPages((cursor) =>
      client.listGaggles({ cursor, limit: PAGE_LIMIT }, { signal: requestSignal }),
    );
  if (!cache) {
    return load(signal).then((gaggles) => gaggles.map(gaggleDefinition));
  }
  return loadInventoryProjection(
    cache,
    ["gaggles"],
    "definitions",
    INVENTORY_DEPENDENCIES,
    GAGGLE_ACTIVITY_DEPENDENCIES,
    () => load(),
    (gaggles) => gaggles.map(gaggleDefinition),
  );
}

function loadGoobers(
  client: DaemonClient,
  gaggle: string,
  cache?: SessionDataCache,
  signal?: AbortSignal,
): Promise<Goober[]> {
  return loadInventoryCollection(
    cache,
    dataCacheKey("operational-inventory", "goobers", gaggle),
    (requestSignal) =>
      collectPages((cursor) =>
        client.listGoobers(
          gaggle,
          { cursor, limit: PAGE_LIMIT },
          { signal: requestSignal },
        ),
      ),
    signal,
  );
}

function loadWorkflows(
  client: DaemonClient,
  gaggle: string,
  cache?: SessionDataCache,
  signal?: AbortSignal,
): Promise<WorkflowSummary[]> {
  const load = (requestSignal?: AbortSignal) =>
    collectPages((cursor) =>
      client.listWorkflows(gaggle, { cursor, limit: PAGE_LIMIT }, { signal: requestSignal }),
    );
  if (!cache) {
    return load(signal);
  }
  const activityDependencies = workflowActivityDependencies(gaggle);
  return Promise.all([
    loadInventoryProjection(
      cache,
      ["workflows", gaggle],
      "definitions",
      INVENTORY_DEPENDENCIES,
      activityDependencies,
      () => load(),
      (workflows) => workflows.map(workflowDefinition),
    ),
    loadInventoryProjection(
      cache,
      ["workflows", gaggle],
      "activity",
      activityDependencies,
      activityDependencies,
      () => load(),
      (workflows) =>
        new Map(
          workflows.map((workflow) => [workflow.identity.name, workflow.concurrency.activeRuns]),
        ),
    ),
  ]).then(([definitions, activeRuns]) =>
    definitions.map((workflow) => ({
      ...workflow,
      concurrency: {
        ...workflow.concurrency,
        activeRuns: activeRuns.get(workflow.identity.name) ?? 0,
      },
    })),
  );
}

function loadConnections(
  client: DaemonClient,
  gaggle: string,
  cache?: SessionDataCache,
  signal?: AbortSignal,
): Promise<RepositoryConnection[]> {
  return loadInventoryCollection(
    cache,
    dataCacheKey("operational-inventory", "connections", gaggle),
    async (requestSignal) =>
      (await client.getGaggleConnections(gaggle, { signal: requestSignal })).repositories,
    signal,
  );
}

function loadWorkflowDefinitions(
  client: DaemonClient,
  gaggle: string,
  cache?: SessionDataCache,
  signal?: AbortSignal,
): Promise<WorkflowDefinitionSummary[]> {
  const load = (requestSignal?: AbortSignal) =>
    collectPages((cursor) =>
      client.listWorkflows(gaggle, { cursor, limit: PAGE_LIMIT }, { signal: requestSignal }),
    );
  if (!cache) {
    return load(signal).then((workflows) => workflows.map(workflowDefinition));
  }
  const activityDependencies = workflowActivityDependencies(gaggle);
  return loadInventoryProjection(
    cache,
    ["workflows", gaggle],
    "definitions",
    INVENTORY_DEPENDENCIES,
    activityDependencies,
    () => load(),
    (workflows) => workflows.map(workflowDefinition),
  );
}

function loadInventoryProjection<T, U>(
  cache: SessionDataCache,
  keyParts: string[],
  projection: "activity" | "definitions",
  dependencies: readonly DataCacheDependency[],
  activityDependencies: readonly DataCacheDependency[],
  load: () => Promise<T[]>,
  project: (items: T[]) => U,
): Promise<U> {
  const loadSource = () =>
    cache.getOrLoad(
      dataCacheKey("operational-inventory", "source", ...keyParts),
      [...INVENTORY_DEPENDENCIES, ...activityDependencies],
      load,
      INVENTORY_CACHE_TTL_MS,
    );
  return cache.getOrLoad(
    dataCacheKey("operational-inventory", projection, ...keyParts),
    dependencies,
    async () => project(await loadSource()),
    INVENTORY_CACHE_TTL_MS,
  );
}

function loadInventoryCollection<T>(
  cache: SessionDataCache | undefined,
  key: string,
  load: (signal?: AbortSignal) => Promise<T[]>,
  signal?: AbortSignal,
): Promise<T[]> {
  if (cache) {
    return cache.getOrLoad(
      key,
      INVENTORY_DEPENDENCIES,
      // Keep session-scoped requests alive across route unmounts so the
      // destination page can join them instead of restarting the fan-out.
      () => load(),
      INVENTORY_CACHE_TTL_MS,
    );
  }
  return load(signal);
}

function workflowActivityDependencies(gaggle: string): readonly DataCacheDependency[] {
  return [
    { model: "run", gaggle },
    { model: "workflow", gaggle },
  ];
}

function gaggleDefinition(gaggle: Gaggle): GaggleDefinition {
  return {
    name: gaggle.name,
    displayName: gaggle.displayName,
    status: gaggle.status,
    project: gaggle.project,
    backlog: gaggle.backlog,
    gooberCount: gaggle.gooberCount,
    workflowCount: gaggle.workflowCount,
    warnings: gaggle.warnings,
  };
}

function workflowDefinition(workflow: WorkflowSummary): WorkflowDefinitionSummary {
  return {
    identity: workflow.identity,
    displayName: workflow.displayName,
    purpose: workflow.purpose,
    triggers: workflow.triggers,
    readiness: workflow.readiness,
    concurrency: {
      desiredRuns: workflow.concurrency.desiredRuns,
      maxConcurrentRuns: workflow.concurrency.maxConcurrentRuns,
      admissionBlocked: workflow.concurrency.admissionBlocked,
      blockingCondition: workflow.concurrency.blockingCondition,
    },
    owners: workflow.owners,
    stageCount: workflow.stageCount,
    definition: workflow.definition,
    warnings: workflow.warnings,
  };
}

async function loadOverviewRunGroups(
  client: DaemonClient,
  signal?: AbortSignal,
  previous?: OperationalRunGroups,
): Promise<OperationalRunGroups> {
  const byPhase = (phase: RunPhase, limit: number) =>
    client.listRuns({ phase, limit }, { signal });
  // Attention candidates are filtered/ordered by last activity rather than
  // start (#1199) — the recency window is meaningless on the started_at axis,
  // since it would drop an old-started run that escalated a minute ago before
  // the portal ever saw it. This requires the read model (#1777); an instance
  // without one refuses the request, which Promise.allSettled below treats
  // like any other single-phase failure.
  const attentionSince = new Date(Date.now() - ATTENTION_RECENCY_WINDOW_MS).toISOString();
  const byRecentActivity = (phase: RunPhase, limit: number) =>
    client.listRuns({ phase, limit, since: attentionSince, orderByActivity: true }, { signal });
  // One phase timing out must not void the other four: the phases are separate
  // queries backing separate groups, and losing "active runs" because the
  // "completed" page was slow discards exactly the data an operator is looking
  // at during an incident (#1709).
  const settled = await Promise.allSettled([
    byPhase("running", ACTIVE_RUN_LIMIT),
    byRecentActivity("escalated", ATTENTION_RUN_LIMIT),
    byRecentActivity("failed", ATTENTION_RUN_LIMIT),
    byPhase("completed", RECENT_OUTCOME_LIMIT),
    byPhase("aborted", RECENT_OUTCOME_LIMIT),
  ]);
  const [, escalatedResult, failedResult] = settled;
  // Every phase failing means the run list as a whole is unreadable, which the
  // caller reports as a failed section rather than silently showing "no runs".
  //
  // The attention group is a further special case: escalated/failed are both
  // queried on the last-activity axis (#1199), which a read-model-less
  // instance refuses outright. That refusal doesn't fail every phase — running/
  // completed/aborted still succeed — so the check above would let it through,
  // and a per-phase fallback would render an attention list that is empty for
  // the same reason as a genuinely idle instance: hiding real escalations
  // behind no error at all. Treat either attention query rejecting as
  // unreadable too.
  const firstError = settled.find((result) => result.status === "rejected");
  if (
    (firstError && settled.every((result) => result.status === "rejected")) ||
    escalatedResult.status === "rejected" ||
    failedResult.status === "rejected"
  ) {
    throw (
      settledError(escalatedResult) ??
      settledError(failedResult) ??
      (firstError ? settledError(firstError) : undefined) ??
      new Error("Unable to read runs.")
    );
  }
  const previousRuns = previous
    ? [...previous.active, ...previous.attention, ...previous.recent]
    : [];
  const [running, escalated, failed, completed, aborted] = settled.map((result, index) =>
    resolvePhaseRuns(result, OVERVIEW_RUN_PHASES[index], previousRuns),
  );
  const incomplete = incompletePhases(settled, OVERVIEW_RUN_PHASES);
  return {
    active: sortRuns(running),
    attention: sortRunsByActivity([...escalated, ...failed]).slice(0, ATTENTION_RUN_LIMIT),
    recent: sortRuns([...completed, ...aborted]).slice(0, RECENT_OUTCOME_LIMIT),
    ...(incomplete ? { incomplete } : {}),
  };
}
