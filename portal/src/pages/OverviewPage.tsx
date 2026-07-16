import { useState } from "react";
import { DataList, DataRow } from "../foundation/DataList";
import { Icon } from "../foundation/Icon";
import { StatusBadge } from "../foundation/StatusBadge";
import {
  instanceWarnings,
  runs,
  workflowForRun,
  workflows,
} from "../fixtures";
import type { Route } from "../routes";

export function OverviewPage({ navigate }: { navigate: (route: Route) => void }) {
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
          <span aria-hidden="true" className="live-mark" />
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
            aria-label={`Open attention run ${attentionRun.title}`}
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
        <DataList
          headers={["Run", "Workflow", "Current stage", "Elapsed", ""]}
          label="Active runs"
          layout="run-grid"
        >
          {activeRuns.map((run) => (
            <DataRow
              key={run.id}
              label={`Open run ${run.title}`}
              layout="run-grid"
              onClick={() => navigate({ page: "run", id: run.id })}
            >
              <span className="row-primary">
                <span className="row-title">{run.title}</span>
                <span className="row-subtitle">
                  {run.issue} · {run.shortId}
                </span>
              </span>
              <span>{workflowForRun(run).name}</span>
              <span className="stage-progress">
                <span aria-hidden="true" className="stage-progress-mark" />
                {run.currentStage}
              </span>
              <span className="mono">{run.duration}</span>
            </DataRow>
          ))}
        </DataList>
      </section>

      <section className="content-section">
        <div className="section-heading">
          <div>
            <p className="section-kicker">History</p>
            <h2>Recent outcomes</h2>
          </div>
        </div>
        <DataList
          headers={["Run", "Outcome", "Workflow", "Duration", ""]}
          label="Recent outcomes"
          layout="outcome-grid"
        >
          {recentRuns.map((run) => (
            <DataRow
              key={run.id}
              label={`Open run ${run.title}`}
              layout="outcome-grid"
              onClick={() => navigate({ page: "run", id: run.id })}
            >
              <span className="row-primary">
                <span className="row-title">{run.title}</span>
                <span className="row-subtitle">{run.issue}</span>
              </span>
              <StatusBadge status={run.status} />
              <span>{workflowForRun(run).name}</span>
              <span className="mono">{run.duration}</span>
            </DataRow>
          ))}
        </DataList>
      </section>

      {warningVisible && instanceWarnings.map((warning) => (
        <section className="warning-strip" key={warning.code}>
          <span className="warning-code">{warning.code}</span>
          <span>
            <strong>{warning.title}</strong>
            <small>{warning.detail}</small>
          </span>
          <button
            aria-label="Dismiss warning preview"
            className="icon-button"
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
