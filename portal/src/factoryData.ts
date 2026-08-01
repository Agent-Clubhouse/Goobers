import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { QueryState } from "./api/queryState";
import type {
  DaemonClient,
  Health,
  Instance,
  RunSummary,
  UpdateModel,
  WorkflowDetail,
} from "./api/types";
import { dataCacheKey, type DataCacheDependency } from "./dataCache";
import {
  buildFactoryFloorModel,
  deriveRunSignal,
  laneKey,
  validateFactoryScope,
  type FactoryFloorModel,
  type FactoryRunSignal,
  type FactoryScope,
} from "./factoryModel";
import { useLiveData } from "./liveData";
import {
  useOperationalSnapshot,
  type GaggleInventory,
  type OperationalSnapshot,
} from "./operationalData";

/**
 * Live reads behind the Factory Floor.
 *
 * The inventory (gaggles, goobers, workflow summaries) and the recent terminal
 * outcomes come from the shared operational snapshot, so the floor participates
 * in the same cache, invalidation and freshness behaviour as every other
 * operational page. What the snapshot does NOT have is the live work: its runs
 * are the latest terminal outcome per workflow. The floor therefore reads the
 * active population itself, `phase=running`, server-side filtered and bounded,
 * plus exactly enough per-run detail to say truthfully whether a run is moving,
 * paused at a human gate, or blocked.
 *
 * Request budget per refresh, all cancellable:
 *   1 active-run list
 * + at most FACTORY_WORKFLOW_DETAIL_LIMIT workflow reads (for real topology)
 * + one per-run signal read for every visible active run
 *
 * The per-run read is chosen by stage kind: a gate needs its journal to see
 * `gate.paused`, while an ordinary stage only needs that stage's attempts. No
 * run is read twice and nothing fans out per stage.
 */

export const FACTORY_ACTIVE_RUN_LIMIT = 50;
export const FACTORY_RUN_DETAIL_CONCURRENCY = 6;
export const FACTORY_WORKFLOW_DETAIL_LIMIT = 12;

export interface FactoryDetail {
  activeRuns: RunSummary[];
  workflowDetails: Map<string, WorkflowDetail>;
  runSignals: Map<string, FactoryRunSignal>;
  runsTruncated: boolean;
}

export interface FactoryFloorData {
  health: Health;
  instance: Instance;
  /** Every configured gaggle, so the scope selector offers real options. */
  inventories: GaggleInventory[];
  /** The scope actually applied, after dropping anything the daemon does not know. */
  scope: FactoryScope;
  /** Scope values that were requested but are not configured. */
  droppedScope: FactoryScope;
  model: FactoryFloorModel;
}

export interface FactoryFloorQuery {
  retry: () => void;
  state: QueryState<FactoryFloorData>;
}

export function useFactoryFloor(
  client: DaemonClient,
  scope: FactoryScope = {},
): FactoryFloorQuery {
  // Unscoped on purpose: the inventory read is what the scope selector is built
  // from, and it is the same cached read the Workflows page already performs.
  // Scoping happens on the active-run read (server-side) and in the model.
  const snapshot = useOperationalSnapshot(client);
  const snapshotData = snapshotValue(snapshot.state);
  const validated = snapshotData
    ? validateFactoryScope(scope, snapshotData.inventories)
    : { scope, dropped: {} };
  const effectiveScope = validated.scope;
  const keySignature = workflowKeySignature(
    snapshotData?.inventories ?? [],
    effectiveScope,
  );
  const workflowKeys = useMemo(
    () => (keySignature === "" ? [] : keySignature.split("|")),
    [keySignature],
  );
  const detail = useFactoryDetail(client, effectiveScope, workflowKeys);
  const detailData = detailValue(detail.state);

  const previous = useRef<FactoryFloorModel | undefined>(undefined);
  const model = useMemo(() => {
    if (!snapshotData) {
      return undefined;
    }
    return buildFactoryFloorModel({
      inventories: snapshotData.inventories,
      workflowDetails: detailData?.workflowDetails,
      activeRuns: detailData?.activeRuns ?? [],
      runSignals: detailData?.runSignals,
      recentOutcomes: snapshotData.runs,
      scope: effectiveScope,
      previous: previous.current,
      runsTruncated: detailData?.runsTruncated,
    });
  }, [detailData, effectiveScope.gaggle, effectiveScope.workflow, snapshotData]);

  useEffect(() => {
    if (model) {
      previous.current = model;
    }
  }, [model]);

  const retry = useCallback(() => {
    snapshot.retry();
    detail.retry();
  }, [detail.retry, snapshot.retry]);

  const state = useMemo<QueryState<FactoryFloorData>>(() => {
    if (!snapshotData || !model || !detailData) {
      if (snapshot.state.status === "error") {
        return { status: "error", error: snapshot.state.error };
      }
      if (detail.state.status === "error") {
        return { status: "error", error: detail.state.error };
      }
      return { status: "loading" };
    }
    const data: FactoryFloorData = {
      health: snapshotData.health,
      instance: snapshotData.instance,
      inventories: snapshotData.inventories,
      scope: effectiveScope,
      droppedScope: validated.dropped,
      model,
    };
    const error =
      (snapshot.state.status === "stale" ? snapshot.state.error : undefined) ??
      (detail.state.status === "stale" ? detail.state.error : undefined) ??
      (detail.state.status === "error" ? detail.state.error : undefined);
    // The floor is stale whenever either completed read is stale. A cold detail
    // read stays in loading above, so the page cannot claim the plant is idle
    // before active work has been read.
    const stale =
      snapshot.state.status === "stale" ||
      detail.state.status === "stale" ||
      detail.state.status === "error";
    return stale ? { status: "stale", data, error } : { status: "ready", data };
  }, [
    detail.state,
    effectiveScope.gaggle,
    effectiveScope.workflow,
    model,
    snapshot.state,
    snapshotData,
    validated.dropped.gaggle,
    validated.dropped.workflow,
  ]);

  return { retry, state };
}

function snapshotValue(
  state: QueryState<OperationalSnapshot>,
): OperationalSnapshot | undefined {
  return state.status === "ready" || state.status === "stale" ? state.data : undefined;
}

function detailValue(state: QueryState<FactoryDetail>): FactoryDetail | undefined {
  return state.status === "ready" || state.status === "stale" ? state.data : undefined;
}

export function factoryWorkflowKeys(
  inventories: readonly GaggleInventory[],
  scope: FactoryScope = {},
  limit = FACTORY_WORKFLOW_DETAIL_LIMIT,
): string[] {
  const seen = new Set<string>();
  return inventories
    .flatMap((inventory) => inventory.workflows)
    .filter(
      (workflow) =>
        (!scope.gaggle || workflow.identity.gaggle === scope.gaggle) &&
        (!scope.workflow || workflow.identity.name === scope.workflow),
    )
    .map((workflow) => laneKey(workflow.identity.gaggle, workflow.identity.name))
    .filter((key) => {
      if (seen.has(key)) {
        return false;
      }
      seen.add(key);
      return true;
    })
    .slice(0, limit);
}

function workflowKeySignature(
  inventories: readonly GaggleInventory[],
  scope: FactoryScope,
): string {
  return factoryWorkflowKeys(inventories, scope, Number.MAX_SAFE_INTEGER).join("|");
}

interface FactoryDetailQuery {
  retry: () => void;
  state: QueryState<FactoryDetail>;
}

function useFactoryDetail(
  client: DaemonClient,
  scope: FactoryScope,
  workflowKeys: readonly string[],
): FactoryDetailQuery {
  const { cache, freshness, isFresh, subscribe } = useLiveData();
  const keySignature = workflowKeys.join("|");
  const cacheKey = dataCacheKey(
    "factory-detail",
    scope.gaggle ?? "",
    scope.workflow ?? "",
    keySignature,
  );
  const cached = cache.get<FactoryDetail>(cacheKey);
  const [stored, setStored] = useState<{
    cacheKey: string;
    state: QueryState<FactoryDetail>;
  }>(() => {
    return {
      cacheKey,
      state: cached ? { status: "ready", data: cached } : { status: "loading" },
    };
  });
  const state =
    stored.cacheKey === cacheKey
      ? stored.state
      : cached
        ? ({ status: "ready", data: cached } as const)
        : ({ status: "loading" } as const);
  const request = useRef<AbortController | undefined>(undefined);
  const dataByKey = useRef(new Map<string, FactoryDetail>());
  const retainedWorkflowDetails = useRef(new Map<string, WorkflowDetail>());
  if (cached && !dataByKey.current.has(cacheKey)) {
    dataByKey.current.set(cacheKey, cached);
    for (const [key, detail] of cached.workflowDetails) {
      retainedWorkflowDetails.current.set(key, detail);
    }
  }

  const refresh = useCallback((): Promise<boolean> => {
    request.current?.abort();
    const dependencies = factoryDetailDependencies(scope);
    const cacheRevision = cache.beginWrite(cacheKey, dependencies);
    const controller = new AbortController();
    request.current = controller;
    const previous = dataByKey.current.get(cacheKey);
    setStored({
      cacheKey,
      state: previous ? { status: "stale", data: previous } : { status: "loading" },
    });

    return loadFactoryDetail(
      client,
      scope,
      workflowKeys,
      controller.signal,
      retainedWorkflowDetails.current,
    ).then(
      (data) => {
        if (controller.signal.aborted) {
          return true;
        }
        if (request.current === controller) {
          request.current = undefined;
        }
        dataByKey.current.set(cacheKey, data);
        for (const [key, detail] of data.workflowDetails) {
          retainedWorkflowDetails.current.set(key, detail);
        }
        cache.set(cacheKey, data, dependencies, cacheRevision);
        setStored({
          cacheKey,
          state: isFresh() ? { status: "ready", data } : { status: "stale", data },
        });
        return true;
      },
      (error: unknown) => {
        if (controller.signal.aborted) {
          return true;
        }
        if (request.current === controller) {
          request.current = undefined;
        }
        const queryError =
          error instanceof Error ? error : new Error("Unable to read active runs.");
        const previous = dataByKey.current.get(cacheKey);
        setStored({
          cacheKey,
          state: previous
            ? { status: "stale", data: previous, error: queryError }
            : { status: "error", error: queryError },
        });
        return false;
      },
    );
  }, [cache, cacheKey, client, isFresh, keySignature, scope.gaggle, scope.workflow]);

  useEffect(() => {
    const unsubscribe = subscribe(
      ["run", "workflow"],
      (_models: ReadonlySet<UpdateModel>, reason) => {
        const cached =
          reason === "initial" ? cache.get<FactoryDetail>(cacheKey) : undefined;
        if (cached) {
          dataByKey.current.set(cacheKey, cached);
          for (const [key, detail] of cached.workflowDetails) {
            retainedWorkflowDetails.current.set(key, detail);
          }
          setStored({
            cacheKey,
            state: isFresh()
              ? { status: "ready", data: cached }
              : { status: "stale", data: cached },
          });
          return true;
        }
        return refresh();
      },
      { gaggle: scope.gaggle, workflow: scope.workflow },
    );
    return () => {
      unsubscribe();
      request.current?.abort();
      request.current = undefined;
    };
  }, [cache, cacheKey, isFresh, refresh, subscribe]);

  useEffect(() => {
    setStored((current) => {
      if (current.cacheKey !== cacheKey) {
        return current;
      }
      if (freshness !== "connected" && current.state.status === "ready") {
        return {
          cacheKey,
          state: { status: "stale", data: current.state.data },
        };
      }
      if (
        freshness === "connected" &&
        current.state.status === "stale" &&
        !current.state.error
      ) {
        return {
          cacheKey,
          state: { status: "ready", data: current.state.data },
        };
      }
      return current;
    });
  }, [cacheKey, freshness]);

  const retry = useCallback(() => {
    cache.remove(cacheKey);
    void refresh();
  }, [cache, cacheKey, refresh]);

  return { retry, state };
}

function factoryDetailDependencies(scope: FactoryScope): readonly DataCacheDependency[] {
  return [
    { model: "run", gaggle: scope.gaggle, workflow: scope.workflow },
    { model: "workflow", gaggle: scope.gaggle, workflow: scope.workflow },
  ];
}

export async function loadFactoryDetail(
  client: DaemonClient,
  scope: FactoryScope,
  workflowKeys: readonly string[],
  signal?: AbortSignal,
  retainedWorkflowDetails: ReadonlyMap<string, WorkflowDetail> = new Map(),
): Promise<FactoryDetail> {
  const options = { signal };
  const runList = await client.listRuns(
    {
      phase: "running",
      gaggle: scope.gaggle,
      workflow: scope.workflow,
      limit: FACTORY_ACTIVE_RUN_LIMIT,
    },
    options,
  );
  const running = runList.runs
    .filter((run) => run.phase === "running")
    .sort(
      (left, right) =>
        Date.parse(left.startedAt) - Date.parse(right.startedAt) ||
        left.id.localeCompare(right.id),
    );
  const activeRuns = running.slice(0, FACTORY_ACTIVE_RUN_LIMIT);
  const runsTruncated =
    runList.nextCursor !== undefined || running.length > FACTORY_ACTIVE_RUN_LIMIT;

  const keys = boundedWorkflowKeys(workflowKeys, activeRuns);
  const detailResults = await Promise.allSettled(
    keys.map((key) => {
      const [gaggle, name] = splitLaneKey(key);
      return client.getWorkflow(gaggle, name, options);
    }),
  );
  throwIfAborted(signal);
  const workflowDetails = new Map<string, WorkflowDetail>(retainedWorkflowDetails);
  detailResults.forEach((result, index) => {
    // A failed refresh keeps the last confirmed topology. A workflow that has
    // never been read still degrades to stages observed from live runs.
    if (result.status === "fulfilled") {
      workflowDetails.set(keys[index], result.value);
    }
  });

  const signalResults = await settleWithConcurrency(
    activeRuns,
    FACTORY_RUN_DETAIL_CONCURRENCY,
    signal,
    (run) => loadRunSignal(client, run, workflowDetails, options),
  );
  const runSignals = new Map<string, FactoryRunSignal>();
  signalResults.forEach((result, index) => {
    const run = activeRuns[index];
    runSignals.set(
      run.id,
      result.status === "fulfilled"
        ? result.value
        : { state: "unknown", confirmed: false },
    );
  });

  return { activeRuns, workflowDetails, runSignals, runsTruncated };
}

async function loadRunSignal(
  client: DaemonClient,
  run: RunSummary,
  workflowDetails: ReadonlyMap<string, WorkflowDetail>,
  options: { signal?: AbortSignal },
): Promise<FactoryRunSignal> {
  const stage = run.currentStage;
  if (!stage) {
    return deriveRunSignal({ run });
  }
  const detail = workflowDetails.get(laneKey(run.gaggle, run.workflow));
  const kind = detail?.graph.nodes.find((node) => node.id === stage)?.kind;
  if (kind === "gate" || kind === undefined) {
    // The journal records both gate pauses and stage completion statuses. Use
    // it whenever topology is unread so an unknown stage kind is not guessed.
    const events = await client.listRunEvents(run.id, options);
    return deriveRunSignal({ run, events: events.events });
  }
  const attempts = await client.listStageAttempts(run.id, stage, options);
  return deriveRunSignal({ run, attempts: attempts.attempts });
}

function boundedWorkflowKeys(
  workflowKeys: readonly string[],
  activeRuns: readonly RunSummary[],
): string[] {
  const configured = new Set(workflowKeys);
  const active = new Set(activeRuns.map((run) => laneKey(run.gaggle, run.workflow)));
  const activeConfigured = workflowKeys
    .filter((key) => active.has(key))
    .sort((left, right) => left.localeCompare(right));
  const activeOutsideInventory = [...active]
    .filter((key) => !configured.has(key))
    .sort((left, right) => left.localeCompare(right));
  const remainingConfigured = workflowKeys.filter((key) => !active.has(key));
  return [...activeConfigured, ...activeOutsideInventory, ...remainingConfigured].slice(
    0,
    FACTORY_WORKFLOW_DETAIL_LIMIT,
  );
}

async function settleWithConcurrency<T, Result>(
  items: readonly T[],
  concurrency: number,
  signal: AbortSignal | undefined,
  task: (item: T) => Promise<Result>,
): Promise<PromiseSettledResult<Result>[]> {
  const results: Array<PromiseSettledResult<Result> | undefined> = new Array(
    items.length,
  );
  let cursor = 0;
  const workerCount = Math.min(
    items.length,
    Math.max(1, Math.floor(concurrency)),
  );
  const workers = Array.from({ length: workerCount }, async () => {
    while (true) {
      throwIfAborted(signal);
      const index = cursor;
      cursor += 1;
      if (index >= items.length) {
        return;
      }
      try {
        results[index] = { status: "fulfilled", value: await task(items[index]) };
      } catch (error: unknown) {
        if (signal?.aborted) {
          throw error;
        }
        results[index] = { status: "rejected", reason: error };
      }
    }
  });
  await Promise.all(workers);
  return results as PromiseSettledResult<Result>[];
}

function throwIfAborted(signal: AbortSignal | undefined): void {
  if (signal?.aborted) {
    throw new Error("Factory detail read cancelled.");
  }
}

function splitLaneKey(key: string): [string, string] {
  const separator = key.indexOf("/");
  return separator < 0
    ? [key, ""]
    : [key.slice(0, separator), key.slice(separator + 1)];
}
