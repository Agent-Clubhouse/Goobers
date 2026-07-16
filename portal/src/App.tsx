import { useEffect, useMemo, useState } from "react";
import {
  instanceWarnings,
  runStatusLabel,
  runs,
  workflowForRun,
  workflows,
  type NodeState,
  type Run,
  type RunEvent,
  type StageAttempt,
  type Workflow,
  type WorkflowStage,
} from "./prototypeData";

type Theme = "light" | "dark";
type IconName =
  | "alert"
  | "arrow"
  | "artifact"
  | "check"
  | "chevron"
  | "clock"
  | "close"
  | "code"
  | "gate"
  | "moon"
  | "overview"
  | "pause"
  | "play"
  | "run"
  | "sun"
  | "workflow";

type Route =
  | { page: "overview" }
  | { page: "workflows" }
  | { page: "runs" }
  | { page: "workflow"; id: string }
  | { page: "run"; id: string };

function Icon({ name, size = 18 }: { name: IconName; size?: number }) {
  const paths: Record<IconName, React.ReactNode> = {
    alert: (
      <>
        <path d="M12 3 2.8 19a1.4 1.4 0 0 0 1.2 2h16a1.4 1.4 0 0 0 1.2-2L12 3Z" />
        <path d="M12 9v4" />
        <path d="M12 17h.01" />
      </>
    ),
    arrow: (
      <>
        <path d="M5 12h14" />
        <path d="m13 6 6 6-6 6" />
      </>
    ),
    artifact: (
      <>
        <path d="M7 3h7l4 4v14H7z" />
        <path d="M14 3v5h5" />
        <path d="M10 13h5" />
        <path d="M10 17h5" />
      </>
    ),
    check: <path d="m5 12 4 4L19 6" />,
    chevron: <path d="m9 18 6-6-6-6" />,
    clock: (
      <>
        <circle cx="12" cy="12" r="9" />
        <path d="M12 7v5l3 2" />
      </>
    ),
    close: (
      <>
        <path d="m6 6 12 12" />
        <path d="m18 6-12 12" />
      </>
    ),
    code: (
      <>
        <path d="m8 9-3 3 3 3" />
        <path d="m16 9 3 3-3 3" />
        <path d="m14 5-4 14" />
      </>
    ),
    gate: (
      <>
        <path d="M5 4h14v16H5z" />
        <path d="M9 4v16" />
        <path d="m13 8 3 4-3 4" />
      </>
    ),
    moon: <path d="M20 15.4A8.5 8.5 0 0 1 8.6 4 8.5 8.5 0 1 0 20 15.4Z" />,
    overview: (
      <>
        <path d="M4 4h6v6H4z" />
        <path d="M14 4h6v6h-6z" />
        <path d="M4 14h6v6H4z" />
        <path d="M14 14h6v6h-6z" />
      </>
    ),
    pause: (
      <>
        <path d="M8 5v14" />
        <path d="M16 5v14" />
      </>
    ),
    play: <path d="m8 5 11 7-11 7Z" />,
    run: (
      <>
        <circle cx="12" cy="12" r="9" />
        <path d="m10 8 6 4-6 4Z" />
      </>
    ),
    sun: (
      <>
        <circle cx="12" cy="12" r="4" />
        <path d="M12 2v2" />
        <path d="M12 20v2" />
        <path d="m4.9 4.9 1.4 1.4" />
        <path d="m17.7 17.7 1.4 1.4" />
        <path d="M2 12h2" />
        <path d="M20 12h2" />
        <path d="m4.9 19.1 1.4-1.4" />
        <path d="m17.7 6.3 1.4-1.4" />
      </>
    ),
    workflow: (
      <>
        <circle cx="5" cy="6" r="2" />
        <circle cx="19" cy="6" r="2" />
        <circle cx="12" cy="18" r="2" />
        <path d="M7 6h10" />
        <path d="m6.5 8 4.2 8" />
        <path d="m17.5 8-4.2 8" />
      </>
    ),
  };

  return (
    <svg aria-hidden="true" className="icon" fill="none" height={size} viewBox="0 0 24 24" width={size}>
      <g stroke="currentColor" strokeLinecap="round" strokeLinejoin="round" strokeWidth="1.8">
        {paths[name]}
      </g>
    </svg>
  );
}

function parseRoute(): Route {
  const path = window.location.hash.replace(/^#\/?/, "");
  const [area, id] = path.split("/");
  if (area === "workflow" && id) {
    return { page: "workflow", id };
  }
  if (area === "run" && id) {
    return { page: "run", id };
  }
  if (area === "workflows") {
    return { page: "workflows" };
  }
  if (area === "runs") {
    return { page: "runs" };
  }
  return { page: "overview" };
}

function routeHash(route: Route): string {
  if (route.page === "workflow" || route.page === "run") {
    return `#/${route.page}/${route.id}`;
  }
  return `#/${route.page}`;
}

function storedTheme(): Theme {
  try {
    return window.localStorage?.getItem("goobers-theme") === "dark" ? "dark" : "light";
  } catch {
    return "light";
  }
}

function persistTheme(theme: Theme) {
  try {
    window.localStorage?.setItem("goobers-theme", theme);
  } catch {
    // Storage can be unavailable in private or constrained browser contexts.
  }
}

function activeArea(route: Route): "overview" | "workflows" | "runs" {
  if (route.page === "workflow") {
    return "workflows";
  }
  if (route.page === "run") {
    return "runs";
  }
  return route.page;
}

function StatusBadge({ status }: { status: Run["status"] }) {
  return (
    <span className={`status-badge status-${status}`}>
      <span className="status-dot" />
      {runStatusLabel(status)}
    </span>
  );
}

function TopologyGraph({
  workflow,
  states,
  activeEdges,
  selectedStageId,
  onSelectStage,
}: {
  workflow: Workflow;
  states?: Record<string, NodeState>;
  activeEdges?: ReadonlySet<string>;
  selectedStageId?: string;
  onSelectStage: (stageId: string) => void;
}) {
  const stageById = useMemo(
    () => new Map(workflow.stages.map((stage) => [stage.id, stage])),
    [workflow.stages],
  );

  return (
    <div className="graph-canvas" aria-label={`${workflow.name} execution graph`}>
      <svg aria-hidden="true" className="graph-edges" preserveAspectRatio="none" viewBox="0 0 100 100">
        {workflow.edges.map((edge) => {
          const from = stageById.get(edge.from);
          const to = stageById.get(edge.to);
          if (!from || !to) {
            return null;
          }
          const edgeKey = `${edge.from}->${edge.to}`;
          const active = activeEdges?.has(edgeKey) ?? false;
          const path = edge.repass
            ? `M ${from.x} ${from.y + 9} C ${from.x} 88, ${to.x} 88, ${to.x} ${to.y + 9}`
            : `M ${from.x + 5} ${from.y} L ${to.x - 5} ${to.y}`;
          return (
            <g key={`${edge.from}-${edge.to}-${edge.label ?? "next"}`}>
              <path
                className={active ? "graph-edge graph-edge-active" : "graph-edge"}
                d={path}
                data-edge={edgeKey}
              />
              {edge.label && (
                <text className="graph-edge-label" x={(from.x + to.x) / 2} y={edge.repass ? 87 : from.y - 5}>
                  {edge.label}
                </text>
              )}
            </g>
          );
        })}
      </svg>
      {workflow.stages.map((stage) => {
        const state = states?.[stage.id] ?? "pending";
        return (
          <button
            aria-label={`${stage.name}, ${state}`}
            className={`graph-node node-${stage.kind} node-${state} ${
              selectedStageId === stage.id ? "graph-node-selected" : ""
            }`}
            key={stage.id}
            onClick={() => onSelectStage(stage.id)}
            style={{ left: `${stage.x}%`, top: `${stage.y}%` }}
            type="button"
          >
            <span className="graph-node-kind">{stage.kind === "gate" ? "gate" : stage.kind}</span>
            <strong>{stage.name}</strong>
            {state !== "pending" && <span className="graph-node-state">{state}</span>}
          </button>
        );
      })}
      <ul className="sr-only">
        {workflow.stages.map((stage) => {
          const outgoing = workflow.edges
            .filter((edge) => edge.from === stage.id)
            .map((edge) => `${edge.label ?? "next"} to ${stageById.get(edge.to)?.name ?? edge.to}`);
          return (
            <li key={`topology-${stage.id}`}>
              {stage.name}, {stage.kind}. {outgoing.length > 0 ? `Outgoing: ${outgoing.join("; ")}.` : "Terminal stage."}
            </li>
          );
        })}
      </ul>
    </div>
  );
}

function DataRow({
  children,
  onClick,
  label,
}: {
  children: React.ReactNode;
  onClick: () => void;
  label: string;
}) {
  return (
    <button aria-label={label} className="data-row" onClick={onClick} type="button">
      {children}
      <span className="row-arrow">
        <Icon name="chevron" size={16} />
      </span>
    </button>
  );
}

function OverviewPage({ navigate }: { navigate: (route: Route) => void }) {
  const [warningVisible, setWarningVisible] = useState(true);
  const activeRuns = runs.filter((run) => run.status === "running");
  const recentRuns = runs.filter((run) => run.status !== "running");
  const attentionRun = runs.find((run) => run.status === "escalated");

  return (
    <>
      <header className="page-heading">
        <p className="page-kicker">Local instance</p>
        <h1>One run needs attention.</h1>
        <p>Everything else is moving normally across the goobers gaggle.</p>
      </header>

      <section aria-label="Instance status" className="instance-strip">
        <div>
          <span className="live-mark" />
          <strong>Daemon connected</strong>
          <span>updated just now</span>
        </div>
        <dl>
          <div>
            <dt>Workflows</dt>
            <dd>{workflows.length}</dd>
          </div>
          <div>
            <dt>Active runs</dt>
            <dd>{activeRuns.length}</dd>
          </div>
          <div>
            <dt>Gaggles</dt>
            <dd>1</dd>
          </div>
        </dl>
      </section>

      {attentionRun && (
        <section className="content-section">
          <div className="section-heading">
            <div>
              <p className="section-kicker section-kicker-danger">Attention</p>
              <h2>Needs a decision</h2>
            </div>
            <span className="section-count">1 run</span>
          </div>
          <button
            className="attention-row"
            onClick={() => navigate({ page: "run", id: attentionRun.id })}
            type="button"
          >
            <span className="attention-icon">
              <Icon name="alert" />
            </span>
            <span className="attention-copy">
              <strong>{attentionRun.title}</strong>
              <span>{attentionRun.escalation?.title}</span>
            </span>
            <span className="attention-meta">
              <span>{attentionRun.issue}</span>
              <span>{attentionRun.repasses} of 3 repasses</span>
            </span>
            <Icon name="arrow" />
          </button>
        </section>
      )}

      <section className="content-section">
        <div className="section-heading">
          <div>
            <p className="section-kicker">Live</p>
            <h2>Active runs</h2>
          </div>
          <button className="text-button" onClick={() => navigate({ page: "runs" })} type="button">
            View all runs <Icon name="arrow" size={15} />
          </button>
        </div>
        <div className="data-table">
          <div className="data-header run-grid">
            <span>Run</span>
            <span>Workflow</span>
            <span>Current stage</span>
            <span>Elapsed</span>
            <span />
          </div>
          {activeRuns.map((run) => (
            <DataRow key={run.id} label={`Open run ${run.title}`} onClick={() => navigate({ page: "run", id: run.id })}>
              <span className="row-primary">
                <span className="row-title">{run.title}</span>
                <span className="row-subtitle">
                  {run.issue} · {run.shortId}
                </span>
              </span>
              <span>{workflowForRun(run).name}</span>
              <span className="stage-progress">
                <span className="stage-progress-mark" />
                {run.currentStage}
              </span>
              <span className="mono">{run.duration}</span>
            </DataRow>
          ))}
        </div>
      </section>

      <section className="content-section">
        <div className="section-heading">
          <div>
            <p className="section-kicker">History</p>
            <h2>Recent outcomes</h2>
          </div>
        </div>
        <div className="data-table">
          <div className="data-header outcome-grid">
            <span>Run</span>
            <span>Outcome</span>
            <span>Workflow</span>
            <span>Duration</span>
            <span />
          </div>
          {recentRuns.map((run) => (
            <DataRow key={run.id} label={`Open run ${run.title}`} onClick={() => navigate({ page: "run", id: run.id })}>
              <span className="row-primary">
                <span className="row-title">{run.title}</span>
                <span className="row-subtitle">{run.issue}</span>
              </span>
              <StatusBadge status={run.status} />
              <span>{workflowForRun(run).name}</span>
              <span className="mono">{run.duration}</span>
            </DataRow>
          ))}
        </div>
      </section>

      {warningVisible && instanceWarnings.map((warning) => (
        <section className="warning-strip" key={warning.code}>
          <span className="warning-code">{warning.code}</span>
          <span>
            <strong>{warning.title}</strong>
            <small>{warning.detail}</small>
          </span>
          <button
            className="icon-button"
            aria-label="Dismiss warning preview"
            onClick={() => setWarningVisible(false)}
            type="button"
          >
            <Icon name="close" size={16} />
          </button>
        </section>
      ))}
    </>
  );
}

function WorkflowsPage({ navigate }: { navigate: (route: Route) => void }) {
  return (
    <>
      <header className="page-heading page-heading-row">
        <div>
          <p className="page-kicker">Definitions</p>
          <h1>Workflows</h1>
          <p>Versioned processes currently provisioned in the local instance.</p>
        </div>
        <div className="scope-chip">
          <span className="scope-mark">G</span>
          goobers gaggle
        </div>
      </header>

      <section className="content-section">
        <div className="data-table">
          <div className="data-header workflow-grid">
            <span>Workflow</span>
            <span>Trigger</span>
            <span>Concurrency</span>
            <span>Last outcome</span>
            <span />
          </div>
          {workflows.map((workflow) => (
            <DataRow
              key={workflow.id}
              label={`Open workflow ${workflow.name}`}
              onClick={() => navigate({ page: "workflow", id: workflow.id })}
            >
              <span className="row-primary">
                <span className="row-title">{workflow.name}</span>
                <span className="row-subtitle">{workflow.description}</span>
              </span>
              <span>{workflow.trigger}</span>
              <span>
                {workflow.activeRuns} active / {workflow.maxConcurrency} max
              </span>
              <span className="outcome-cell">
                <StatusBadge status={workflow.lastOutcome} />
                <small>{workflow.lastRunAt}</small>
              </span>
            </DataRow>
          ))}
        </div>
      </section>
    </>
  );
}

function RunsPage({ navigate }: { navigate: (route: Route) => void }) {
  const [filter, setFilter] = useState<"all" | "active" | "attention" | "complete">("all");
  const filteredRuns = runs.filter((run) => {
    if (filter === "active") {
      return run.status === "running";
    }
    if (filter === "attention") {
      return run.status === "escalated" || run.status === "failed";
    }
    if (filter === "complete") {
      return run.status === "completed";
    }
    return true;
  });

  return (
    <>
      <header className="page-heading">
        <p className="page-kicker">Journal</p>
        <h1>Runs</h1>
        <p>Every execution, ordered by the most recent journal activity.</p>
      </header>

      <div className="filter-bar" aria-label="Filter runs">
        {(["all", "active", "attention", "complete"] as const).map((option) => (
          <button
            className={filter === option ? "filter-button filter-button-active" : "filter-button"}
            key={option}
            onClick={() => setFilter(option)}
            type="button"
          >
            {option === "all" ? "All runs" : option}
          </button>
        ))}
      </div>

      <section className="content-section">
        <div className="data-table">
          <div className="data-header all-runs-grid">
            <span>Run</span>
            <span>Status</span>
            <span>Current stage</span>
            <span>Started</span>
            <span>Duration</span>
            <span />
          </div>
          {filteredRuns.map((run) => (
            <DataRow key={run.id} label={`Open run ${run.title}`} onClick={() => navigate({ page: "run", id: run.id })}>
              <span className="row-primary">
                <span className="row-title">{run.title}</span>
                <span className="row-subtitle">
                  {run.issue} · {workflowForRun(run).name}
                </span>
              </span>
              <StatusBadge status={run.status} />
              <span>{run.currentStage}</span>
              <span>{run.startedAt}</span>
              <span className="mono">{run.duration}</span>
            </DataRow>
          ))}
        </div>
      </section>
    </>
  );
}

function StageDefinition({ stage }: { stage: WorkflowStage }) {
  return (
    <aside className="definition-panel">
      <div className="inspector-heading">
        <span className={`primitive-icon primitive-${stage.kind}`}>
          <Icon name={stage.kind === "gate" ? "gate" : stage.kind === "agentic" ? "code" : "workflow"} size={17} />
        </span>
        <div>
          <span>{stage.kind}</span>
          <h3>{stage.name}</h3>
        </div>
      </div>
      <p className="inspector-description">{stage.description}</p>
      <dl className="property-list">
        <div>
          <dt>{stage.kind === "gate" ? "Evaluator" : "Goober"}</dt>
          <dd>{stage.evaluator ?? stage.goober}</dd>
        </div>
        <div>
          <dt>Policy</dt>
          <dd>{stage.retry}</dd>
        </div>
      </dl>
      <div className="code-heading">
        <span>Definition</span>
        <span>YAML</span>
      </div>
      <pre className="code-block">{stage.yaml}</pre>
    </aside>
  );
}

function WorkflowPage({ workflow, navigate }: { workflow: Workflow; navigate: (route: Route) => void }) {
  const [selectedStageId, setSelectedStageId] = useState(workflow.stages[0]?.id ?? "");
  const selectedStage = workflow.stages.find((stage) => stage.id === selectedStageId) ?? workflow.stages[0];
  const workflowRuns = runs.filter((run) => run.workflowId === workflow.id);

  return (
    <>
      <nav className="breadcrumbs" aria-label="Breadcrumb">
        <button onClick={() => navigate({ page: "workflows" })} type="button">
          Workflows
        </button>
        <Icon name="chevron" size={14} />
        <span>{workflow.name}</span>
      </nav>
      <header className="detail-heading">
        <div>
          <span className="definition-label">Workflow definition</span>
          <h1>{workflow.name}</h1>
          <p>{workflow.description}</p>
        </div>
        <dl className="detail-meta">
          <div>
            <dt>Trigger</dt>
            <dd>{workflow.trigger}</dd>
          </div>
          <div>
            <dt>Concurrency</dt>
            <dd>{workflow.activeRuns} / {workflow.maxConcurrency}</dd>
          </div>
          <div>
            <dt>Gaggle</dt>
            <dd>{workflow.gaggle}</dd>
          </div>
        </dl>
      </header>

      <section className="graph-layout">
        <div className="graph-panel">
          <div className="panel-heading-row">
            <div>
              <p className="section-kicker">Structure</p>
              <h2>Execution graph</h2>
            </div>
            <span className="graph-legend">Select a stage to inspect it</span>
          </div>
          <TopologyGraph
            onSelectStage={setSelectedStageId}
            selectedStageId={selectedStageId}
            workflow={workflow}
          />
        </div>
        {selectedStage && <StageDefinition stage={selectedStage} />}
      </section>

      <section className="content-section">
        <div className="section-heading">
          <div>
            <p className="section-kicker">History</p>
            <h2>Recent runs</h2>
          </div>
        </div>
        <div className="data-table">
          {workflowRuns.map((run) => (
            <DataRow key={run.id} label={`Open run ${run.title}`} onClick={() => navigate({ page: "run", id: run.id })}>
              <span className="row-primary">
                <span className="row-title">{run.title}</span>
                <span className="row-subtitle">{run.issue} · {run.startedAt}</span>
              </span>
              <StatusBadge status={run.status} />
              <span>{run.currentStage}</span>
              <span className="mono">{run.duration}</span>
            </DataRow>
          ))}
        </div>
      </section>
    </>
  );
}

function deriveNodeStates(workflow: Workflow, events: RunEvent[], index: number): Record<string, NodeState> {
  const states = Object.fromEntries(workflow.stages.map((stage) => [stage.id, "pending" as NodeState]));
  let activeStageId: string | undefined;
  for (const event of events.slice(0, index + 1)) {
    if (!event.stageId) {
      continue;
    }
    if (event.type === "stage.started") {
      if (activeStageId && activeStageId !== event.stageId && states[activeStageId] === "active") {
        states[activeStageId] = "complete";
      }
      states[event.stageId] = "active";
      activeStageId = event.stageId;
    } else if (event.type === "stage.finished" || event.type === "artifact.recorded") {
      if (event.type === "stage.finished") {
        states[event.stageId] = event.tone === "danger" ? "failed" : "complete";
        if (activeStageId === event.stageId) {
          activeStageId = undefined;
        }
      }
    } else if (event.type === "gate.evaluated") {
      if (activeStageId && activeStageId !== event.stageId && states[activeStageId] === "active") {
        states[activeStageId] = "complete";
      }
      states[event.stageId] = event.tone === "danger" ? "escalated" : "complete";
      activeStageId = undefined;
    } else if (event.type === "run.finished") {
      if (activeStageId && activeStageId !== event.stageId && states[activeStageId] === "active") {
        states[activeStageId] = "complete";
      }
      states[event.stageId] = event.tone === "danger" ? "escalated" : "complete";
      activeStageId = undefined;
    }
  }
  return states;
}

function deriveTraversedEdges(events: RunEvent[], index: number): Set<string> {
  const traversed = new Set<string>();
  let currentStageId: string | undefined;
  for (const event of events.slice(0, index + 1)) {
    if (!event.stageId) {
      continue;
    }
    const entersStage =
      event.type === "stage.started" ||
      event.type === "stage.finished" ||
      event.type === "gate.evaluated" ||
      event.type === "run.finished";
    if (!entersStage) {
      continue;
    }
    if (currentStageId && currentStageId !== event.stageId) {
      traversed.add(`${currentStageId}->${event.stageId}`);
    }
    currentStageId = event.stageId;
  }
  return traversed;
}

function visibleAttempts(run: Run, stageId: string, eventSeq: number): StageAttempt[] {
  return run.attempts
    .filter((attempt) => attempt.stageId === stageId && attempt.startedSeq <= eventSeq)
    .map((attempt) => {
      if (attempt.endedSeq !== undefined && attempt.endedSeq <= eventSeq) {
        return attempt;
      }
      return {
        ...attempt,
        status: "running",
        duration: "In progress",
        output: undefined,
        artifacts: [],
      };
    });
}

function AttemptInspector({
  run,
  stage,
  eventSeq,
}: {
  run: Run;
  stage: WorkflowStage;
  eventSeq: number;
}) {
  const attempts = visibleAttempts(run, stage.id, eventSeq);
  const [selectedAttemptNumber, setSelectedAttemptNumber] = useState<number>();
  const selectedAttempt =
    attempts.find((attempt) => attempt.number === selectedAttemptNumber) ?? attempts[attempts.length - 1];

  useEffect(() => {
    setSelectedAttemptNumber(undefined);
  }, [stage.id, eventSeq]);

  return (
    <aside className="run-inspector">
      <div className="inspector-heading">
        <span className={`primitive-icon primitive-${stage.kind}`}>
          <Icon name={stage.kind === "gate" ? "gate" : stage.kind === "agentic" ? "code" : "workflow"} size={17} />
        </span>
        <div>
          <span>{stage.kind}</span>
          <h3>{stage.name}</h3>
        </div>
      </div>

      {attempts.length === 0 ? (
        <div className="not-reached">
          <span>Not reached at this point</span>
          <small>Move the playhead forward to inspect this stage.</small>
        </div>
      ) : (
        <>
          <div className="attempt-switcher" aria-label="Stage attempts">
            {attempts.map((attempt) => (
              <button
                className={selectedAttempt?.id === attempt.id ? "attempt-button attempt-button-active" : "attempt-button"}
                key={attempt.id}
                onClick={() => setSelectedAttemptNumber(attempt.number)}
                type="button"
              >
                Attempt {attempt.number}
              </button>
            ))}
          </div>
          {selectedAttempt && (
            <div className="attempt-content">
              <div className="attempt-summary-row">
                <span className={`attempt-state attempt-${selectedAttempt.status}`}>{selectedAttempt.status}</span>
                <span className="mono">{selectedAttempt.duration}</span>
                <span>{selectedAttempt.kind}</span>
              </div>
              <p>{selectedAttempt.summary}</p>
              {selectedAttempt.output && (
                <div className="output-line">
                  <span>Output</span>
                  <code>{selectedAttempt.output}</code>
                </div>
              )}
              <div className="artifact-heading">
                <span>Artifacts</span>
                <span>{selectedAttempt.artifacts.length}</span>
              </div>
              {selectedAttempt.artifacts.length === 0 ? (
                <p className="empty-detail">No artifacts recorded yet.</p>
              ) : (
                <div className="artifact-list">
                  {selectedAttempt.artifacts.map((artifact) => (
                    <div className="artifact-row" key={artifact.name}>
                      <Icon name="artifact" size={17} />
                      <span>
                        <strong>{artifact.name}</strong>
                        <small>{artifact.summary}</small>
                      </span>
                      <span className="artifact-size">{artifact.size}</span>
                    </div>
                  ))}
                </div>
              )}
            </div>
          )}
        </>
      )}

      <details className="definition-disclosure">
        <summary>Stage definition</summary>
        <p>{stage.description}</p>
        <pre className="code-block">{stage.yaml}</pre>
      </details>
    </aside>
  );
}

function RunPage({ run, navigate }: { run: Run; navigate: (route: Route) => void }) {
  const workflow = workflowForRun(run);
  const [replayIndex, setReplayIndex] = useState(run.events.length - 1);
  const [isPlaying, setIsPlaying] = useState(false);
  const [speed, setSpeed] = useState(1);
  const currentEvent = run.events[replayIndex] ?? run.events[0];
  const [selectedStageId, setSelectedStageId] = useState(
    currentEvent?.stageId ?? workflow.stages[0]?.id ?? "",
  );
  const selectedStage =
    workflow.stages.find((stage) => stage.id === selectedStageId) ?? workflow.stages[0];
  const nodeStates = deriveNodeStates(workflow, run.events, replayIndex);
  const traversedEdges = deriveTraversedEdges(run.events, replayIndex);

  useEffect(() => {
    if (!isPlaying) {
      return;
    }
    if (replayIndex >= run.events.length - 1) {
      setIsPlaying(false);
      return;
    }
    const timeout = window.setTimeout(() => {
      setReplayIndex((current) => Math.min(current + 1, run.events.length - 1));
    }, Math.max(110, 850 / speed));
    return () => window.clearTimeout(timeout);
  }, [isPlaying, replayIndex, run.events.length, speed]);

  useEffect(() => {
    if (currentEvent?.stageId) {
      setSelectedStageId(currentEvent.stageId);
    }
  }, [currentEvent]);

  const togglePlayback = () => {
    if (replayIndex >= run.events.length - 1) {
      setReplayIndex(0);
    }
    setIsPlaying((current) => !current);
  };

  return (
    <>
      <nav className="breadcrumbs" aria-label="Breadcrumb">
        <button onClick={() => navigate({ page: "runs" })} type="button">
          Runs
        </button>
        <Icon name="chevron" size={14} />
        <span>{run.shortId}</span>
      </nav>
      <header className="run-heading">
        <div>
          <div className="run-title-line">
            <StatusBadge status={run.status} />
            <span className="mono run-id">{run.shortId}</span>
          </div>
          <h1>{run.title}</h1>
          <p>
            {run.issue} · {workflow.name} v{workflow.version} · {run.startedAt}
          </p>
        </div>
        <dl className="run-meta">
          <div>
            <dt>Duration</dt>
            <dd>{run.duration}</dd>
          </div>
          <div>
            <dt>Repasses</dt>
            <dd>{run.repasses} / 3</dd>
          </div>
          <div>
            <dt>Trigger</dt>
            <dd>{run.trigger}</dd>
          </div>
          <div>
            <dt>Workflow pin</dt>
            <dd className="mono">v{workflow.version} · {workflow.digest.slice(7, 15)}</dd>
          </div>
        </dl>
      </header>

      {run.escalation && replayIndex >= run.events.length - 2 && (
        <section className="escalation-panel">
          <span className="escalation-icon">
            <Icon name="alert" />
          </span>
          <div>
            <span className="escalation-label">Escalation cause</span>
            <h2>{run.escalation.title}</h2>
            <p>{run.escalation.cause}</p>
            <dl>
              <div>
                <dt>Gate</dt>
                <dd>{run.escalation.gate}</dd>
              </div>
              <div>
                <dt>Branch</dt>
                <dd className="mono">{run.escalation.branch}</dd>
              </div>
              <div>
                <dt>Budget</dt>
                <dd>{run.escalation.attemptsUsed} / {run.escalation.attemptsAllowed} repasses</dd>
              </div>
            </dl>
          </div>
        </section>
      )}

      <section className="run-workspace">
        <div className="run-primary">
          <div className="graph-panel run-graph-panel">
            <div className="panel-heading-row">
              <div>
                <p className="section-kicker">Structure</p>
                <h2>Execution graph</h2>
              </div>
              <button
                className="workflow-link"
                onClick={() => navigate({ page: "workflow", id: workflow.id })}
                type="button"
              >
                View definition <Icon name="arrow" size={14} />
              </button>
            </div>
            <TopologyGraph
              activeEdges={traversedEdges}
              onSelectStage={setSelectedStageId}
              selectedStageId={selectedStageId}
              states={nodeStates}
              workflow={workflow}
            />
          </div>

          <div className="playback-panel">
            <div className="playback-now">
              <span className={`event-mark event-${currentEvent?.tone ?? "neutral"}`} />
              <div>
                <span>
                  Event {replayIndex + 1} of {run.events.length} · {currentEvent?.elapsed}
                </span>
                <strong>{currentEvent?.title}</strong>
              </div>
            </div>
            <div className="playback-controls">
              <button
                aria-label={isPlaying ? "Pause replay" : "Play replay"}
                className="play-button"
                onClick={togglePlayback}
                type="button"
              >
                <Icon name={isPlaying ? "pause" : "play"} size={17} />
              </button>
              <input
                aria-label="Replay position"
                max={run.events.length - 1}
                min={0}
                onChange={(event) => {
                  setIsPlaying(false);
                  setReplayIndex(Number(event.target.value));
                }}
                type="range"
                value={replayIndex}
              />
              <div className="speed-control" aria-label="Replay speed">
                {[1, 5, 10].map((value) => (
                  <button
                    className={speed === value ? "speed-button speed-button-active" : "speed-button"}
                    key={value}
                    onClick={() => setSpeed(value)}
                    type="button"
                  >
                    {value}x
                  </button>
                ))}
              </div>
            </div>
          </div>

          <div className="event-ledger">
            <div className="panel-heading-row">
              <div>
                <p className="section-kicker">Journal</p>
                <h2>Event ledger</h2>
              </div>
              <span className="graph-legend">Ordered by durable sequence</span>
            </div>
            <ol>
              {run.events.map((event, index) => (
                <li className={index === replayIndex ? "ledger-item ledger-item-active" : "ledger-item"} key={event.id}>
                  <button
                    onClick={() => {
                      setIsPlaying(false);
                      setReplayIndex(index);
                    }}
                    type="button"
                  >
                    <span className="ledger-seq mono">{String(event.seq).padStart(2, "0")}</span>
                    <span className={`event-mark event-${event.tone}`} />
                    <span className="ledger-copy">
                      <strong>{event.title}</strong>
                      <span>{event.detail}</span>
                    </span>
                    <span className="ledger-type mono">{event.type}</span>
                    <span className="ledger-time mono">{event.elapsed}</span>
                  </button>
                </li>
              ))}
            </ol>
          </div>
        </div>

        {selectedStage && <AttemptInspector eventSeq={currentEvent?.seq ?? 0} run={run} stage={selectedStage} />}
      </section>
    </>
  );
}

export function App() {
  const [route, setRoute] = useState<Route>(() => parseRoute());
  const [theme, setTheme] = useState<Theme>(() => storedTheme());

  useEffect(() => {
    const onHashChange = () => setRoute(parseRoute());
    window.addEventListener("hashchange", onHashChange);
    return () => window.removeEventListener("hashchange", onHashChange);
  }, []);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    persistTheme(theme);
  }, [theme]);

  const navigate = (nextRoute: Route) => {
    const nextHash = routeHash(nextRoute);
    if (window.location.hash === nextHash) {
      setRoute(nextRoute);
    } else {
      window.location.hash = nextHash;
    }
  };

  const area = activeArea(route);
  const run = route.page === "run" ? runs.find((candidate) => candidate.id === route.id) : undefined;
  const workflow =
    route.page === "workflow" ? workflows.find((candidate) => candidate.id === route.id) : undefined;

  return (
    <div className="portal-frame">
      <aside className="sidebar">
        <button className="brand" onClick={() => navigate({ page: "overview" })} type="button">
          <img alt="" src="/goober-mascot.png" />
          <span>
            <strong>goobers</strong>
            <small>local operations</small>
          </span>
        </button>

        <nav className="primary-nav" aria-label="Primary">
          <button
            className={area === "overview" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "overview" })}
            type="button"
          >
            <Icon name="overview" />
            Overview
          </button>
          <button
            className={area === "workflows" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "workflows" })}
            type="button"
          >
            <Icon name="workflow" />
            Workflows
            <span className="nav-count">{workflows.length}</span>
          </button>
          <button
            className={area === "runs" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "runs" })}
            type="button"
          >
            <Icon name="run" />
            Runs
            <span className="nav-count">{runs.length}</span>
          </button>
        </nav>

        <div className="sidebar-status">
          <div>
            <span className="live-mark" />
            <span>
              <strong>local-dev</strong>
              <small>127.0.0.1 · connected</small>
            </span>
          </div>
          <span className="version">v0.6</span>
        </div>
      </aside>

      <div className="portal-main">
        <header className="topbar">
          <div className="topbar-context">
            <span className="scope-mark">G</span>
            <span>
              <strong>goobers</strong>
              <small>1 gaggle · 4 goobers</small>
            </span>
          </div>
          <div className="topbar-actions">
            <span className="prototype-label">Interactive prototype</span>
            <button
              aria-label={`Use ${theme === "light" ? "dark" : "light"} theme`}
              className="theme-button"
              onClick={() => setTheme((current) => (current === "light" ? "dark" : "light"))}
              type="button"
            >
              <Icon name={theme === "light" ? "moon" : "sun"} size={17} />
            </button>
          </div>
        </header>

        <main className="page-content">
          {route.page === "overview" && <OverviewPage navigate={navigate} />}
          {route.page === "workflows" && <WorkflowsPage navigate={navigate} />}
          {route.page === "runs" && <RunsPage navigate={navigate} />}
          {route.page === "workflow" && workflow && <WorkflowPage navigate={navigate} workflow={workflow} />}
          {route.page === "run" && run && <RunPage key={run.id} navigate={navigate} run={run} />}
          {route.page === "workflow" && !workflow && <p>Workflow not found.</p>}
          {route.page === "run" && !run && <p>Run not found.</p>}
        </main>
      </div>
    </div>
  );
}
