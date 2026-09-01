import { useEffect, useRef, useState } from "react";
import type { KeyboardEvent } from "react";
import type {
  DaemonClient,
  RunSummary,
  WorkflowDetail,
  WorkflowSummary,
} from "../api/types";
import { latestWorkflowOutcome } from "../operationalData";
import { routeHash } from "../routing";
import { validateWorkflowDetail } from "../workflowDetailData";
import { ScopePivot } from "./ScopePivot";
import { GraphFrame } from "../ui/GraphFrame";
import { StatusBadge } from "../ui/StatusBadge";
import { formatTriggers } from "../pages/WorkflowsPage";
import { WorkflowTopologyGraph } from "./WorkflowTopologyGraph";

type WorkflowDetailState =
  | { status: "loading" }
  | { status: "error"; error: Error }
  | { status: "ready"; detail: WorkflowDetail };

export function GaggleWorkflowExplorer({
  client,
  gaggleDisplayName,
  runs,
  workflows,
}: {
  client: DaemonClient;
  gaggleDisplayName: string;
  runs: RunSummary[];
  workflows: WorkflowSummary[];
}) {
  const [selectedWorkflowName, setSelectedWorkflowName] = useState(
    workflows[0]?.identity.name ?? "",
  );
  const selectedWorkflow =
    workflows.find(({ identity }) => identity.name === selectedWorkflowName) ??
    workflows[0];

  useEffect(() => {
    if (
      selectedWorkflowName === "" ||
      workflows.some(({ identity }) => identity.name === selectedWorkflowName)
    ) {
      return;
    }
    setSelectedWorkflowName(workflows[0]?.identity.name ?? "");
  }, [selectedWorkflowName, workflows]);

  if (!selectedWorkflow) {
    return (
      <GraphFrame
        className="gaggle-topology-panel"
        eyebrow="Definitions"
        title="Workflow topology"
      >
        <p className="inline-empty">No workflows are provisioned for this gaggle.</p>
      </GraphFrame>
    );
  }

  return (
    <GraphFrame
      action={
        <span className="graph-legend">
          {workflows.length} {workflows.length === 1 ? "workflow" : "workflows"}
        </span>
      }
      className="gaggle-topology-panel"
      eyebrow="Definitions"
      title="Workflow topology"
    >
      <div className="gaggle-workflow-explorer">
        <WorkflowPicker
          gaggleDisplayName={gaggleDisplayName}
          onSelect={setSelectedWorkflowName}
          runs={runs}
          selectedWorkflowName={selectedWorkflow.identity.name}
          workflows={workflows}
        />
        <SelectedWorkflow
          client={client}
          gaggleDisplayName={gaggleDisplayName}
          summary={selectedWorkflow}
        />
      </div>
    </GraphFrame>
  );
}

export function WorkflowPicker({
  gaggleDisplayName,
  onSelect,
  runs,
  selectedWorkflowName,
  workflows,
}: {
  gaggleDisplayName: string;
  onSelect: (workflowName: string) => void;
  runs: RunSummary[];
  selectedWorkflowName: string;
  workflows: WorkflowSummary[];
}) {
  const tabRefs = useRef<Array<HTMLButtonElement | null>>([]);
  const selectAndFocus = (index: number) => {
    const workflow = workflows[index];
    if (!workflow) {
      return;
    }
    onSelect(workflow.identity.name);
    tabRefs.current[index]?.focus();
  };
  const onTabKeyDown = (event: KeyboardEvent<HTMLButtonElement>, index: number) => {
    let nextIndex: number | undefined;
    if (event.key === "ArrowRight" || event.key === "ArrowDown") {
      nextIndex = (index + 1) % workflows.length;
    } else if (event.key === "ArrowLeft" || event.key === "ArrowUp") {
      nextIndex = (index - 1 + workflows.length) % workflows.length;
    } else if (event.key === "Home") {
      nextIndex = 0;
    } else if (event.key === "End") {
      nextIndex = workflows.length - 1;
    }
    if (nextIndex === undefined) {
      return;
    }
    event.preventDefault();
    selectAndFocus(nextIndex);
  };

  return (
    <div className="gaggle-workflow-picker">
      <div className="gaggle-workflow-picker-heading">
        <h3>Independent workflows</h3>
        <p>Definitions share this gaggle's resources, but do not imply an execution order.</p>
      </div>
      <div
        aria-label={`${gaggleDisplayName} workflows`}
        className="gaggle-workflow-tabs"
        role="tablist"
      >
        {workflows.map((workflow, index) => {
          const selected = workflow.identity.name === selectedWorkflowName;
          const latestOutcome = latestWorkflowOutcome(
            runs,
            workflow.identity.gaggle,
            workflow.identity.name,
          );
          return (
            <button
              aria-controls="gaggle-selected-workflow"
              aria-selected={selected}
              className={`gaggle-workflow-choice${selected ? " is-selected" : ""}`}
              key={workflow.identity.name}
              onClick={() => onSelect(workflow.identity.name)}
              onKeyDown={(event) => onTabKeyDown(event, index)}
              ref={(element) => {
                tabRefs.current[index] = element;
              }}
              role="tab"
              tabIndex={selected ? 0 : -1}
              type="button"
            >
              <strong>{workflow.displayName}</strong>
              <small>
                {workflow.stageCount} {workflow.stageCount === 1 ? "stage" : "stages"} ·{" "}
                {formatTriggers(workflow)}
              </small>
              {latestOutcome ? (
                <StatusBadge status={latestOutcome.phase} />
              ) : (
                <span className="gaggle-workflow-no-runs">No runs</span>
              )}
            </button>
          );
        })}
      </div>
    </div>
  );
}

function SelectedWorkflow({
  client,
  gaggleDisplayName,
  summary,
}: {
  client: DaemonClient;
  gaggleDisplayName: string;
  summary: WorkflowSummary;
}) {
  const [retryKey, setRetryKey] = useState(0);
  const state = useWorkflowDefinition(client, summary, retryKey);

  return (
    <section
      aria-label={`${summary.displayName} workflow topology`}
      className="gaggle-selected-workflow"
      id="gaggle-selected-workflow"
      role="tabpanel"
    >
      <header>
        <div>
          <p className="section-kicker">Workflow preview</p>
          <h3>{summary.displayName}</h3>
          <p>{summary.purpose || "No purpose is documented for this workflow."}</p>
          <small>
            {summary.stageCount} {summary.stageCount === 1 ? "stage" : "stages"} ·{" "}
            {formatTriggers(summary)}
          </small>
        </div>
        <div className="gaggle-workflow-preview-actions">
          <a
            href={routeHash({
              page: "workflow",
              gaggle: summary.identity.gaggle,
              id: summary.identity.name,
            })}
          >
            Open full workflow
          </a>
          <ScopePivot
            label={`${gaggleDisplayName} / ${summary.displayName}`}
            scope={{
              gaggle: summary.identity.gaggle,
              workflow: summary.identity.name,
            }}
          />
        </div>
      </header>

      {state.status === "loading" && (
        <p className="inline-empty" role="status">
          Loading workflow graph…
        </p>
      )}
      {state.status === "error" && (
        <div className="gaggle-workflow-error" role="alert">
          <div>
            <strong>Workflow graph unavailable</strong>
            <p>{state.error.message}</p>
          </div>
          <button onClick={() => setRetryKey((current) => current + 1)} type="button">
            Retry
          </button>
        </div>
      )}
      {state.status === "ready" && (
        <div className="gaggle-workflow-preview">
          <WorkflowTopologyGraph graph={state.detail.graph} preview />
        </div>
      )}
    </section>
  );
}

function useWorkflowDefinition(
  client: DaemonClient,
  summary: WorkflowSummary,
  retryKey: number,
): WorkflowDetailState {
  const [state, setState] = useState<WorkflowDetailState>({ status: "loading" });

  useEffect(() => {
    const controller = new AbortController();
    setState({ status: "loading" });
    void client
      .getWorkflow(summary.identity.gaggle, summary.identity.name, {
        signal: controller.signal,
      })
      .then(
        (detail) => {
          if (controller.signal.aborted) {
            return;
          }
          try {
            validateWorkflowDetail(
              detail,
              summary.identity.gaggle,
              summary.identity.name,
            );
          } catch (error: unknown) {
            setState({
              status: "error",
              error:
                error instanceof Error
                  ? error
                  : new Error("The workflow definition could not be validated."),
            });
            return;
          }
          setState({ status: "ready", detail });
        },
        (error: unknown) => {
          if (controller.signal.aborted) {
            return;
          }
          setState({
            status: "error",
            error:
              error instanceof Error
                ? error
                : new Error("The workflow definition could not be loaded."),
          });
        },
      );
    return () => controller.abort();
  }, [
    client,
    retryKey,
    summary.definition.digest,
    summary.identity.gaggle,
    summary.identity.name,
  ]);

  return state;
}
