import { useId } from "react";
import type {
  FactoryCarrier,
  FactoryDock,
  FactoryFloorModel,
  FactoryLane,
  FactoryLens,
  FactoryStation,
  FactoryWorker,
  FactoryWorkerPlacement,
} from "../factoryModel";
import {
  carrierLabel,
  dockLabel,
  laneLabel,
  machineStatusText,
  shortKind,
  stationLabel,
  workerLabel,
} from "../factoryLabels";
import type { FactorySelection } from "../factorySelection";
import { isSelected } from "../factorySelection";

/**
 * The plant itself: a top-down operations floor drawn from the real model.
 *
 * Everything is positioned from model coordinates, which are a pure function of
 * the daemon snapshot, so a refresh that changes nothing moves nothing. The
 * only motion is a CSS transition on a carrier's transform, which can only fire
 * when that run's stage actually changed, plus the blocked-stage alarm.
 *
 * Interaction is plain HTML buttons positioned over an SVG ground layer: the
 * SVG carries the scenery (bays, conveyors, docks, hazard markings) and the
 * buttons carry the semantics, so keyboard focus, labels and pressed state are
 * native rather than simulated on SVG shapes.
 */
export function FactoryFloor({
  animateTransitions,
  model,
  lens,
  onSelect,
  reducedMotion,
  selection,
}: {
  animateTransitions: boolean;
  model: FactoryFloorModel;
  lens: FactoryLens;
  onSelect: (selection: FactorySelection) => void;
  reducedMotion: boolean;
  selection: FactorySelection;
}) {
  const domId = useId().replaceAll(":", "");
  const tileId = `factory-tile-${domId}`;
  const hazardId = `factory-hazard-${domId}`;
  const arrowId = `factory-arrow-${domId}`;
  const workersByStation = new Map<string, FactoryWorker[]>();
  for (const worker of model.workers) {
    for (const placement of worker.placements) {
      if (!placement.stationId) {
        continue;
      }
      const list = workersByStation.get(placement.stationId) ?? [];
      list.push(worker);
      workersByStation.set(placement.stationId, list);
    }
  }

  return (
    <div
      aria-label="Factory floor"
      className="factory-floor"
      data-lens={lens}
      data-motion={reducedMotion ? "reduced" : "full"}
      role="group"
      style={{ height: `${model.height}px`, width: `${model.width}px` }}
    >
      <div
        className="factory-canvas"
        style={{ height: `${model.height}px`, width: `${model.width}px` }}
      >
        <svg
          aria-hidden="true"
          className="factory-ground"
          focusable="false"
          height={model.height}
          viewBox={`0 0 ${model.width} ${model.height}`}
          width={model.width}
        >
          <defs>
            <pattern
              height="26"
              id={tileId}
              patternUnits="userSpaceOnUse"
              width="26"
            >
              <path className="factory-tile-line" d="M26 0 H0 V26" fill="none" />
            </pattern>
            <pattern
              height="10"
              id={hazardId}
              patternTransform="rotate(45)"
              patternUnits="userSpaceOnUse"
              width="10"
            >
              <rect className="factory-hazard-base" height="10" width="10" x="0" y="0" />
              <rect className="factory-hazard-stripe" height="10" width="5" x="0" y="0" />
            </pattern>
            <marker
              id={arrowId}
              markerHeight="7"
              markerWidth="7"
              orient="auto"
              refX="5.4"
              refY="3.5"
            >
              <path className="factory-conveyor-head" d="M0 0 L7 3.5 L0 7 z" />
            </marker>
          </defs>
          <rect
            className="factory-slab"
            height={model.height}
            rx="18"
            width={model.width}
            x="0"
            y="0"
          />
          <rect
            fill={`url(#${tileId})`}
            height={model.height}
            opacity="0.9"
            width={model.width}
            x="0"
            y="0"
          />
          {model.lanes.map((lane) => (
            <LaneScenery
              animateTransitions={animateTransitions}
              arrowId={arrowId}
              hazardId={hazardId}
              key={lane.id}
              lane={lane}
            />
          ))}
          {model.commons.workerIds.length > 0 && (
            <>
              <rect
                className="factory-commons-pad"
                height={model.commons.height}
                rx="14"
                width={model.commons.width}
                x={model.commons.x}
                y={model.commons.y}
              />
              <text
                className="factory-pad-label"
                x={model.commons.x + 16}
                y={model.commons.y + 22}
              >
                Ready commons · idle goobers
              </text>
            </>
          )}
        </svg>

        {model.lanes.map((lane) => (
          <LaneShell
            key={lane.id}
            lane={lane}
            onSelect={onSelect}
            partial={model.runsTruncated}
            selected={isSelected(selection, { kind: "lane", id: lane.id })}
          />
        ))}

        {model.stations.map((station) => (
          <Station
            key={station.id}
            onSelect={onSelect}
            selected={isSelected(selection, { kind: "station", id: station.id })}
            station={station}
            workers={workersByStation.get(station.id) ?? []}
          />
        ))}

        {model.carriers.filter((carrier) => carrier.rendered).map((carrier) => (
          <Carrier
            animateTransitions={animateTransitions}
            carrier={carrier}
            key={carrier.runId}
            onSelect={onSelect}
            selected={isSelected(selection, { kind: "run", id: carrier.runId })}
          />
        ))}

        {model.workers.flatMap((worker) =>
          worker.placements.filter((placement) => placement.rendered).map((placement) => (
            <Worker
              key={placement.id}
              onSelect={onSelect}
              placement={placement}
              selected={isSelected(selection, { kind: "worker", id: worker.id })}
              worker={worker}
            />
          )),
        )}
        {model.commons.overflowWorkerCount > 0 && (
          <button
            aria-label={`${model.commons.overflowWorkerCount} additional ready goobers. Select the floor summary.`}
            className="factory-overflow factory-commons-overflow"
            onClick={() => onSelect({ kind: "overview" })}
            style={{
              left: `${model.commons.x + model.commons.width - 104}px`,
              top: `${model.commons.y + 14}px`,
            }}
            type="button"
          >
            +{model.commons.overflowWorkerCount} ready
          </button>
        )}
      </div>
    </div>
  );
}

function LaneScenery({
  animateTransitions,
  arrowId,
  hazardId,
  lane,
}: {
  animateTransitions: boolean;
  arrowId: string;
  hazardId: string;
  lane: FactoryLane;
}) {
  return (
    <g>
      <rect
        className="factory-bay"
        height={lane.height}
        rx="16"
        width={lane.width - 16}
        x="8"
        y={lane.y}
      />
      <rect
        className="factory-bay-header"
        height={26}
        rx="8"
        width={lane.width - 40}
        x="20"
        y={lane.y + 6}
      />
      <rect
        fill={`url(#${hazardId})`}
        height={lane.height - 20}
        opacity="0.75"
        width="6"
        x="10"
        y={lane.y + 10}
      />
      <rect
        className="factory-yard-pad"
        height={lane.yard.height}
        rx="10"
        width={lane.yard.width}
        x={lane.yard.x}
        y={lane.yard.y}
      />
      {[0, 1, 2, 3].map((slot) => (
        <rect
          className="factory-yard-slot"
          height="22"
          key={`slot-${slot}`}
          rx="4"
          width="28"
          x={lane.yard.x + 12 + (slot % 2) * 34}
          y={lane.yard.y + 14 + Math.floor(slot / 2) * 28}
        />
      ))}
      <path
        className="factory-floor-decal"
        d={`M ${lane.yard.x + lane.yard.width + 4} ${lane.yard.y + lane.yard.height / 2} h 12 m -5 -4 l 5 4 l -5 4`}
      />
      <text className="factory-pad-label" x={lane.yard.x + 10} y={lane.yard.y - 6}>
        Inbound
      </text>
      {lane.stations.map((station) => (
        <rect
          className="factory-apron"
          height={84}
          key={`apron-${station.id}`}
          rx="8"
          width={station.width}
          x={station.x}
          y={station.y + station.height + 4}
        />
      ))}
      {lane.conveyors.map((conveyor) => (
        <g key={conveyor.id}>
          <path className="factory-conveyor-bed" d={conveyor.path} />
          <path
            className="factory-conveyor-line"
            d={conveyor.path}
            data-active={animateTransitions && conveyor.active ? "true" : "false"}
            data-kind={conveyor.kind}
            markerEnd={`url(#${arrowId})`}
          />
          {(conveyor.branch || conveyor.outcome) && (
            <text
              className="factory-conveyor-label"
              textAnchor="middle"
              x={conveyor.labelX}
              y={conveyor.labelY}
            >
              {[conveyor.branch, conveyor.outcome].filter(Boolean).join(" · ")}
            </text>
          )}
        </g>
      ))}
      {lane.docks.map((dock) => (
        <Dock dock={dock} key={dock.id} />
      ))}
    </g>
  );
}

function Dock({ dock }: { dock: FactoryDock }) {
  return (
    <g>
      <rect
        className="factory-dock"
        data-terminal={dock.terminal}
        height={dock.height}
        rx="8"
        width={dock.width}
        x={dock.x}
        y={dock.y}
      />
      <text
        className="factory-dock-label"
        textAnchor="middle"
        x={dock.x + dock.width / 2}
        y={dock.y + dock.height / 2 + 4}
      >
        {dockLabel(dock.terminal)}
      </text>
    </g>
  );
}

function LaneShell({
  lane,
  onSelect,
  partial,
  selected,
}: {
  lane: FactoryLane;
  onSelect: (selection: FactorySelection) => void;
  partial: boolean;
  selected: boolean;
}) {
  const topologyUnread = lane.source === "observed";
  return (
    <>
      <div
        className="factory-lane-plaque-row"
        style={{ top: `${lane.y + 8}px`, width: `${lane.width - 16}px` }}
      >
        <button
        aria-label={laneLabel(lane, partial)}
        aria-pressed={selected}
        className="factory-lane-plaque"
        data-blocked={lane.blockedRuns > 0 ? "true" : "false"}
        onClick={() => onSelect({ kind: "lane", id: lane.id })}
        type="button"
        >
        <span className="factory-lane-title">{lane.displayName}</span>
        <span className="factory-lane-gaggle">{lane.gaggleDisplayName}</span>
        <span className="factory-lane-metrics">
          <span className="factory-lane-capacity">
            <span className="factory-lane-metric">
              {lane.activeRuns}{partial ? "+" : ""} active · workflow limit{" "}
              {lane.limit ?? "unknown"}
            </span>
            <span className="factory-lane-gauge" aria-hidden="true">
              <span
                style={{
                  width: `${
                    lane.saturation === undefined
                      ? 0
                      : Math.min(100, Math.round(lane.saturation * 100))
                  }%`,
                }}
              />
            </span>
          </span>
          {lane.unreadRuns > 0 && (
            <span className="factory-lane-badge">{lane.unreadRuns} signal unread</span>
          )}
          {topologyUnread && (
            <span className="factory-lane-badge">
              {lane.stations.length === 0
                ? "topology not read in this batch"
                : "observed topology · order unknown"}
            </span>
          )}
          {lane.stageCount !== lane.stations.length && (
            <span className="factory-lane-badge">
              {lane.stageCount} configured, {lane.stations.length} drawn
            </span>
          )}
        </span>
        </button>
      </div>
      {lane.yard.overflowRunCount > 0 && (
        <button
        aria-label={`${lane.yard.overflowRunCount} additional runs waiting at inbound for ${lane.displayName}. Select the workflow line.`}
        className="factory-overflow factory-yard-overflow"
        onClick={() => onSelect({ kind: "lane", id: lane.id })}
        style={{
          left: `${lane.yard.x + 8}px`,
          top: `${lane.yard.y + 68}px`,
        }}
        type="button"
        >
        +{lane.yard.overflowRunCount} queued
        </button>
      )}
    </>
  );
}


function Station({
  onSelect,
  selected,
  station,
  workers,
}: {
  onSelect: (selection: FactorySelection) => void;
  selected: boolean;
  station: FactoryStation;
  workers: readonly FactoryWorker[];
}) {
  const alarm = station.alarm ?? "off";
  return (
    <>
      {station.alarm && (
        <span
          aria-hidden="true"
          className="factory-alarm-wash"
          data-alarm={station.alarm}
          style={{
            height: `${station.height + 56}px`,
            left: `${station.x - 14}px`,
            top: `${station.y - 14}px`,
            width: `${station.width + 28}px`,
          }}
        />
      )}
      <button
        aria-label={stationLabel(station, workers)}
        aria-pressed={selected}
        className="factory-station"
        data-alarm={alarm}
        data-kind={station.kind}
        data-source={station.source}
        data-status={station.status}
        onClick={() => onSelect({ kind: "station", id: station.id })}
        style={{
          height: `${station.height}px`,
          left: `${station.x}px`,
          top: `${station.y}px`,
          width: `${station.width}px`,
        }}
        type="button"
      >
        <span aria-hidden="true" className="factory-station-roof">
          {station.alarm && (
            <span className="factory-alarm-beacon" data-alarm={station.alarm}>
              <span className="factory-alarm-lamp" />
            </span>
          )}
          <span className="factory-station-vent" />
          <span className="factory-station-vent" />
          <span className="factory-station-vent" />
        </span>
        <span className="factory-station-body">
          <span className="factory-station-kind">{shortKind(station)}</span>
          <span className="factory-station-name">{station.stageId}</span>
          <span className="factory-station-gauge">
            <span className="factory-gauge-readout">WIP {station.wip}</span>
            <span className="factory-machine-status">{machineStatusText(station)}</span>
          </span>
        </span>
        <span aria-hidden="true" className="factory-status-light" />
        {station.alarm === "blocked" && (
          <span className="sr-only">Alarm: stage blocked</span>
        )}
        {station.alarm === "hold" && (
          <span className="sr-only">Alarm: human gate hold</span>
        )}
      </button>
      {station.overflowRunCount > 0 && (
        <button
          aria-label={`${station.overflowRunCount} additional runs at stage ${station.stageId}. Select the stage to inspect all runs.`}
          className="factory-overflow factory-carrier-overflow"
          onClick={() => onSelect({ kind: "station", id: station.id })}
          style={{
            left: `${station.x + 8}px`,
            top: `${station.y + station.height + 64}px`,
          }}
          type="button"
        >
          +{station.overflowRunCount} more
        </button>
      )}
      {station.workerOverflowCount > 0 && (
        <button
          aria-label={`${station.workerOverflowCount} additional goobers at stage ${station.stageId}. Select the stage to inspect staffing.`}
          className="factory-overflow factory-worker-overflow"
          onClick={() => onSelect({ kind: "station", id: station.id })}
          style={{
            left: `${station.x + 108}px`,
            top: `${station.y + station.height + 52}px`,
          }}
          type="button"
        >
          +{station.workerOverflowCount} staff
        </button>
      )}
    </>
  );
}



function Carrier({
  animateTransitions,
  carrier,
  onSelect,
  selected,
}: {
  animateTransitions: boolean;
  carrier: FactoryCarrier;
  onSelect: (selection: FactorySelection) => void;
  selected: boolean;
}) {
  const moved =
    animateTransitions && carrier.transition?.kind === "stage-change";
  return (
    <button
      aria-label={carrierLabel(carrier)}
      aria-pressed={selected}
      className={
        moved ? "factory-carrier is-transitioning" : "factory-carrier"
      }
      data-moved={moved ? "true" : "false"}
      data-state={carrier.state}
      onClick={() => onSelect({ kind: "run", id: carrier.runId })}
      style={{ transform: `translate(${carrier.x}px, ${carrier.y}px)` }}
      type="button"
    >
      <span aria-hidden="true" className="factory-carrier-lid" />
      <span aria-hidden="true" className="factory-carrier-code">
        …{carrier.runId.slice(-4)}
      </span>
    </button>
  );
}


function Worker({
  onSelect,
  placement,
  selected,
  worker,
}: {
  onSelect: (selection: FactorySelection) => void;
  placement: FactoryWorkerPlacement;
  selected: boolean;
  worker: FactoryWorker;
}) {
  return (
    <button
      aria-label={workerLabel(worker, placement)}
      aria-pressed={selected}
      className="factory-worker"
      data-active={placement.active ? "true" : "false"}
      onClick={() => onSelect({ kind: "worker", id: worker.id })}
      style={{ left: `${placement.x}px`, top: `${placement.y}px` }}
      type="button"
    >
      <span aria-hidden="true" className="factory-worker-head" />
      <span aria-hidden="true" className="factory-worker-body" />
      <span aria-hidden="true" className="factory-worker-tag">
        {worker.name.slice(0, 3)}
      </span>
    </button>
  );
}
