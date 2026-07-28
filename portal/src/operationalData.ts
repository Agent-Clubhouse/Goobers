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
const WORKFLOW_RUN_LIMIT = 5;
// Keep browser connection capacity available for unrelated daemon requests.
const WORKFLOW_OUTCOME_CONCURRENCY = 4;
const OPERATIONAL_SNAPSHOT_CACHE_KEY = dataCacheKey("operational-snapshot");
const OPERATIONAL_OVERVIEW_CACHE_KEY = dataCacheKey("operational-overview");
const OPERATIONAL_GAGGLES_CACHE_KEY = dataCacheKey("operational-inventory", "gaggles");
export const INVENTORY_CACHE_TTL_MS = 5 * 60_000;
const OPERATIONAL_DEPENDENCIES: readonly DataCacheDependency[] = [
  { model: "instance" },
  { model: "workflow" },
  { model: "run" },
];
const INVENTORY_DEPENDENCIES: readonly DataCacheDependency[] = [
  { model: "inventory" },
];

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
  scope: { gaggle?: string; workflow?: string } = {},
): OperationalSnapshotQuery {
  const { cache, freshness, isFresh, subscribe } = useLiveData();
  const [state, setState] = useState<QueryState<OperationalSnapshot>>(() => {
    const cached = cache.get<OperationalSnapshot>(OPERATIONAL_SNAPSHOT_CACHE_KEY);
    return cached ? { status: "ready", data: cached } : { status: "loading" };
  });

  const performRefresh = useCallback(
    async (_models: ReadonlySet<UpdateModel> | undefined, signal: AbortSignal) => {
      const cacheRevision = cache.beginWrite(
        OPERATIONAL_SNAPSHOT_CACHE_KEY,
        OPERATIONAL_DEPENDENCIES,
      );
      setState((current) =>
        current.status === "ready" || current.status === "stale"
          ? { status: "stale", data: current.data }
          : { status: "loading" },
      );

      try {
        const data = await loadOperationalSnapshot(client, signal, cache);
        if (signal.aborted) {
          return false;
        }
        cache.set(
          OPERATIONAL_SNAPSHOT_CACHE_KEY,
          data,
          OPERATIONAL_DEPENDENCIES,
          cacheRevision,
        );
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
    [cache, client, isFresh],
  );
  const refresh = useCoalescedOperationalRefresh(performRefresh);

  useEffect(
    () =>
      subscribe(
        ["instance", "workflow", "run"],
        (models, reason) => {
          const cached =
            reason === "initial"
              ? cache.get<OperationalSnapshot>(OPERATIONAL_SNAPSHOT_CACHE_KEY)
              : undefined;
          if (cached) {
            setState(
              isFresh() ? { status: "ready", data: cached } : { status: "stale", data: cached },
            );
            return true;
          }
          return refresh(models);
        },
        scope,
      ),
    [cache, isFresh, refresh, scope.gaggle, scope.workflow, subscribe],
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

  return {
    retry: () => {
      cache.remove(OPERATIONAL_SNAPSHOT_CACHE_KEY);
      void refresh();
    },
    state,
  };
}

export async function loadOperationalSnapshot(
  client: DaemonClient,
  signal?: AbortSignal,
  cache?: SessionDataCache,
): Promise<OperationalSnapshot> {
  const options = { signal };
  const [health, instance, inventories] = await Promise.all([
    client.getHealth(options),
    client.getInstance(options),
    loadOperationalInventory(client, cache, signal),
  ]);
  const runs = await collectWorkflowOutcomes(client, inventories, signal);

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

async function collectWorkflowOutcomes(
  client: DaemonClient,
  inventories: GaggleInventory[],
  signal?: AbortSignal,
): Promise<RunSummary[]> {
  const identities = inventories.flatMap((inventory) =>
    inventory.workflows.map((workflow) => workflow.identity),
  );
  const outcomes = new Array<RunSummary | undefined>(identities.length);
  let nextIndex = 0;

  async function collectNext(): Promise<void> {
    while (nextIndex < identities.length) {
      const index = nextIndex;
      nextIndex += 1;
      const { gaggle, name } = identities[index];
      const response = await client.listRuns(
        { gaggle, workflow: name, limit: WORKFLOW_RUN_LIMIT },
        { signal },
      );
      outcomes[index] = latestWorkflowOutcome(response.runs, gaggle, name);
    }
  }

  await Promise.all(
    Array.from(
      { length: Math.min(WORKFLOW_OUTCOME_CONCURRENCY, identities.length) },
      collectNext,
    ),
  );
  return outcomes.filter((run): run is RunSummary => run !== undefined);
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
    wantRuns ? loadOverviewRunGroups(client, signal) : Promise.resolve(previous!.groups),
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
  const gaggles = await loadGaggles(client, cache, signal);
  const workflowLists = await Promise.all(
    gaggles.map((gaggle) => loadWorkflows(client, gaggle.name, cache, signal)),
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
): Promise<GaggleInventory[]> {
  const gaggles = await loadGaggles(client, cache, signal);
  return Promise.all(
    gaggles.map(async (gaggle) => {
      const [goobers, workflows] = await Promise.all([
        loadGoobers(client, gaggle.name, cache, signal),
        loadWorkflows(client, gaggle.name, cache, signal),
      ]);
      return { gaggle, goobers, workflows };
    }),
  );
}

function loadGaggles(
  client: DaemonClient,
  cache?: SessionDataCache,
  signal?: AbortSignal,
): Promise<Gaggle[]> {
  return loadInventoryCollection(
    cache,
    OPERATIONAL_GAGGLES_CACHE_KEY,
    (requestSignal) =>
      collectPages((cursor) =>
        client.listGaggles({ cursor, limit: PAGE_LIMIT }, { signal: requestSignal }),
      ),
    signal,
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
  return loadInventoryCollection(
    cache,
    dataCacheKey("operational-inventory", "workflows", gaggle),
    (requestSignal) =>
      collectPages((cursor) =>
        client.listWorkflows(
          gaggle,
          { cursor, limit: PAGE_LIMIT },
          { signal: requestSignal },
        ),
      ),
    signal,
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

async function loadOverviewRunGroups(
  client: DaemonClient,
  signal?: AbortSignal,
): Promise<OperationalRunGroups> {
  const byPhase = (phase: RunPhase, limit: number) =>
    client.listRuns({ phase, limit }, { signal });
  // One phase timing out must not void the other four: the phases are separate
  // queries backing separate groups, and losing "active runs" because the
  // "completed" page was slow discards exactly the data an operator is looking
  // at during an incident (#1709).
  const settled = await Promise.allSettled([
    byPhase("running", ACTIVE_RUN_LIMIT),
    byPhase("escalated", ATTENTION_RUN_LIMIT),
    byPhase("failed", ATTENTION_RUN_LIMIT),
    byPhase("completed", RECENT_OUTCOME_LIMIT),
    byPhase("aborted", RECENT_OUTCOME_LIMIT),
  ]);
  // Every phase failing means the run list as a whole is unreadable, which the
  // caller reports as a failed section rather than silently showing "no runs".
  const firstError = settled.find((result) => result.status === "rejected");
  if (firstError && settled.every((result) => result.status === "rejected")) {
    throw settledError(firstError) ?? new Error("Unable to read runs.");
  }
  const [running, escalated, failed, completed, aborted] = settled.map((result) =>
    settledValue(result)?.runs ?? [],
  );
  return {
    active: sortRuns(running),
    attention: sortRuns([...escalated, ...failed]).slice(0, ATTENTION_RUN_LIMIT),
    recent: sortRuns([...completed, ...aborted]).slice(0, RECENT_OUTCOME_LIMIT),
  };
}
