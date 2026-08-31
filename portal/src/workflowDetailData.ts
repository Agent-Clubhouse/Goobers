import { useCallback, useEffect, useRef, useState } from "react";
import { MalformedResponseError } from "./api/errors";
import type { QueryState } from "./api/queryState";
import type { DaemonClient, RunSummary, WorkflowDetail } from "./api/types";
import { dataCacheKey, type DataCacheDependency } from "./dataCache";
import { useLiveData } from "./liveData";

const RECENT_RUN_LIMIT = 20;

export interface WorkflowDetailSnapshot {
  workflow: WorkflowDetail;
  runs: RunSummary[];
}

export interface WorkflowDetailQuery {
  retry: () => void;
  state: QueryState<WorkflowDetailSnapshot>;
}

export function useWorkflowDetail(
  client: DaemonClient,
  gaggle: string,
  workflowName: string,
): WorkflowDetailQuery {
  const { cache, freshness, isFresh, subscribe } = useLiveData();
  const cacheKey = dataCacheKey("workflow-detail", gaggle, workflowName);
  const [state, setState] = useState<QueryState<WorkflowDetailSnapshot>>(() => {
    const cached = cache.get<WorkflowDetailSnapshot>(cacheKey);
    return cached ? { status: "ready", data: cached } : { status: "loading" };
  });
  const request = useRef<AbortController | undefined>(undefined);

  const refresh = useCallback((): Promise<boolean> => {
    request.current?.abort();
    const dependencies = workflowDetailDependencies(gaggle, workflowName);
    const cacheRevision = cache.beginWrite(cacheKey, dependencies);
    const controller = new AbortController();
    request.current = controller;
    setState((current) =>
      current.status === "ready" || current.status === "stale"
        ? { status: "stale", data: current.data }
        : { status: "loading" },
    );

    return loadWorkflowDetail(client, gaggle, workflowName, controller.signal).then(
      (data) => {
        if (controller.signal.aborted) {
          return true;
        }
        if (request.current === controller) {
          request.current = undefined;
        }
        cache.set(cacheKey, data, dependencies, cacheRevision);
        // The stream can drop while this request is in flight; the freshness
        // effect below only fires on a freshness change, so publishing an
        // unconditional "ready" here would leave the page claiming live data
        // until the next transition (#3657).
        setState(isFresh() ? { status: "ready", data } : { status: "stale", data });
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
          error instanceof Error ? error : new Error("Unable to read workflow detail.");
        setState((current) =>
          current.status === "stale"
            ? { status: "stale", data: current.data, error: queryError }
            : { status: "error", error: queryError },
        );
        return false;
      },
    );
  }, [cache, cacheKey, client, gaggle, isFresh, workflowName]);

  useEffect(() => {
    const cached = cache.get<WorkflowDetailSnapshot>(cacheKey);
    setState(cached ? { status: "ready", data: cached } : { status: "loading" });
    const unsubscribe = subscribe(
      ["workflow", "run"],
      (_models, reason) => {
        const current =
          reason === "initial" ? cache.get<WorkflowDetailSnapshot>(cacheKey) : undefined;
        if (current) {
          setState(
            isFresh() ? { status: "ready", data: current } : { status: "stale", data: current },
          );
          return true;
        }
        return refresh();
      },
      { gaggle, workflow: workflowName },
    );
    return () => {
      unsubscribe();
      request.current?.abort();
      request.current = undefined;
    };
  }, [cache, cacheKey, isFresh, refresh, subscribe]);

  // Freshness downgrade (#1714).
  //
  // Every other query hook has this; these two did not, so a detail page kept
  // reporting "ready" after the live stream dropped — the row looked current
  // while the stream behind it was gone. The status is what the page renders
  // its freshness indicator from, so without this the indicator lies.
  //
  // It moves ready -> stale on disconnect and back on reconnect, and never
  // clears an error: a stale-with-error state must stay visibly errored rather
  // than silently becoming "ready" because the socket came back.
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

  const retry = useCallback(() => {
    cache.remove(cacheKey);
    setState({ status: "loading" });
    void refresh();
  }, [cache, cacheKey, refresh]);
  return { retry, state };
}

function workflowDetailDependencies(
  gaggle: string,
  workflow: string,
): readonly DataCacheDependency[] {
  return [
    { model: "workflow", gaggle, workflow },
    { model: "run", gaggle, workflow },
  ];
}

export async function loadWorkflowDetail(
  client: DaemonClient,
  gaggle: string,
  workflowName: string,
  signal?: AbortSignal,
): Promise<WorkflowDetailSnapshot> {
  const options = { signal };
  const [workflow, runList] = await Promise.all([
    client.getWorkflow(gaggle, workflowName, options),
    client.listRuns({ gaggle, workflow: workflowName, limit: RECENT_RUN_LIMIT }, options),
  ]);

  validateWorkflowDetail(workflow, gaggle, workflowName);

  const runs = runList.runs
    .filter((run) => run.gaggle === gaggle && run.workflow === workflowName)
    .sort(
      (left, right) =>
        Date.parse(right.finishedAt ?? right.startedAt) -
          Date.parse(left.finishedAt ?? left.startedAt) ||
        right.id.localeCompare(left.id),
    )
    .slice(0, RECENT_RUN_LIMIT);
  return { workflow, runs };
}

export function validateWorkflowDetail(
  workflow: WorkflowDetail,
  gaggle: string,
  workflowName: string,
): void {
  if (
    workflow.identity.gaggle !== gaggle ||
    workflow.identity.name !== workflowName ||
    workflow.graph.name !== workflowName
  ) {
    throw new MalformedResponseError("The daemon returned mismatched workflow detail.");
  }
  if (
    workflow.definition.version !== workflow.graph.version ||
    workflow.definition.digest !== workflow.graph.digest
  ) {
    throw new MalformedResponseError("The daemon returned inconsistent workflow definition metadata.");
  }
  const stagesByName = new Map(workflow.stages.map((stage) => [stage.name, stage]));
  if (
    workflow.stageCount !== workflow.graph.nodes.length ||
    workflow.stages.length !== workflow.graph.nodes.length ||
    !workflow.graph.nodes.some((node) => node.id === workflow.graph.start) ||
    workflow.graph.nodes.some((node) => stagesByName.get(node.id)?.kind !== node.kind)
  ) {
    throw new MalformedResponseError("The daemon returned inconsistent workflow stages.");
  }
}
