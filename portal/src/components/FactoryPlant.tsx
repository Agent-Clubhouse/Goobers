import { lazy, Suspense, useMemo, type CSSProperties } from "react";
import type {
  FactoryCarrier,
  FactoryFloorModel,
  FactoryLane,
  FactoryLens,
  FactoryStation,
  FactoryWorker,
  FactoryWorkerPlacement,
} from "../factoryModel";
import { carrierIsWorking } from "../factoryModel";
import {
  carrierLabel,
  laneLabel,
  machineStatusText,
  shortKind,
  stationLabel,
  workerLabel,
} from "../factoryLabels";
import {
  buildClassicPlant,
  CLASSIC_PLANT_HEIGHT,
  CLASSIC_PLANT_WIDTH,
  type ClassicPoint,
} from "../factoryClassicPlant";
import type { FactorySelection } from "../factorySelection";
import { isSelected } from "../factorySelection";
import { projectedPointStyle } from "../factoryWebGL";

const FactoryWebGLScene = lazy(async () => {
  const module = await import("./FactoryWebGLScene");
  return { default: module.FactoryWebGLScene };
});

/**
 * The boss's-window plant view.
 *
 * WebGL draws the operating hall when the browser supports it; the approved
 * factory illustration stays mounted underneath as the automatic fallback.
 * Semantic controls and live state still come from the same model as Lines.
 * The whole 1450 by 950 scene scales as one unit, so the plant always fits
 * instead of turning into a scrollable map.
 */
export function FactoryPlant({
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
  const scene = useMemo(() => buildClassicPlant(model), [model]);
  const working = model.carriers.some(carrierIsWorking);
  const zones = useMemo(() => buildZoneSummaries(model), [model]);
  const stationsById = new Map(model.stations.map((station) => [station.id, station]));
  const workersById = new Map(model.workers.map((worker) => [worker.id, worker]));
  const workersByStation = new Map<string, FactoryWorker[]>();
  for (const worker of model.workers) {
    for (const stationId of worker.activeStationIds) {
      const workers = workersByStation.get(stationId) ?? [];
      workers.push(worker);
      workersByStation.set(stationId, workers);
    }
  }

  return (
    <div
      aria-label="Factory plant"
      className="factory-plant factory-plant-classic"
      data-lens={lens}
      data-motion={reducedMotion ? "reduced" : "full"}
      data-responsive-layout="fit"
      data-working={working ? "true" : "false"}
      role="group"
      style={{ height: `${CLASSIC_PLANT_HEIGHT}px`, width: `${CLASSIC_PLANT_WIDTH}px` }}
    >
      <div className="factory-plant-scene">
        <Suspense fallback={<FactoryPlantFallback />}>
          <FactoryWebGLScene
            lens={lens}
            model={model}
            reducedMotion={reducedMotion}
            scene={scene}
          />
        </Suspense>

        <ZoneCards zones={zones} />

        {scene.lanes.map(({ lane, sign }) => (
          <LaneSign
            key={lane.id}
            lane={lane}
            onSelect={onSelect}
            partial={model.runsTruncated}
            point={sign}
            selected={isSelected(selection, { kind: "lane", id: lane.id })}
          />
        ))}

        {scene.stations.map(({ station, machine }) => (
          <StationCard
            key={station.id}
            machine={machine}
            onSelect={onSelect}
            selected={isSelected(selection, { kind: "station", id: station.id })}
            station={station}
            workers={workersByStation.get(station.id) ?? []}
          />
        ))}

        {scene.carriers.map(({ carrier, point }) => (
          <Carrier
            animateTransitions={animateTransitions}
            carrier={carrier}
            key={carrier.runId}
            onSelect={onSelect}
            point={point}
            selected={isSelected(selection, { kind: "run", id: carrier.runId })}
          />
        ))}

        {scene.workers.map(({ placement, point, workerId }) => {
          const worker = workersById.get(workerId);
          return worker ? (
            <Worker
              key={placement.id}
              onSelect={onSelect}
              placement={placement}
              point={point}
              selected={isSelected(selection, { kind: "worker", id: worker.id })}
              working={
                placement.stationId
                  ? stationsById.get(placement.stationId)?.status === "running"
                  : false
              }
              worker={worker}
            />
          ) : null;
        })}

        {model.lanes.flatMap((lane) => {
          const buttons = [];
          if (lane.yard.overflowRunCount > 0) {
            const inbound = scene.lanes.find((item) => item.lane.id === lane.id)?.inbound;
            if (inbound) {
              buttons.push(
                <button
                  aria-label={`${lane.yard.overflowRunCount} additional runs waiting at inbound for ${lane.displayName}. Select the workflow line.`}
                  className="factory-overflow factory-plant-overflow"
                  key={`${lane.id}-inbound-overflow`}
                  onClick={() => onSelect({ kind: "lane", id: lane.id })}
                  style={pointStyle({ x: inbound.x - 20, y: inbound.y + 58 })}
                  type="button"
                >
                  +{lane.yard.overflowRunCount} queued
                </button>,
              );
            }
          }
          for (const station of lane.stations) {
            const placement = scene.stations.find((item) => item.station.id === station.id);
            if (!placement) {
              continue;
            }
            if (station.overflowRunCount > 0) {
              buttons.push(
                <button
                  aria-label={`${station.overflowRunCount} additional runs at stage ${station.stageId}. Select the stage to inspect all runs.`}
                  className="factory-overflow factory-plant-overflow"
                  key={`${station.id}-run-overflow`}
                  onClick={() => onSelect({ kind: "station", id: station.id })}
                  style={pointStyle({
                    x: placement.machine.x - 18,
                    y: placement.machine.y + 62,
                  })}
                  type="button"
                >
                  +{station.overflowRunCount} more
                </button>,
              );
            }
            if (station.workerOverflowCount > 0) {
              buttons.push(
                <button
                  aria-label={`${station.workerOverflowCount} additional goobers at stage ${station.stageId}. Select the stage to inspect staffing.`}
                  className="factory-overflow factory-plant-overflow factory-plant-overflow-staff"
                  key={`${station.id}-staff-overflow`}
                  onClick={() => onSelect({ kind: "station", id: station.id })}
                  style={pointStyle({
                    x: placement.machine.x + 65,
                    y: placement.machine.y + 40,
                  })}
                  type="button"
                >
                  +{station.workerOverflowCount} staff
                </button>,
              );
            }
          }
          return buttons;
        })}

        {model.commons.overflowWorkerCount > 0 && (
          <button
            aria-label={`${model.commons.overflowWorkerCount} additional ready goobers. Select the floor summary.`}
            className="factory-overflow factory-plant-overflow"
            onClick={() => onSelect({ kind: "overview" })}
            style={pointStyle({ x: 835, y: 760 })}
            type="button"
          >
            +{model.commons.overflowWorkerCount} ready
          </button>
        )}

        <div aria-hidden="true" className="factory-plant-hud factory-plant-hud-top">
          <span>
            {model.scope.gaggle ?? "All gaggles"} · {model.counts.activeRuns}
            {model.runsTruncated ? "+" : ""} work orders
          </span>
          <b>
            {working
              ? "FACTORY WORKING"
              : model.counts.blockedStages > 0
                ? "ATTENTION REQUIRED"
                : model.counts.heldStages > 0
                  ? "HUMAN HOLD"
                  : model.counts.unreadRuns > 0
                    ? "SIGNALS INCOMPLETE"
                    : "PLANT READY"}
          </b>
        </div>
        <div aria-hidden="true" className="factory-plant-hud factory-plant-hud-bottom">
          <span><i data-tone="working" /> Working</span>
          <span><i data-tone="waiting" /> Waiting</span>
          <span><i data-tone="blocked" /> Blocked</span>
          <strong>{model.counts.goobers} goobers posted</strong>
        </div>
      </div>
    </div>
  );
}

function LaneSign({
  lane,
  onSelect,
  partial,
  point,
  selected,
}: {
  lane: FactoryLane;
  onSelect: (selection: FactorySelection) => void;
  partial: boolean;
  point: ClassicPoint;
  selected: boolean;
}) {
  return (
    <button
      aria-label={laneLabel(lane, partial)}
      aria-pressed={selected}
      className="factory-plant-sign"
      data-blocked={lane.blockedRuns > 0 ? "true" : "false"}
      onClick={() => onSelect({ kind: "lane", id: lane.id })}
      style={pointStyle(point)}
      type="button"
    >
      <span className="factory-plant-sign-title">{lane.displayName}</span>
      <span className="factory-plant-sign-gaggle">
        {lane.gaggleDisplayName} · {lane.activeRuns}{partial ? "+" : ""} active
      </span>
      <span className="factory-plant-sign-readout">
        {lane.gaggleDisplayName} · {lane.displayName}
      </span>
      {lane.source === "observed" && (
        <span className="factory-plant-sign-badge">
          {lane.stations.length === 0
            ? `${lane.stageCount} stages unread`
            : "observed topology · order unknown"}
        </span>
      )}
    </button>
  );
}

function StationCard({
  machine,
  onSelect,
  selected,
  station,
  workers,
}: {
  machine: ClassicPoint;
  onSelect: (selection: FactorySelection) => void;
  selected: boolean;
  station: FactoryStation;
  workers: readonly FactoryWorker[];
}) {
  const fill =
    station.saturation === undefined
      ? undefined
      : Math.min(100, Math.round(station.saturation * 100));
  return (
    <>
      <button
        aria-label={stationLabel(station, workers)}
        aria-pressed={selected}
        className="factory-plant-machine"
        data-alarm={station.alarm ?? "off"}
        data-kind={station.kind}
        data-source={station.source}
        data-start={station.isStart ? "true" : "false"}
        data-status={station.status}
        onClick={() => onSelect({ kind: "station", id: station.id })}
        style={projectedPointStyle(machine, 0.72)}
        type="button"
      >
        <span aria-hidden="true" className="factory-plant-machine-core">
          <i />
        </span>
        <span className="factory-plant-machine-tooltip">
          <span className="factory-plant-placard-head">
            <span className="factory-plant-placard-kind">{shortKind(station)}</span>
            <span className="factory-plant-placard-status">
              {station.alarm ? "ALARM" : machineStatusText(station)}
            </span>
          </span>
          <span className="factory-plant-placard-name">{station.stageId}</span>
          <span className="factory-plant-placard-foot">
            <span className="factory-plant-placard-wip">WIP {station.wip}</span>
            <span
              aria-hidden="true"
              className="factory-plant-placard-gauge"
              data-known={fill === undefined ? "false" : "true"}
            >
              <span style={{ width: `${fill ?? 0}%` }} />
            </span>
            <span>{station.limit === undefined ? "capacity ?" : `${station.wip} / ${station.limit}`}</span>
          </span>
        </span>
        {station.alarm && (
          <span aria-hidden="true" className="factory-plant-machine-alarm">
            {station.alarm === "blocked" ? "BLOCKED" : "HOLD"}
          </span>
        )}
        {station.alarm === "blocked" && <span className="sr-only">Alarm: stage blocked</span>}
        {station.alarm === "hold" && <span className="sr-only">Alarm: human gate hold</span>}
      </button>
    </>
  );
}

interface PlantZoneSummary {
  id: string;
  title: string;
  point: ClassicPoint;
  stages: number;
  wip: number;
  blocked: number;
  held: number;
  unknown: number;
}

const ZONE_DEFINITIONS: readonly Pick<PlantZoneSummary, "id" | "title" | "point">[] = [
  { id: "intake", title: "Intake Dock", point: { x: 70, y: 685 } },
  { id: "planning", title: "Planning Loft", point: { x: 70, y: 350 } },
  { id: "build", title: "Build Line", point: { x: 505, y: 805 } },
  { id: "quality", title: "Quality Tower", point: { x: 1110, y: 300 } },
  { id: "shipping", title: "Shipping Bay", point: { x: 1125, y: 735 } },
];

function buildZoneSummaries(model: FactoryFloorModel): PlantZoneSummary[] {
  const zones = ZONE_DEFINITIONS.map((zone) => ({
    ...zone,
    stages: 0,
    wip: 0,
    blocked: 0,
    held: 0,
    unknown: 0,
  }));
  for (const lane of model.lanes) {
    lane.stations.forEach((station, index) => {
      const zoneIndex =
        lane.stations.length <= 1
          ? 2
          : Math.round((index * (zones.length - 1)) / (lane.stations.length - 1));
      const zone = zones[zoneIndex];
      zone.stages += 1;
      zone.wip += station.wip;
      zone.blocked += station.status === "blocked" ? 1 : 0;
      zone.held += station.status === "held" ? 1 : 0;
      zone.unknown += station.status === "unknown" ? 1 : 0;
    });
  }
  return zones;
}

function ZoneCards({ zones }: { zones: readonly PlantZoneSummary[] }) {
  return (
    <>
      {zones.map((zone) => {
        const state =
          zone.blocked > 0
            ? "blocked"
            : zone.held > 0
              ? "held"
              : zone.unknown > 0
                ? "unknown"
                : zone.wip > 0
                  ? "running"
                  : "idle";
        return (
          <div
            aria-hidden="true"
            className="factory-plant-zone-card"
            data-state={state}
            key={zone.id}
            style={pointStyle(zone.point)}
          >
            <span className="factory-plant-zone-title">
              <i />
              {zone.title}
            </span>
            <strong>{zone.wip}</strong>
            <small>
              {zone.stages} stages
              {zone.blocked > 0
                ? ` · ${zone.blocked} blocked`
                : zone.held > 0
                  ? ` · ${zone.held} held`
                  : zone.unknown > 0
                    ? ` · ${zone.unknown} unread`
                    : zone.wip > 0
                      ? " · running"
                      : " · ready"}
            </small>
          </div>
        );
      })}
    </>
  );
}

function Carrier({
  animateTransitions,
  carrier,
  onSelect,
  point,
  selected,
}: {
  animateTransitions: boolean;
  carrier: FactoryCarrier;
  onSelect: (selection: FactorySelection) => void;
  point: ClassicPoint;
  selected: boolean;
}) {
  const moved = animateTransitions && carrier.transition?.kind === "stage-change";
  return (
    <button
      aria-label={carrierLabel(carrier)}
      aria-pressed={selected}
      className={moved ? "factory-plant-crate is-transitioning" : "factory-plant-crate"}
      data-moved={moved ? "true" : "false"}
      data-state={carrier.state}
      onClick={() => onSelect({ kind: "run", id: carrier.runId })}
      style={projectedPointStyle(point, 0.34)}
      type="button"
    >
      <span aria-hidden="true" className="plant-crate-top" />
      <span aria-hidden="true" className="plant-crate-front" />
      <span aria-hidden="true" className="plant-crate-right" />
      <span aria-hidden="true" className="plant-crate-halo" />
      <span className="sr-only">{carrier.runId}</span>
    </button>
  );
}

function Worker({
  onSelect,
  placement,
  point,
  selected,
  working,
  worker,
}: {
  onSelect: (selection: FactorySelection) => void;
  placement: FactoryWorkerPlacement;
  point: ClassicPoint;
  selected: boolean;
  working: boolean;
  worker: FactoryWorker;
}) {
  return (
    <button
      aria-label={workerLabel(worker, placement)}
      aria-pressed={selected}
      className="factory-plant-staff"
      data-active={placement.active ? "true" : "false"}
      data-working={working ? "true" : "false"}
      onClick={() => onSelect({ kind: "worker", id: worker.id })}
      style={projectedPointStyle(point, 0.48)}
      type="button"
    >
      <span aria-hidden="true" className="factory-plant-staff-head" />
      <span aria-hidden="true" className="factory-plant-staff-body" />
      <span aria-hidden="true" className="factory-plant-staff-tag">
        {worker.displayName.slice(0, 10)}
      </span>
    </button>
  );
}

function pointStyle(point: ClassicPoint): CSSProperties {
  return {
    left: `${(point.x / CLASSIC_PLANT_WIDTH) * 100}%`,
    top: `${(point.y / CLASSIC_PLANT_HEIGHT) * 100}%`,
  };
}

function FactoryPlantFallback() {
  return (
    <div
      aria-hidden="true"
      className="factory-plant-renderer"
      data-webgl="fallback"
    >
      <img
        alt=""
        className="factory-plant-backdrop"
        draggable="false"
        src="/factory-plant-base.png"
      />
    </div>
  );
}
