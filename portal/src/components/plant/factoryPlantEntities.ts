/**
 * Keyed entity specifications and reconciliation for the WebGL Plant.
 *
 * The runtime never rebuilds the scene for a new model, lens, or theme. It
 * derives one flat, deterministic specification list from the confirmed model
 * and reconciles it against the objects it already owns, keyed by the same
 * identity the semantic HTML layer uses: `station.id` for machines,
 * `conveyor.id` for declared tracks, `carrier.runId` for work-order crates,
 * and `placement.id` for posted goobers.
 *
 * Nothing here touches Three.js or the DOM, so both the specification rules and
 * the reconciliation contract are unit testable on their own.
 */

import {
  carrierIsWorking,
  type FactoryFloorModel,
  type FactoryLens,
  type FactoryStation,
} from "../../factoryModel";
import type { FactoryPlantLayout } from "../../factoryPlantLayout";
import {
  assessCarrierRisk,
  assessStationRisk,
  plantRiskEmphasis,
  type PlantRiskEmphasis,
  type PlantRiskLevel,
} from "../../plantRisk";
import type { PlantProjectionKind } from "../../plantProbeSink";

export type PlantEntityKind = "machine" | "conveyor" | "crate" | "worker";

/**
 * Scene tones, named for what the thing *is*.
 *
 * These map onto authored scene-palette entries rather than UI tokens, so a
 * crate is crate-coloured in both themes instead of inheriting whatever the
 * panel chrome happens to be.
 */
export type PlantTone =
  | "crate"
  | "crateBlocked"
  | "crateHeld"
  | "crateUnknown"
  | "machineBody"
  | "machineBodyAlt"
  | "machineTrim"
  | "statusBlocked"
  | "statusHeld"
  | "statusIdle"
  | "statusImpeded"
  | "statusRunning"
  | "statusUnknown"
  | "structure"
  | "worker"
  | "workerIdle";

export interface PlantWorldPoint {
  x: number;
  z: number;
}

export interface PlantTransferSpec {
  /** Where the crate stood before its confirmed stage change. */
  from: PlantWorldPoint;
  /**
   * Identity of the observed transition.
   *
   * A transfer plays once per confirmed move. Remounting, toggling layout, or
   * changing theme or lens re-delivers the same signature, which is how replay
   * is suppressed without inspecting React lifecycles.
   */
  signature: string;
}

export interface PlantProjectionSpec {
  id: string;
  /** The canonical layout overlay anchor this entity is measured against. */
  anchorId: string;
  kind: PlantProjectionKind;
  elevation: number;
}

export interface PlantRiskMarkerSpec {
  level: PlantRiskLevel;
  /** False for an unread signal; an unconfirmed reading never paints as a hazard. */
  confirmed: boolean;
}

export interface PlantEntitySpec {
  /** Semantic identity: station, run, or placement. */
  key: string;
  entity: PlantEntityKind;
  /**
   * Structural shape. A change here — and only a change here — replaces the
   * Object3D instead of updating it.
   */
  shape: string;
  position: PlantWorldPoint;
  /** Radians around Y, deterministic per key. */
  orientation: number;
  /** Deterministic animation phase derived from the key, never array order. */
  phase: number;
  tone: PlantTone;
  /** Confirmed operating state: emissive strength and motion truth. */
  active: boolean;
  /**
   * Risk-lens emphasis.
   *
   * `primary` is a confirmed hazard at full legibility, `unknown` is the
   * neutral incomplete treatment, and `context` is healthy floor kept visible
   * but desaturated. Outside the Risk lens everything is `primary`.
   */
  emphasis: PlantRiskEmphasis;
  /** Present only when this entity should carry a status marker. */
  marker?: PlantRiskMarkerSpec;
  projection?: PlantProjectionSpec;
  transfer?: PlantTransferSpec;
}

export interface PlantReconcileStats {
  created: number;
  replaced: number;
  updated: number;
  removed: number;
  live: number;
}

export interface PlantEntityRecord<TEntity> {
  spec: PlantEntitySpec;
  entity: TEntity;
}

export interface PlantReconcileHandlers<TEntity> {
  create: (spec: PlantEntitySpec) => TEntity;
  update: (entity: TEntity, spec: PlantEntitySpec, previous: PlantEntitySpec) => void;
  dispose: (entity: TEntity, spec: PlantEntitySpec) => void;
}

const MACHINE_ELEVATION = 0.72;
const CRATE_ELEVATION = 0.34;
const WORKER_ELEVATION = 0.48;

export function plantEntityRegistryKey(entity: PlantEntityKind, key: string): string {
  return `${entity}\u0000${key}`;
}

/**
 * Stable 32-bit hash of an entity key.
 *
 * Phases used to come from the array index, so inserting one run at the head of
 * the queue re-phased every crate behind it. Hashing the identity keeps each
 * entity's motion attached to the entity.
 */
export function plantKeyHash(key: string): number {
  let hash = 2166136261;
  for (let index = 0; index < key.length; index += 1) {
    hash ^= key.charCodeAt(index);
    hash = Math.imul(hash, 16777619);
  }
  return hash >>> 0;
}

export function plantPhase(key: string): number {
  return ((plantKeyHash(key) % 1000) / 1000) * Math.PI * 2;
}

export function buildPlantEntitySpecs({
  layout,
  lens,
  model,
}: {
  layout: FactoryPlantLayout;
  lens: FactoryLens;
  model: FactoryFloorModel;
}): PlantEntitySpec[] {
  const specs: PlantEntitySpec[] = [];
  const stationById = new Map(model.stations.map((station) => [station.id, station]));
  const anchorById = new Map(
    layout.overlayAnchors.map((anchor) => [anchor.id, anchor]),
  );
  const carrierById = new Map(
    model.carriers.map((carrier) => [carrier.runId, carrier]),
  );
  const placementById = new Map(
    model.workers.flatMap((worker) =>
      worker.placements.map((placement) => [placement.id, placement] as const),
    ),
  );

  for (const machine of layout.machines) {
    const station = stationById.get(machine.id);
    if (!station) {
      continue;
    }
    const position = {
      x: machine.transform.position.x,
      z: machine.transform.position.z,
    };
    const verdict = assessStationRisk(station);
    const emphasis = lens === "risk" ? plantRiskEmphasis(verdict) : "primary";
    const phase = plantPhase(station.id);
    specs.push({
      active: station.status === "running",
      emphasis,
      entity: "machine",
      key: station.id,
      ...(lens === "risk" && emphasis !== "context"
        ? { marker: { confirmed: verdict.confirmed, level: verdict.level } }
        : {}),
      orientation: machine.transform.rotationY,
      phase,
      position,
      projection: {
        anchorId: machine.overlayAnchorId,
        elevation:
          anchorById.get(machine.overlayAnchorId)?.position.y ?? MACHINE_ELEVATION,
        id: station.id,
        kind: "station",
      },
      shape: `machine:${machineShapeKind(station)}`,
      tone: machineTone(station),
    });
  }

  for (const track of layout.tracks) {
    const segment = track.segments[0];
    specs.push({
      active: track.active,
      emphasis: lens === "risk" && !track.active ? "context" : "primary",
      entity: "conveyor",
      key: track.id,
      orientation: segment?.transform.rotationY ?? 0,
      phase: plantPhase(track.id),
      position: {
        x: segment?.transform.position.x ?? track.points[0]?.x ?? 0,
        z: segment?.transform.position.z ?? track.points[0]?.z ?? 0,
      },
      shape: `conveyor:${track.kind}`,
      tone: track.active
        ? "statusRunning"
        : track.kind === "repass"
          ? "statusImpeded"
          : "structure",
    });
  }

  for (const anchor of layout.carriers) {
    if (!anchor.rendered) {
      continue;
    }
    const carrier = carrierById.get(anchor.id);
    if (!carrier) {
      continue;
    }
    const position = { x: anchor.position.x, z: anchor.position.z };
    const verdict = assessCarrierRisk(carrier);
    const emphasis = lens === "risk" ? plantRiskEmphasis(verdict) : "primary";
    specs.push({
      active: carrierIsWorking(carrier),
      emphasis,
      entity: "crate",
      key: carrier.runId,
      ...(lens === "risk" && emphasis !== "context"
        ? { marker: { confirmed: verdict.confirmed, level: verdict.level } }
        : {}),
      orientation: 0,
      phase: plantPhase(carrier.runId),
      position,
      projection: {
        anchorId: anchor.overlayAnchorId ?? `carrier:${carrier.runId}`,
        elevation:
          anchorById.get(anchor.overlayAnchorId ?? "")?.position.y ??
          CRATE_ELEVATION,
        id: carrier.runId,
        kind: "carrier",
      },
      shape: "crate",
      tone: crateTone(verdict.level, verdict.confirmed),
      ...(anchor.transitionFrom
        ? {
            transfer: {
              from: {
                x: anchor.transitionFrom.x,
                z: anchor.transitionFrom.z,
              },
              signature: `${carrier.runId}\u0000${
                carrier.transition?.fromStationId ?? ""
              }\u0000${carrier.stationId}\u0000${carrier.stageId ?? ""}`,
            },
          }
        : {}),
    });
  }

  for (const anchor of layout.workers) {
    if (!anchor.rendered) {
      continue;
    }
    const placement = placementById.get(anchor.id);
    if (!placement) {
      continue;
    }
    const position = { x: anchor.position.x, z: anchor.position.z };
    specs.push({
      active: placement.stationId
        ? stationById.get(placement.stationId)?.status === "running"
        : false,
      emphasis: lens === "risk" ? "context" : "primary",
      entity: "worker",
      key: placement.id,
      orientation: 0,
      phase: plantPhase(placement.id),
      position,
      projection: {
        anchorId: anchor.overlayAnchorId ?? `worker:${placement.id}`,
        elevation:
          anchorById.get(anchor.overlayAnchorId ?? "")?.position.y ??
          WORKER_ELEVATION,
        id: placement.id,
        kind: "worker",
      },
      shape: "worker",
      tone: placement.active ? "worker" : "workerIdle",
    });
  }

  return specs;
}

/**
 * Reconciles the specification list against retained objects.
 *
 * An entity that is still present keeps its Object3D identity, its materials,
 * and its animation phase. Only a structural shape change replaces it, and only
 * a disappearance disposes it.
 */
export function reconcilePlantEntities<TEntity>(
  registry: Map<string, PlantEntityRecord<TEntity>>,
  specs: readonly PlantEntitySpec[],
  handlers: PlantReconcileHandlers<TEntity>,
): PlantReconcileStats {
  const stats: PlantReconcileStats = {
    created: 0,
    live: 0,
    removed: 0,
    replaced: 0,
    updated: 0,
  };
  const seen = new Set<string>();

  for (const spec of specs) {
    const registryKey = plantEntityRegistryKey(spec.entity, spec.key);
    if (seen.has(registryKey)) {
      continue;
    }
    seen.add(registryKey);
    const existing = registry.get(registryKey);
    if (existing && existing.spec.shape === spec.shape) {
      handlers.update(existing.entity, spec, existing.spec);
      existing.spec = spec;
      stats.updated += 1;
      continue;
    }
    if (existing) {
      handlers.dispose(existing.entity, existing.spec);
      stats.replaced += 1;
    }
    registry.set(registryKey, { entity: handlers.create(spec), spec });
    stats.created += 1;
  }

  for (const [registryKey, record] of [...registry]) {
    if (seen.has(registryKey)) {
      continue;
    }
    handlers.dispose(record.entity, record.spec);
    registry.delete(registryKey);
    stats.removed += 1;
  }

  stats.live = registry.size;
  return stats;
}

/**
 * The silhouette a stage is drawn with.
 *
 * A declared evaluator gets its own shape rather than borrowing the shape of
 * whatever node kind it happens to be implemented as, because "this stage
 * judges the work" is the distinction an operator is actually scanning for.
 */
export function machineShapeKind(station: FactoryStation): string {
  if (station.evaluator) {
    return "evaluator";
  }
  return station.kind;
}

/**
 * A machine's body tone.
 *
 * Hazard status is *not* painted across the body: the beacon and ring carry it.
 * A hazard machine keeps a neutral body so its silhouette stays readable and a
 * bay full of trouble does not collapse into one field of alarm colour.
 */
export function machineTone(station: FactoryStation): PlantTone {
  if (station.status === "unknown") {
    return "statusUnknown";
  }
  if (station.status === "running") {
    return "machineBody";
  }
  return "machineBodyAlt";
}

/** A work order's crate tone. Unconfirmed reads never take a hazard colour. */
export function crateTone(level: PlantRiskLevel, confirmed: boolean): PlantTone {
  if (!confirmed) {
    return level === "unknown" ? "crateUnknown" : "crate";
  }
  if (level === "blocked") {
    return "crateBlocked";
  }
  if (level === "held") {
    return "crateHeld";
  }
  return "crate";
}
