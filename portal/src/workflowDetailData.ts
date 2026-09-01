import { MalformedResponseError } from "./api/errors";
import type { QueryState } from "./api/queryState";
import type { DaemonClient, RunSummary, UpdateModel, WorkflowDetail } from "./api/types";
import { dataCacheKey, type DataCacheDependency } from "./dataCache";
import { useLiveQuery } from "./liveQuery";

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
  return useLiveQuery<WorkflowDetailSnapshot>({
    cacheKey: dataCacheKey("workflow-detail", gaggle, workflowName),
    dependencies: workflowDetailDependencies(gaggle, workflowName),
    models: WORKFLOW_DETAIL_MODELS,
    scope: { gaggle, workflow: workflowName },
    load: (signal) => loadWorkflowDetail(client, gaggle, workflowName, signal),
    errorMessage: "Unable to read workflow detail.",
  });
}

const WORKFLOW_DETAIL_MODELS: readonly UpdateModel[] = ["workflow", "run"];

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
