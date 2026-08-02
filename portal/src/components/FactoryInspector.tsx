import type { FactoryFloorData } from "../factoryData";
import {
  capacityLabel,
  findCarrier,
  findLane,
  findStation,
  findWorker,
  holdReasonLabel,
  runStateLabel,
  stageKindLabel,
  stationStatusLabel,
  type FactoryAttentionItem,
  type FactoryCarrier,
  type FactoryFloorModel,
  type FactoryLane,
  type FactoryStation,
  type FactoryWorker,
} from "../factoryModel";
import type { FactorySelection } from "../factorySelection";
import { formatDuration, formatTimestamp } from "../runDetailData";
import { routeHash } from "../routing";
import { Icon } from "../ui/Icon";
import { Inspector } from "../ui/Inspector";
import { StatusBadge } from "../ui/StatusBadge";

/**
 * The right rail: what the selected thing actually is, in safe operational
 * terms, plus the links that take an operator to the page that can act on it.
 *
 * Only identifiers the portal already surfaces appear here: display names,
 * stage ids, run ids, phases, counts, timings and closed-set hold reasons.
 */
export function FactoryInspector({
  data,
  freshness,
  onSelect,
  selection,
}: {
  data: FactoryFloorData;
  freshness: { label: string; state: "live" | "refreshing" | "degraded" };
  onSelect: (selection: FactorySelection) => void;
  selection: FactorySelection;
}) {
  const { model } = data;
  return (
    <Inspector className="factory-inspector" label="Factory inspector">
      <div className="factory-inspector-status">
        <span
          aria-live="polite"
          className="factory-freshness"
          data-state={freshness.state}
          role="status"
        >
          <span aria-hidden="true" className="factory-freshness-mark" />
          {freshness.label}
        </span>
        {selection.kind !== "overview" && (
          <button
            className="text-button"
            onClick={() => onSelect({ kind: "overview" })}
            type="button"
          >
            Back to floor summary
          </button>
        )}
      </div>

      {selection.kind === "overview" && (
        <FloorSummary model={model} onSelect={onSelect} />
      )}
      {selection.kind === "run" && (
        <RunDetails
          carrier={findCarrier(model, selection.id)}
          model={model}
          onSelect={onSelect}
        />
      )}
      {selection.kind === "station" && (
        <StationDetails
          model={model}
          onSelect={onSelect}
          station={findStation(model, selection.id)}
        />
      )}
      {selection.kind === "worker" && (
        <WorkerDetails
          model={model}
          onSelect={onSelect}
          worker={findWorker(model, selection.id)}
        />
      )}
      {selection.kind === "lane" && (
        <LaneDetails
          lane={findLane(model, selection.id)}
          onSelect={onSelect}
          partial={model.runsTruncated}
        />
      )}
      {selection.kind === "gaggle" && (
        <GaggleDetails model={model} name={selection.name} />
      )}
    </Inspector>
  );
}

function FloorSummary({
  model,
  onSelect,
}: {
  model: FactoryFloorModel;
  onSelect: (selection: FactorySelection) => void;
}) {
  const { counts, capacity } = model;
  return (
    <>
      <div className="inspector-heading">
        <span className="primitive-icon primitive-deterministic">
          <Icon name="factory" size={17} />
        </span>
        <div>
          <span>Floor</span>
          <h3>Live plant summary</h3>
        </div>
      </div>

      <dl className="factory-metrics">
        <Metric label="Gaggles" value={counts.gaggles} />
        <Metric label="Workflows" value={counts.workflows} />
        <Metric label="Goobers" value={`${counts.goobers - counts.idleGoobers} / ${counts.goobers}`} sub="working / configured" />
        <Metric
          label="Active runs"
          value={`${counts.activeRuns}${model.runsTruncated ? "+" : ""}`}
          sub={model.runsTruncated ? "partial" : undefined}
        />
        <Metric
          label="Held runs"
          value={`${counts.blockedRuns}${model.runsTruncated ? "+" : ""}`}
        />
        <Metric
          label="Signals unread"
          value={`${counts.unreadRuns}${model.runsTruncated ? "+" : ""}`}
        />
        <Metric
          label="Human holds"
          value={`${counts.heldStages}${model.runsTruncated ? "+" : ""}`}
        />
        <Metric
          label="Blocked stages"
          value={`${counts.blockedStages}${model.runsTruncated ? "+" : ""}`}
        />
      </dl>

      <dl className="property-list">
        <div>
          <dt>Floor capacity</dt>
          <dd>
            {model.runsTruncated
              ? capacity.limit === undefined
                ? `${capacity.wip}+ running · partial view · workflow limit unknown`
                : `${capacity.wip}+ running · partial view · known workflow limits total ${capacity.limit}`
              : capacity.limit === undefined
              ? `${capacity.wip} running · limit unknown`
              : `${capacity.wip} / ${capacity.limit} concurrent runs`}
          </dd>
        </div>
        {capacity.unknownLimits > 0 && (
          <div>
            <dt>Unknown limits</dt>
            <dd>
              {capacity.unknownLimits} workflow
              {capacity.unknownLimits === 1 ? "" : "s"} report no concurrency limit
            </dd>
          </div>
        )}
        <div>
          <dt>Queued at inbound</dt>
          <dd>{counts.queuedRuns}</dd>
        </div>
        {model.runsTruncated && (
          <div>
            <dt>Floor bound</dt>
            <dd>Partial view. More active runs exist beyond the 50-run floor bound.</dd>
          </div>
        )}
      </dl>

      <section aria-label="Attention" className="factory-attention">
        <h4>Needs attention</h4>
        {model.attention.length === 0 ? (
          <p className="inline-empty">
            {counts.unreadRuns > 0
              ? "No confirmed hold is visible. Some run signals are unread."
              : "Nothing is held and no recent run failed or escalated."}
          </p>
        ) : (
          <ul>
            {model.attention.map((item) => (
              <li key={item.id}>
                <AttentionRow item={item} onSelect={onSelect} />
              </li>
            ))}
          </ul>
        )}
      </section>
    </>
  );
}

function AttentionRow({
  item,
  onSelect,
}: {
  item: FactoryAttentionItem;
  onSelect: (selection: FactorySelection) => void;
}) {
  return (
    <div className="factory-attention-row" data-kind={item.kind}>
      <div>
        <span className="mono factory-attention-run">{item.runId}</span>
        <small>
          {item.workflowDisplayName}
          {item.stageId ? ` · ${item.stageId}` : ""}
        </small>
      </div>
      <div className="factory-attention-meta">
        {item.kind === "blocked-run" ? (
          <span className="factory-hold-chip">{holdReasonLabel(item.reason)}</span>
        ) : (
          <StatusBadge status={item.phase} />
        )}
        {item.kind === "blocked-run" ? (
          <button
            className="text-button"
            onClick={() => onSelect({ kind: "run", id: item.runId })}
            type="button"
          >
            Inspect
          </button>
        ) : (
          <a className="text-button" href={routeHash({ page: "run", id: item.runId })}>
            Open run
          </a>
        )}
      </div>
    </div>
  );
}

function RunDetails({
  carrier,
  model,
  onSelect,
}: {
  carrier: FactoryCarrier | undefined;
  model: FactoryFloorModel;
  onSelect: (selection: FactorySelection) => void;
}) {
  if (!carrier) {
    return <MissingSelection subject="run" />;
  }
  const station = findStation(model, carrier.stationId);
  const owner = carrier.ownerWorkerId ? findWorker(model, carrier.ownerWorkerId) : undefined;
  return (
    <>
      <div className="inspector-heading">
        <span className="primitive-icon primitive-agentic">
          <Icon name="run" size={17} />
        </span>
        <div>
          <span>Work in progress</span>
          <h3 className="mono">{carrier.runId}</h3>
        </div>
      </div>

      <div className="factory-state-line" data-state={carrier.state}>
        <span className="factory-state-chip">{runStateLabel(carrier.state)}</span>
        {carrier.reason && <span>{holdReasonLabel(carrier.reason)}</span>}
        {!carrier.confirmed && (
          <span className="factory-state-note">stage signal unread</span>
        )}
      </div>

      <dl className="property-list">
        <div>
          <dt>Gaggle</dt>
          <dd>{carrier.gaggle}</dd>
        </div>
        <div>
          <dt>Workflow</dt>
          <dd>{carrier.workflowDisplayName}</dd>
        </div>
        <div>
          <dt>Stage</dt>
          <dd>
            {carrier.stageId ? (
              <button
                className="text-button"
                onClick={() => onSelect({ kind: "station", id: carrier.stationId })}
                type="button"
              >
                {carrier.stageId}
              </button>
            ) : (
              "Not started"
            )}
          </dd>
        </div>
        <div>
          <dt>Phase</dt>
          <dd>
            <StatusBadge status={carrier.phase} />
          </dd>
        </div>
        <div>
          <dt>Elapsed</dt>
          <dd className="mono">{formatDuration(carrier.durationMillis)}</dd>
        </div>
        <div>
          <dt>Last activity</dt>
          <dd>
            <time dateTime={carrier.lastActivityAt}>
              {formatTimestamp(carrier.lastActivityAt)}
            </time>
          </dd>
        </div>
        <div>
          <dt>Trigger</dt>
          <dd>{carrier.triggerKind}</dd>
        </div>
        <div>
          <dt>Retries</dt>
          <dd>
            {carrier.retryCount} total · {carrier.policyRetryCount} policy ·{" "}
            {carrier.infraRetryCount} infra
          </dd>
        </div>
        <div>
          <dt>Repasses</dt>
          <dd>{carrier.repassCount}</dd>
        </div>
        {owner && (
          <div>
            <dt>Stage owner</dt>
            <dd>
              <button
                className="text-button"
                onClick={() => onSelect({ kind: "worker", id: owner.id })}
                type="button"
              >
                {owner.displayName}
              </button>
            </dd>
          </div>
        )}
        {station && (
          <div>
            <dt>Stage load</dt>
            <dd>{capacityLabel(station.wip, station.limit)}</dd>
          </div>
        )}
      </dl>

      <div className="factory-links">
        <a className="factory-link" href={routeHash({ page: "run", id: carrier.runId })}>
          Open run <Icon name="arrow" size={15} />
        </a>
        <a
          className="factory-link"
          href={routeHash({
            page: "workflow",
            gaggle: carrier.gaggle,
            id: carrier.workflow,
          })}
        >
          Open workflow <Icon name="arrow" size={15} />
        </a>
      </div>
    </>
  );
}

function StationDetails({
  model,
  onSelect,
  station,
}: {
  model: FactoryFloorModel;
  onSelect: (selection: FactorySelection) => void;
  station: FactoryStation | undefined;
}) {
  if (!station) {
    return <MissingSelection subject="stage" />;
  }
  const workers = model.workers.filter((worker) =>
    worker.activeStationIds.includes(station.id),
  );
  const runs = model.carriers.filter((carrier) => carrier.stationId === station.id);
  return (
    <>
      <div className="inspector-heading">
        <span className={`primitive-icon primitive-${station.kind}`}>
          <Icon
            name={
              station.kind === "gate" ? "gate" : station.kind === "agentic" ? "code" : "workflow"
            }
            size={17}
          />
        </span>
        <div>
          <span>{stageKindLabel(station.kind)}</span>
          <h3>{station.stageId}</h3>
        </div>
      </div>

      <div className="factory-state-line" data-state={station.status}>
        <span className="factory-state-chip">{stationStatusLabel(station.status)}</span>
        {station.alarm === "blocked" && (
          <span className="factory-alarm-text">Hard blocked work is present</span>
        )}
        {station.alarm === "hold" && (
          <span className="factory-hold-text">Every run here is paused at a human gate</span>
        )}
        {station.unknownCount > 0 && (
          <span className="factory-state-note">
            {station.unknownCount} run signal
            {station.unknownCount === 1 ? "" : "s"} unread
          </span>
        )}
      </div>

      <dl className="property-list">
        <div>
          <dt>Workflow</dt>
          <dd>{station.workflowDisplayName}</dd>
        </div>
        <div>
          <dt>Gaggle</dt>
          <dd>{station.gaggle}</dd>
        </div>
        <div>
          <dt>Owner</dt>
          <dd>
            {station.owner
              ? (station.owner.displayName ?? `${station.owner.gaggle}/${station.owner.name}`)
              : station.kind === "deterministic"
                ? "Deterministic runtime"
                : "None declared"}
          </dd>
        </div>
        {station.kind === "gate" && (
          <div>
            <dt>Evaluator</dt>
            <dd>{station.evaluator ?? "Not declared"}</dd>
          </div>
        )}
        <div>
          <dt>Load</dt>
          <dd>{capacityLabel(station.wip, station.limit)}</dd>
        </div>
        <div>
          <dt>Held here</dt>
          <dd>{station.blockedCount}</dd>
        </div>
        <div>
          <dt>Signals unread</dt>
          <dd>{station.unknownCount}</dd>
        </div>
        <div>
          <dt>Stage source</dt>
          <dd>
            {station.source === "declared"
              ? "Workflow definition"
              : "Observed from live runs"}
          </dd>
        </div>
      </dl>

      <section aria-label="Runs at this stage" className="factory-attention">
        <h4>Runs at this stage</h4>
        {runs.length === 0 ? (
          <p className="inline-empty">No active run is standing on this stage.</p>
        ) : (
          <ul>
            {runs.map((run) => (
              <li key={run.runId}>
                <div className="factory-attention-row" data-kind="station-run">
                  <div>
                    <span className="mono factory-attention-run">{run.runId}</span>
                    <small>{runStateLabel(run.state)}</small>
                  </div>
                  <button
                    className="text-button"
                    onClick={() => onSelect({ kind: "run", id: run.runId })}
                    type="button"
                  >
                    Inspect
                  </button>
                </div>
              </li>
            ))}
          </ul>
        )}
      </section>

      {workers.length > 0 && (
        <p className="factory-inspector-note">
          Staffed by {workers.map((worker) => worker.displayName).join(", ")}.
        </p>
      )}

      <div className="factory-links">
        <a
          className="factory-link"
          href={routeHash({
            page: "workflow",
            gaggle: station.gaggle,
            id: station.workflow,
          })}
        >
          Open workflow <Icon name="arrow" size={15} />
        </a>
        <a
          className="factory-link"
          href={routeHash({
            page: "runs",
            filters: {
              gaggle: station.gaggle,
              workflow: station.workflow,
              stage: station.stageId,
            },
          })}
        >
          Runs at this stage <Icon name="arrow" size={15} />
        </a>
      </div>
    </>
  );
}

function WorkerDetails({
  model,
  onSelect,
  worker,
}: {
  model: FactoryFloorModel;
  onSelect: (selection: FactorySelection) => void;
  worker: FactoryWorker | undefined;
}) {
  if (!worker) {
    return <MissingSelection subject="goober" />;
  }
  return (
    <>
      <div className="inspector-heading">
        <span className="primitive-icon primitive-agentic">
          <Icon name="code" size={17} />
        </span>
        <div>
          <span>Goober</span>
          <h3>{worker.displayName}</h3>
        </div>
      </div>
      <dl className="property-list">
        <div>
          <dt>Gaggle</dt>
          <dd>{worker.gaggleDisplayName}</dd>
        </div>
        <div>
          <dt>Harness</dt>
          <dd>{worker.harness}</dd>
        </div>
        <div>
          <dt>Definition</dt>
          <dd>{worker.status}</dd>
        </div>
        <div>
          <dt>Active runs</dt>
          <dd>{worker.activeRunCount}</dd>
        </div>
        <div>
          <dt>Posted at</dt>
          <dd>{worker.idle ? "Ready commons" : `${worker.activeStationIds.length} stages`}</dd>
        </div>
      </dl>

      <section aria-label="Owned stages" className="factory-attention">
        <h4>Owned stages</h4>
        {worker.stages.length === 0 ? (
          <p className="inline-empty">This goober owns no workflow stages.</p>
        ) : (
          <ul>
            {worker.stages.map((stage) => {
              const station = findStation(model, stage.stationId);
              return (
                <li key={stage.stationId}>
                  <div className="factory-attention-row" data-kind="worker-stage">
                    <div>
                      <span className="factory-attention-run">{stage.stage}</span>
                      <small>
                        {stage.workflow} · {stage.kind}
                      </small>
                    </div>
                    {station ? (
                      <button
                        className="text-button"
                        onClick={() => onSelect({ kind: "station", id: stage.stationId })}
                        type="button"
                      >
                        {station.wip} running
                      </button>
                    ) : (
                      <span className="factory-attention-note">out of scope</span>
                    )}
                  </div>
                </li>
              );
            })}
          </ul>
        )}
      </section>

      <div className="factory-links">
        <a className="factory-link" href={routeHash({ page: "gaggle", id: worker.gaggle })}>
          Open gaggle <Icon name="arrow" size={15} />
        </a>
      </div>
    </>
  );
}

function LaneDetails({
  lane,
  onSelect,
  partial,
}: {
  lane: FactoryLane | undefined;
  onSelect: (selection: FactorySelection) => void;
  partial: boolean;
}) {
  if (!lane) {
    return <MissingSelection subject="workflow" />;
  }
  return (
    <>
      <div className="inspector-heading">
        <span className="primitive-icon primitive-deterministic">
          <Icon name="workflow" size={17} />
        </span>
        <div>
          <span>Workflow line</span>
          <h3>{lane.displayName}</h3>
        </div>
      </div>

      <dl className="property-list">
        <div>
          <dt>Gaggle</dt>
          <dd>
            <button
              className="text-button"
              onClick={() => onSelect({ kind: "gaggle", name: lane.gaggle })}
              type="button"
            >
              {lane.gaggleDisplayName}
            </button>
          </dd>
        </div>
        <div>
          <dt>Stages</dt>
          <dd>
            {lane.stageCount} configured, {lane.stations.length} drawn
          </dd>
        </div>
        <div>
          <dt>Active runs</dt>
          <dd>
            {partial
              ? `At least ${lane.activeRuns} active · workflow limit ${lane.limit ?? "unknown"}`
              : capacityLabel(lane.activeRuns, lane.limit)}
          </dd>
        </div>
        <div>
          <dt>Held runs</dt>
          <dd>{lane.blockedRuns}</dd>
        </div>
        <div>
          <dt>Signals unread</dt>
          <dd>{lane.unreadRuns}</dd>
        </div>
        <div>
          <dt>Topology</dt>
          <dd>
            {lane.source === "declared"
              ? "Read from the workflow definition"
              : lane.stations.length === 0
                ? "Topology not read in this batch"
                : "Observed from live runs only; order unknown"}
          </dd>
        </div>
      </dl>

      <div className="factory-links">
        <a
          className="factory-link"
          href={routeHash({ page: "workflow", gaggle: lane.gaggle, id: lane.workflow })}
        >
          Open workflow <Icon name="arrow" size={15} />
        </a>
        <a
          className="factory-link"
          href={routeHash({
            page: "runs",
            filters: { gaggle: lane.gaggle, workflow: lane.workflow },
          })}
        >
          Runs for this workflow <Icon name="arrow" size={15} />
        </a>
      </div>
    </>
  );
}

function GaggleDetails({ model, name }: { model: FactoryFloorModel; name: string }) {
  const gaggle = model.gaggles.find((candidate) => candidate.name === name);
  if (!gaggle) {
    return <MissingSelection subject="gaggle" />;
  }
  return (
    <>
      <div className="inspector-heading">
        <span className="primitive-icon primitive-deterministic">
          <Icon name="overview" size={17} />
        </span>
        <div>
          <span>Gaggle</span>
          <h3>{gaggle.displayName}</h3>
        </div>
      </div>
      <dl className="property-list">
        <div>
          <dt>Definition</dt>
          <dd>{gaggle.status}</dd>
        </div>
        <div>
          <dt>Workflow lines</dt>
          <dd>{gaggle.workflowCount}</dd>
        </div>
        <div>
          <dt>Goobers</dt>
          <dd>{gaggle.gooberCount}</dd>
        </div>
        <div>
          <dt>Active runs</dt>
          <dd>{gaggle.activeRuns}</dd>
        </div>
        <div>
          <dt>Signals unread</dt>
          <dd>{gaggle.unreadRuns}</dd>
        </div>
        <div>
          <dt>Human holds</dt>
          <dd>{gaggle.heldStages}</dd>
        </div>
        <div>
          <dt>Blocked stages</dt>
          <dd>{gaggle.blockedStages}</dd>
        </div>
      </dl>
      <div className="factory-links">
        <a className="factory-link" href={routeHash({ page: "gaggle", id: gaggle.name })}>
          Open gaggle <Icon name="arrow" size={15} />
        </a>
      </div>
    </>
  );
}

function Metric({
  label,
  sub,
  value,
}: {
  label: string;
  sub?: string;
  value: number | string;
}) {
  return (
    <div>
      <dt>{label}</dt>
      <dd>
        {value}
        {sub && <small>{sub}</small>}
      </dd>
    </div>
  );
}

function MissingSelection({ subject }: { subject: string }) {
  return (
    <p className="inline-empty" role="status">
      That {subject} is no longer on the floor. It may have finished or moved out of the
      selected scope.
    </p>
  );
}
