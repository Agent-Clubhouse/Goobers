import { useCallback, useEffect, useRef, useState } from "react";
import { MalformedResponseError } from "./api/errors";
import type { QueryState } from "./api/queryState";
import type { DaemonClient, RunSummary, WorkflowDetail } from "./api/types";
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
  const [state, setState] = useState<QueryState<WorkflowDetailSnapshot>>({
    status: "loading",
  });
  const request = useRef<AbortController | undefined>(undefined);
  const { subscribe } = useLiveData();

  const refresh = useCallback((): Promise<boolean> => {
    request.current?.abort();
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
        setState({ status: "ready", data });
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
  }, [client, gaggle, workflowName]);

  useEffect(() => {
    setState({ status: "loading" });
    const unsubscribe = subscribe(["workflow", "run"], refresh);
    return () => {
      unsubscribe();
      request.current?.abort();
      request.current = undefined;
    };
  }, [refresh, subscribe]);

  const retry = useCallback(() => {
    setState({ status: "loading" });
    void refresh();
  }, [refresh]);
  return { retry, state };
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
