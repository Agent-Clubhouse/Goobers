import { useId, useMemo } from "react";
import type {
  FactoryCarrier,
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
import {
  buildFactoryPlant,
  type PlantCrate,
  type PlantDistrict,
  type PlantDock,
  type PlantLabel,
  type PlantMachine,
  type PlantSolid,
  type PlantStaff,
} from "../factoryPlant";
import type { FactorySelection } from "../factorySelection";
import { isSelected } from "../factorySelection";

/**
 * The plant layout: the same floor model seen as one isometric hall.
 *
 * Every district is a configured workflow, every building is a declared stage
 * drawn as the machine its kind implies, every belt is a real graph edge, every
 * crate is an active run standing at the stage the daemon reports, and every
 * figure is a goober at a stage it owns while that stage holds work. Geometry
 * comes from `buildFactoryPlant`, which is pure, so a refresh that changes
 * nothing moves nothing.
 *
 * The scenery is one SVG ground layer and the semantics are ordinary HTML
 * buttons on top, exactly as in the line layout, so focus, names and pressed
 * state are native. Buttons are clipped to their own silhouette because an
 * isometric scene overlaps rectangles everywhere and a near machine must not
 * swallow a click meant for the one behind it.
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
  const scene = useMemo(() => buildFactoryPlant(model), [model]);
  const domId = useId().replaceAll(":", "");
  const floorId = `plant-floor-${domId}`;
  const hazardId = `plant-hazard-${domId}`;
  const wallId = `plant-wall-${domId}`;
  const blockedWashId = `plant-wash-blocked-${domId}`;
  const heldWashId = `plant-wash-hold-${domId}`;
  const arrowId = `plant-arrow-${domId}`;

  const lanesById = new Map(model.lanes.map((lane) => [lane.id, lane]));
  const stationsById = new Map(model.stations.map((station) => [station.id, station]));
  const carriersById = new Map(model.carriers.map((carrier) => [carrier.runId, carrier]));
  const workersById = new Map(model.workers.map((worker) => [worker.id, worker]));
  const placementsById = new Map(
    model.workers.flatMap((worker) =>
      worker.placements.map(
        (placement) => [placement.id, placement] as [string, FactoryWorkerPlacement],
      ),
    ),
  );
  const workersByStation = new Map<string, FactoryWorker[]>();
  for (const worker of model.workers) {
    for (const stationId of worker.activeStationIds) {
      const list = workersByStation.get(stationId) ?? [];
      list.push(worker);
      workersByStation.set(stationId, list);
    }
  }

  const selectedMachine = scene.machines.find((machine) =>
    isSelected(selection, { kind: "station", id: machine.id }),
  );

  return (
    <div
      aria-label="Factory plant"
      className="factory-plant"
      data-lens={lens}
      data-motion={reducedMotion ? "reduced" : "full"}
      data-responsive-layout="scroll-under-1100"
      role="group"
    >
      <div
        className="factory-plant-scene"
        style={{ height: `${scene.height}px`, width: `${scene.width}px` }}
      >
        <svg
          aria-hidden="true"
          className="factory-plant-ground"
          focusable="false"
          height={scene.height}
          viewBox={`0 0 ${scene.width} ${scene.height}`}
          width={scene.width}
        >
          <defs>
            <linearGradient id={floorId} x1="0" x2="0.4" y1="0" y2="1">
              <stop className="plant-floor-stop-near" offset="0" />
              <stop className="plant-floor-stop-far" offset="1" />
            </linearGradient>
            <linearGradient id={wallId} x1="0" x2="0" y1="0" y2="1">
              <stop className="plant-wall-stop-top" offset="0" />
              <stop className="plant-wall-stop-base" offset="1" />
            </linearGradient>
            <pattern
              height="9"
              id={hazardId}
              patternTransform="rotate(28)"
              patternUnits="userSpaceOnUse"
              width="9"
            >
              <rect className="plant-hazard-base" height="9" width="9" x="0" y="0" />
              <rect className="plant-hazard-stripe" height="9" width="4.5" x="0" y="0" />
            </pattern>
            <radialGradient id={blockedWashId}>
              <stop className="plant-wash-blocked-core" offset="0" />
              <stop className="plant-wash-edge" offset="1" />
            </radialGradient>
            <radialGradient id={heldWashId}>
              <stop className="plant-wash-hold-core" offset="0" />
              <stop className="plant-wash-edge" offset="1" />
            </radialGradient>
            <marker
              id={arrowId}
              markerHeight="6"
              markerWidth="6"
              orient="auto"
              refX="4.6"
              refY="3"
            >
              <path className="plant-belt-head" d="M0 0 L6 3 L0 6 z" />
            </marker>
          </defs>

          <polygon className="plant-floor" fill={`url(#${floorId})`} points={scene.hall.floor} />
          {scene.hall.grid.map((line, index) => (
            <path className="plant-grid" d={line} key={`grid-${index}`} />
          ))}
          {scene.hall.walls.map((wall) => (
            <g key={wall.id}>
              <polygon className="plant-wall" fill={`url(#${wallId})`} points={wall.face} />
              <polygon className="plant-wall-cap" points={wall.cap} />
              {wall.windows.map((pane, index) => (
                <polygon className="plant-window" key={`${wall.id}-pane-${index}`} points={pane} />
              ))}
            </g>
          ))}
          <path className="plant-rail" d={scene.hall.rail} />
          {scene.hall.railPosts.map((post, index) => (
            <path className="plant-rail-post" d={post} key={`post-${index}`} />
          ))}
          <polygon className="plant-aisle" points={scene.hall.aisle} />
          {scene.hall.aisleMarks.map((mark, index) => (
            <path className="plant-aisle-mark" d={mark} key={`aisle-${index}`} />
          ))}

          {scene.districts.map((district) => (
            <DistrictGround
              district={district}
              hazardId={hazardId}
              key={district.id}
            />
          ))}

          {scene.machines.map((machine) => (
            <g key={`ground-${machine.id}`}>
              {machine.alarm && (
                <polygon
                  className="plant-alarm-wash"
                  fill={`url(#${machine.alarm === "blocked" ? blockedWashId : heldWashId})`}
                  points={machine.wash}
                />
              )}
              <polygon className="plant-shadow" points={machine.shadow} />
            </g>
          ))}
          {selectedMachine && (
            <polygon className="plant-selection-ring" points={selectedMachine.shadow} />
          )}
          {scene.docks.map((dock) => (
            <polygon className="plant-shadow" key={`ground-${dock.id}`} points={dock.shadow} />
          ))}

          {scene.tracks.map((track) => (
            <g key={track.id}>
              <path className="plant-belt-bed" d={track.bed} />
              {track.rails.map((rail, index) => (
                <path className="plant-belt-rail" d={rail} key={`${track.id}-rail-${index}`} />
              ))}
              <path
                className="plant-belt-line"
                d={track.bed}
                data-active={animateTransitions && track.active ? "true" : "false"}
                data-kind={track.kind}
                markerEnd={`url(#${arrowId})`}
              />
            </g>
          ))}

          {scene.commons && (
            <polygon className="plant-commons-pad" points={scene.commons.pad} />
          )}
        </svg>

        {scene.districts.map((district) => {
          const lane = lanesById.get(district.id);
          return lane ? (
            <DistrictSign
              district={district}
              key={`sign-${district.id}`}
              lane={lane}
              onSelect={onSelect}
              partial={model.runsTruncated}
              selected={isSelected(selection, { kind: "lane", id: lane.id })}
            />
          ) : null;
        })}

        {scene.docks.map((dock) => (
          <Dock dock={dock} key={dock.id} />
        ))}

        {scene.machines.map((machine) => {
          const station = stationsById.get(machine.id);
          return station ? (
            <Machine
              key={machine.id}
              machine={machine}
              onSelect={onSelect}
              selected={isSelected(selection, { kind: "station", id: machine.id })}
              station={station}
              workers={workersByStation.get(machine.id) ?? []}
            />
          ) : null;
        })}

        {scene.crates.map((crate) => {
          const carrier = carriersById.get(crate.id);
          return carrier ? (
            <Crate
              carrier={carrier}
              crate={crate}
              animateTransitions={animateTransitions}
              key={crate.id}
              onSelect={onSelect}
              selected={isSelected(selection, { kind: "run", id: crate.id })}
            />
          ) : null;
        })}

        {scene.staff.map((figure) => {
          const worker = workersById.get(figure.workerId);
          const placement = placementsById.get(figure.id);
          return worker && placement ? (
            <Staff
              figure={figure}
              key={figure.id}
              onSelect={onSelect}
              placement={placement}
              selected={isSelected(selection, { kind: "worker", id: worker.id })}
              worker={worker}
            />
          ) : null;
        })}

        {scene.districts.map((district) => {
          const lane = lanesById.get(district.id);
          if (!lane || lane.yard.overflowRunCount === 0) {
            return null;
          }
          return (
            <button
              aria-label={`${lane.yard.overflowRunCount} additional runs waiting at inbound for ${lane.displayName}. Select the workflow line.`}
              className="factory-overflow factory-plant-overflow"
              key={`yard-overflow-${district.id}`}
              onClick={() => onSelect({ kind: "lane", id: lane.id })}
              style={{
                left: `${district.yard.overflowAnchor.x}px`,
                top: `${district.yard.overflowAnchor.y}px`,
              }}
              type="button"
            >
              +{lane.yard.overflowRunCount} queued
            </button>
          );
        })}

        {scene.machines.flatMap((machine) => {
          const station = stationsById.get(machine.id);
          if (!station) {
            return [];
          }
          const buttons = [];
          if (station.overflowRunCount > 0) {
            buttons.push(
              <button
                aria-label={`${station.overflowRunCount} additional runs at stage ${station.stageId}. Select the stage to inspect all runs.`}
                className="factory-overflow factory-plant-overflow"
                key={`run-overflow-${machine.id}`}
                onClick={() => onSelect({ kind: "station", id: machine.id })}
                style={{
                  left: `${machine.apron.x}px`,
                  top: `${machine.apron.y}px`,
                }}
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
                key={`staff-overflow-${machine.id}`}
                onClick={() => onSelect({ kind: "station", id: machine.id })}
                style={{
                  left: `${machine.left + machine.width - 12}px`,
                  top: `${machine.top + machine.height * 0.52}px`,
                }}
                type="button"
              >
                +{station.workerOverflowCount} staff
              </button>,
            );
          }
          return buttons;
        })}

        {scene.commons && model.commons.overflowWorkerCount > 0 && (
          <button
            aria-label={`${model.commons.overflowWorkerCount} additional ready goobers. Select the floor summary.`}
            className="factory-overflow factory-plant-overflow"
            onClick={() => onSelect({ kind: "overview" })}
            style={{
              left: `${scene.commons.overflowAnchor.x}px`,
              top: `${scene.commons.overflowAnchor.y}px`,
            }}
            type="button"
          >
            +{model.commons.overflowWorkerCount} ready
          </button>
        )}

        <AnnotationLayer scene={scene} />
      </div>
    </div>
  );
}

function DistrictGround({
  district,
  hazardId,
}: {
  district: PlantDistrict;
  hazardId: string;
}) {
  return (
    <g data-source={district.source}>
      <polygon className="plant-plot" points={district.plot} />
      <polygon className="plant-plot-kerb" points={district.kerb} />
      <polygon fill={`url(#${hazardId})`} opacity="0.7" points={district.hazard} />
      <text
        className="plant-floor-paint"
        transform={district.floorLabel.transform}
        x="0"
        y="0"
      >
        {district.floorLabel.text}
      </text>
      <polygon className="plant-yard-pad" points={district.yard.pad} />
      {district.yard.slots.map((slot, index) => (
        <polygon className="plant-yard-slot" key={`${district.id}-slot-${index}`} points={slot} />
      ))}
      {district.returnTracks.map((returnTrack, index) => (
        <path
          className="plant-return-track"
          d={returnTrack}
          key={`${district.id}-return-${index}`}
        />
      ))}
      {district.emptyBay && (
        <polygon className="plant-empty-bay" points={district.emptyBay.pad} />
      )}
    </g>
  );
}

function AnnotationLayer({
  scene,
}: {
  scene: ReturnType<typeof buildFactoryPlant>;
}) {
  const labels: { id: string; className: string; label: PlantLabel }[] = [
    ...scene.districts.map((district) => ({
      id: `${district.id}-inbound`,
      className: "plant-pad-label",
      label: district.yard.label,
    })),
    ...scene.districts.flatMap((district) =>
      district.emptyBay
        ? [
            {
              id: `${district.id}-empty`,
              className: "plant-pad-label",
              label: district.emptyBay.label,
            },
          ]
        : [],
    ),
    ...scene.tracks.flatMap((track) =>
      track.label
        ? [
            {
              id: `${track.id}-outcome`,
              className: "plant-belt-label",
              label: track.label,
            },
          ]
        : [],
    ),
    ...(scene.commons
      ? [
          {
            id: "commons-label",
            className: "plant-pad-label",
            label: scene.commons.label,
          },
        ]
      : []),
  ];
  return (
    <svg
      aria-hidden="true"
      className="factory-plant-annotations"
      focusable="false"
      height={scene.height}
      viewBox={`0 0 ${scene.width} ${scene.height}`}
      width={scene.width}
    >
      {labels.map(({ className, id, label }) => (
        <g className="plant-annotation" key={id}>
          <rect
            className="plant-annotation-plate"
            height={label.height}
            rx="5"
            width={label.width}
            x={label.x - label.width / 2}
            y={label.y - label.height / 2}
          />
          <text
            className={className}
            textAnchor="middle"
            x={label.x}
            y={label.y + 3}
          >
            {label.text}
          </text>
        </g>
      ))}
    </svg>
  );
}

function DistrictSign({
  district,
  lane,
  onSelect,
  partial,
  selected,
}: {
  district: PlantDistrict;
  lane: FactoryLane;
  onSelect: (selection: FactorySelection) => void;
  partial: boolean;
  selected: boolean;
}) {
  const saturation =
    lane.saturation === undefined ? 0 : Math.min(100, Math.round(lane.saturation * 100));
  return (
    <button
      aria-label={laneLabel(lane, partial)}
      aria-pressed={selected}
      className="factory-plant-sign"
      data-blocked={lane.blockedRuns > 0 ? "true" : "false"}
      onClick={() => onSelect({ kind: "lane", id: lane.id })}
      style={{ left: `${district.sign.x}px`, top: `${district.sign.y}px` }}
      type="button"
    >
      <span aria-hidden="true" className="factory-plant-sign-mast" />
      <span className="factory-plant-sign-board">
        <span className="factory-plant-sign-title">{lane.displayName}</span>
        <span className="factory-plant-sign-gaggle">{lane.gaggleDisplayName}</span>
        <span className="factory-plant-sign-readout">
          {lane.activeRuns}
          {partial ? "+" : ""} active · limit {lane.limit ?? "unknown"}
        </span>
        <span aria-hidden="true" className="factory-plant-sign-gauge" data-known={lane.limit === undefined ? "false" : "true"}>
          <span style={{ width: `${saturation}%` }} />
        </span>
        {lane.unreadRuns > 0 && (
          <span className="factory-plant-sign-badge">{lane.unreadRuns} signal unread</span>
        )}
        {lane.source === "observed" && (
          <span className="factory-plant-sign-badge">
            {lane.stations.length === 0
              ? "topology not read in this batch"
              : "observed topology · order unknown"}
          </span>
        )}
        {lane.stageCount !== lane.stations.length && (
          <span className="factory-plant-sign-badge">
            {lane.stageCount} configured, {lane.stations.length} drawn
          </span>
        )}
      </span>
    </button>
  );
}

function Machine({
  machine,
  onSelect,
  selected,
  station,
  workers,
}: {
  machine: PlantMachine;
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
    <button
      aria-label={stationLabel(station, workers)}
      aria-pressed={selected}
      className="factory-plant-machine"
      data-alarm={machine.alarm ?? "off"}
      data-kind={machine.kind}
      data-source={machine.source}
      data-start={machine.isStart ? "true" : "false"}
      data-status={machine.status}
      onClick={() => onSelect({ kind: "station", id: machine.id })}
      style={{
        clipPath: `path("${machine.clip}")`,
        height: `${machine.height}px`,
        left: `${machine.left}px`,
        top: `${machine.top}px`,
        width: `${machine.width}px`,
        zIndex: depthOrder(machine.depth, 1),
      }}
      type="button"
    >
      <Solids height={machine.height} solids={machine.solids} width={machine.width} />
      <span
        aria-hidden="true"
        className="factory-plant-lamp"
        style={{ left: `${machine.lamp.x}px`, top: `${machine.lamp.y}px` }}
      />
      {machine.alarm && (
        <span
          aria-hidden="true"
          className="factory-plant-beacon"
          data-alarm={machine.alarm}
          style={{
            height: `${machine.beaconSize}px`,
            left: `${machine.beacon.x}px`,
            top: `${machine.beacon.y}px`,
            width: `${machine.beaconSize * 0.5}px`,
          }}
        >
          <span className="factory-plant-beacon-lamp" />
        </span>
      )}
      <span
        aria-hidden="true"
        className="factory-plant-placard"
        style={{
          height: `${machine.placard.height}px`,
          left: `${machine.placard.x}px`,
          top: `${machine.placard.y}px`,
          width: `${machine.placard.width}px`,
        }}
      >
        <span className="factory-plant-placard-head">
          <span className="factory-plant-placard-kind">{shortKind(station)}</span>
          <span className="factory-plant-placard-status">{machineStatusText(station)}</span>
        </span>
        <span className="factory-plant-placard-name">{station.stageId}</span>
        <span className="factory-plant-placard-foot">
          <span className="factory-plant-placard-wip">WIP {station.wip}</span>
          <span className="factory-plant-placard-gauge" data-known={fill === undefined ? "false" : "true"}>
            <span style={{ width: `${fill ?? 0}%` }} />
          </span>
        </span>
      </span>
      {machine.alarm === "blocked" && <span className="sr-only">Alarm: stage blocked</span>}
      {machine.alarm === "hold" && <span className="sr-only">Alarm: human gate hold</span>}
    </button>
  );
}

function Solids({
  height,
  solids,
  width,
}: {
  height: number;
  solids: readonly PlantSolid[];
  width: number;
}) {
  return (
    <svg
      aria-hidden="true"
      className="factory-plant-art"
      focusable="false"
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      width={width}
    >
      {solids.map((solid, index) => (
        <g data-role={solid.role} key={`${solid.role}-${index}`}>
          <polygon className="plant-face plant-face-front" points={solid.front} />
          <polygon className="plant-face plant-face-right" points={solid.right} />
          <polygon className="plant-face plant-face-top" points={solid.top} />
        </g>
      ))}
    </svg>
  );
}

function Dock({ dock }: { dock: PlantDock }) {
  return (
    <div
      aria-hidden="true"
      className="factory-plant-dock"
      data-terminal={dock.terminal}
      style={{
        height: `${dock.height}px`,
        left: `${dock.left}px`,
        top: `${dock.top}px`,
        width: `${dock.width}px`,
        zIndex: depthOrder(dock.depth, 0),
      }}
    >
      <Solids height={dock.height} solids={dock.solids} width={dock.width} />
      <span
        className="factory-plant-dock-label"
        style={{ left: `${dock.label.x}px`, top: `${dock.label.y}px` }}
      >
        {dockLabel(dock.terminal)}
      </span>
    </div>
  );
}

function Crate({
  animateTransitions,
  carrier,
  crate,
  onSelect,
  selected,
}: {
  animateTransitions: boolean;
  carrier: FactoryCarrier;
  crate: PlantCrate;
  onSelect: (selection: FactorySelection) => void;
  selected: boolean;
}) {
  const moved = animateTransitions && crate.moved;
  return (
    <button
      aria-label={carrierLabel(carrier)}
      aria-pressed={selected}
      className={moved ? "factory-plant-crate is-transitioning" : "factory-plant-crate"}
      data-moved={moved ? "true" : "false"}
      data-state={carrier.state}
      onClick={() => onSelect({ kind: "run", id: crate.id })}
      style={{
        clipPath: `path("${crate.clip}")`,
        height: `${crate.size}px`,
        transform: `translate(${crate.x}px, ${crate.y}px)`,
        width: `${crate.size}px`,
        zIndex: depthOrder(crate.depth, 3),
      }}
      type="button"
    >
      <svg
        aria-hidden="true"
        className="factory-plant-art"
        focusable="false"
        height={crate.size}
        viewBox={`0 0 ${crate.size} ${crate.size}`}
        width={crate.size}
      >
        <polygon className="plant-crate-face plant-crate-front" points={crate.front} />
        <polygon className="plant-crate-face plant-crate-right" points={crate.right} />
        <polygon className="plant-crate-face plant-crate-top" points={crate.top} />
        <polygon className="plant-crate-halo" points={crate.top} />
      </svg>
    </button>
  );
}

function Staff({
  figure,
  onSelect,
  placement,
  selected,
  worker,
}: {
  figure: PlantStaff;
  onSelect: (selection: FactorySelection) => void;
  placement: FactoryWorkerPlacement;
  selected: boolean;
  worker: FactoryWorker;
}) {
  return (
    <button
      aria-label={workerLabel(worker, placement)}
      aria-pressed={selected}
      className="factory-plant-staff"
      data-active={figure.active ? "true" : "false"}
      onClick={() => onSelect({ kind: "worker", id: worker.id })}
      style={{
        height: `${figure.size * 2.1}px`,
        left: `${figure.x}px`,
        top: `${figure.y}px`,
        width: `${figure.size}px`,
        zIndex: depthOrder(figure.depth, 3),
      }}
      type="button"
    >
      <span aria-hidden="true" className="factory-plant-staff-head" />
      <span aria-hidden="true" className="factory-plant-staff-body" />
      <span aria-hidden="true" className="factory-plant-staff-tag">
        {worker.name.slice(0, 3)}
      </span>
    </button>
  );
}

/** Painter order for absolutely positioned plant objects. */
function depthOrder(depth: number, bias: number): number {
  return Math.round(depth * 10) * 4 + bias;
}
