/**
 * The truthful Risk model.
 *
 * Risk answers one question: *what is confirmed to be wrong right now, and how
 * much of the floor could not be read?* Those are two different answers and the
 * lens must never merge them. A stale page, an observed-rather-than-declared
 * topology, a truncated run list, and an unread capacity limit are all reasons
 * the picture is incomplete; none of them is a hazard, and painting them as one
 * teaches operators to ignore the colour that means "stop".
 *
 * Everything here is pure: no Three.js, no DOM, no React. The lens, the scene,
 * the overlay, the inspector, and the harness all read the same functions, so a
 * ring in the hall and a sentence in the drawer cannot disagree.
 */

import type {
  FactoryCarrier,
  FactoryFloorModel,
  FactoryLane,
  FactoryStation,
} from "./factoryModel";
import type { DataFreshness, LiveFreshness } from "./liveData";

/**
 * Confirmed hazard levels plus the two non-hazard outcomes.
 *
 * `unknown` is deliberately *not* a hazard: it means the read did not confirm a
 * state, which is why it never inherits hold amber.
 */
export type PlantRiskLevel =
  | "blocked"
  | "held"
  | "impeded"
  | "unknown"
  | "healthy";

/** Precedence: blocked > held > impeded > unknown/incomplete > healthy. */
const RISK_RANK: Record<PlantRiskLevel, number> = {
  blocked: 4,
  healthy: 0,
  held: 3,
  impeded: 2,
  unknown: 1,
};

export const PLANT_RISK_LEVELS: readonly PlantRiskLevel[] = [
  "blocked",
  "held",
  "impeded",
  "unknown",
  "healthy",
];

export function plantRiskRank(level: PlantRiskLevel): number {
  return RISK_RANK[level];
}

export function maxPlantRiskLevel(
  a: PlantRiskLevel,
  b: PlantRiskLevel,
): PlantRiskLevel {
  return RISK_RANK[a] >= RISK_RANK[b] ? a : b;
}

/** Confirmed hazards, in precedence order. Excludes `unknown` and `healthy`. */
export function isConfirmedRiskLevel(level: PlantRiskLevel): boolean {
  return level === "blocked" || level === "held" || level === "impeded";
}

/** How the page's own read is doing when the lens is evaluated. */
export type PlantFreshness = "live" | "refreshing" | "degraded";

/**
 * The complete read truth the Plant needs before it may claim an all-clear.
 *
 * Query freshness says whether this page is currently refreshing or retaining
 * a failed read. Data freshness says how current and complete the daemon read
 * model is. Transport says whether the next change can reach the page. They are
 * deliberately separate because any one can be healthy while another is not.
 */
export interface PlantReadState {
  query: PlantFreshness;
  data: DataFreshness;
  transport: LiveFreshness;
}

/** A fully current, complete and connected read for direct embeddings/tests. */
export const PLANT_READ_CURRENT: PlantReadState = {
  data: { kind: "current", lagSeconds: 0 },
  query: "live",
  transport: "connected",
};

/**
 * Why the picture may be incomplete.
 *
 * Every flag is orthogonal to the hazard level: a fully healthy floor can be
 * incomplete, and a confirmed blocked stage can be perfectly fresh.
 */
export interface PlantRiskCompleteness {
  /** The page is showing the last confirmed read while a refresh is in flight. */
  stale: boolean;
  /** The last refresh failed; the plant is the previous confirmed state. */
  degraded: boolean;
  /** At least one lane's topology was observed from runs, not declared. */
  observedTopology: boolean;
  /** The daemon reported more active runs than the floor bound shows. */
  truncated: boolean;
  /** At least one shown workflow reports no usable concurrency limit. */
  unknownCapacity: boolean;
  /** At least one run or stage signal could not be confirmed. */
  unreadSignals: boolean;
  /** No read-state envelope has established how current the data is. */
  dataUnknown: boolean;
  /** The daemon read model is beyond its current-data lag bound. */
  lagging: boolean;
  /** The daemon explicitly reported a partial read model. */
  partial: boolean;
  /** Live updates are reconnecting. */
  reconnecting: boolean;
  /** The browser or daemon transport is offline. */
  offline: boolean;
  /** SSE is unavailable and updates are arriving by polling. */
  pollingFallback: boolean;
  /** The transport is refreshing a full snapshot. */
  transportStale: boolean;
}

export const PLANT_RISK_COMPLETE: PlantRiskCompleteness = {
  dataUnknown: false,
  degraded: false,
  lagging: false,
  offline: false,
  observedTopology: false,
  partial: false,
  pollingFallback: false,
  reconnecting: false,
  stale: false,
  transportStale: false,
  truncated: false,
  unknownCapacity: false,
  unreadSignals: false,
};

export function plantRiskIsComplete(
  completeness: PlantRiskCompleteness,
): boolean {
  return (
    !completeness.stale &&
    !completeness.degraded &&
    !completeness.observedTopology &&
    !completeness.truncated &&
    !completeness.unknownCapacity &&
    !completeness.unreadSignals &&
    !completeness.dataUnknown &&
    !completeness.lagging &&
    !completeness.partial &&
    !completeness.reconnecting &&
    !completeness.offline &&
    !completeness.pollingFallback &&
    !completeness.transportStale
  );
}

/** Short, closed-set phrases for each completeness gap. */
export function plantCompletenessGaps(
  completeness: PlantRiskCompleteness,
): string[] {
  const gaps: string[] = [];
  if (completeness.degraded) {
    gaps.push("last refresh failed");
  }
  if (completeness.stale) {
    gaps.push("refresh in flight");
  }
  if (completeness.dataUnknown) {
    gaps.push("data freshness unknown");
  }
  if (completeness.lagging) {
    gaps.push("data lagging");
  }
  if (completeness.partial) {
    gaps.push("read model partial");
  }
  if (completeness.offline) {
    gaps.push("transport offline");
  }
  if (completeness.reconnecting) {
    gaps.push("transport reconnecting");
  }
  if (completeness.pollingFallback) {
    gaps.push("transport polling fallback");
  }
  if (completeness.transportStale) {
    gaps.push("transport refreshing snapshot");
  }
  if (completeness.unreadSignals) {
    gaps.push("signals unread");
  }
  if (completeness.truncated) {
    gaps.push("run list truncated");
  }
  if (completeness.observedTopology) {
    gaps.push("topology observed, not declared");
  }
  if (completeness.unknownCapacity) {
    gaps.push("capacity limits unread");
  }
  return gaps;
}

/** One entity's Risk verdict. */
export interface PlantRiskVerdict {
  level: PlantRiskLevel;
  /**
   * True only when the level is a hazard *and* the read that produced it was
   * confirmed. An unconfirmed carrier can never be painted as blocked or held.
   */
  confirmed: boolean;
  /** True when this entity itself carries a completeness gap. */
  incomplete: boolean;
  /** Closed-set reason phrases; never journal text. */
  reasons: readonly string[];
}

const HEALTHY: PlantRiskVerdict = {
  confirmed: false,
  incomplete: false,
  level: "healthy",
  reasons: [],
};

/**
 * A station's Risk verdict.
 *
 * `unknown` status and unread per-run signals are completeness, not hazard. An
 * observed-topology station is also incomplete: the daemon inferred it from
 * runs, so its stage order is not authoritative.
 */
export function assessStationRisk(station: FactoryStation): PlantRiskVerdict {
  const reasons: string[] = [];
  const incomplete =
    station.status === "unknown" ||
    station.unknownCount > 0 ||
    station.source === "observed" ||
    station.limit === undefined;
  if (station.source === "observed") {
    reasons.push("stage observed, not declared");
  }
  if (station.limit === undefined) {
    reasons.push("stage capacity unread");
  }
  if (station.unknownCount > 0) {
    reasons.push(`${station.unknownCount} run signals unread`);
  }
  if (station.status === "blocked") {
    return {
      confirmed: true,
      incomplete,
      level: "blocked",
      reasons: ["stage blocked", ...reasons],
    };
  }
  if (station.status === "held") {
    return {
      confirmed: true,
      incomplete,
      level: "held",
      reasons: ["human hold", ...reasons],
    };
  }
  if (station.status === "impeded") {
    return {
      confirmed: true,
      incomplete,
      level: "impeded",
      reasons: ["stage impeded", ...reasons],
    };
  }
  if (station.status === "unknown") {
    return {
      confirmed: false,
      incomplete: true,
      level: "unknown",
      reasons: ["stage state unread", ...reasons],
    };
  }
  if (incomplete) {
    return { confirmed: false, incomplete: true, level: "unknown", reasons };
  }
  return HEALTHY;
}

/**
 * A carrier's Risk verdict.
 *
 * An unconfirmed carrier is `unknown` no matter what state the summary carries:
 * the stage-level read that would have confirmed "blocked" or "paused" was not
 * available, so the plant must not draw a confirmed hazard.
 */
export function assessCarrierRisk(carrier: FactoryCarrier): PlantRiskVerdict {
  if (!carrier.confirmed) {
    return {
      confirmed: false,
      incomplete: true,
      level: "unknown",
      reasons: ["run signal unread"],
    };
  }
  if (carrier.state === "blocked") {
    return {
      confirmed: true,
      incomplete: false,
      level: "blocked",
      reasons: ["run blocked"],
    };
  }
  if (carrier.state === "paused") {
    return {
      confirmed: true,
      incomplete: false,
      level: "held",
      reasons: ["run held at a gate"],
    };
  }
  if (carrier.state === "unknown") {
    return {
      confirmed: false,
      incomplete: true,
      level: "unknown",
      reasons: ["run state unread"],
    };
  }
  return HEALTHY;
}

/** A workflow bay's verdict: the strongest verdict of the stages inside it. */
export function assessLaneRisk(
  lane: FactoryLane,
  stations: readonly FactoryStation[],
): PlantRiskVerdict {
  let level: PlantRiskLevel = "healthy";
  let confirmed = false;
  let incomplete = lane.source === "observed";
  const reasons: string[] = [];
  if (lane.source === "observed") {
    reasons.push("line observed, not declared");
  }
  if (lane.limit === undefined) {
    incomplete = true;
    reasons.push("line capacity unread");
  }
  if (lane.unreadRuns > 0) {
    incomplete = true;
    reasons.push(`${lane.unreadRuns} run signals unread`);
  }
  for (const station of stations) {
    if (station.laneId !== lane.id) {
      continue;
    }
    const verdict = assessStationRisk(station);
    const next = maxPlantRiskLevel(level, verdict.level);
    if (next !== level) {
      level = next;
      confirmed = verdict.confirmed;
    } else if (verdict.level === level && verdict.confirmed) {
      confirmed = confirmed || verdict.confirmed;
    }
    incomplete = incomplete || verdict.incomplete;
  }
  if (level === "healthy" && incomplete) {
    return { confirmed: false, incomplete, level: "unknown", reasons };
  }
  return { confirmed, incomplete, level, reasons };
}

export interface PlantRiskCounts {
  blocked: number;
  held: number;
  impeded: number;
  unknown: number;
  healthy: number;
}

export interface PlantRiskSummary {
  /** The strongest verdict on the floor. */
  level: PlantRiskLevel;
  /** Confirmed hazards only. Unknowns are never counted here. */
  confirmed: number;
  stations: PlantRiskCounts;
  carriers: PlantRiskCounts;
  completeness: PlantRiskCompleteness;
  complete: boolean;
  /**
   * True only when nothing is confirmed wrong *and* nothing is missing.
   * The Plant may only say "no confirmed current risk" when this is true.
   */
  allClear: boolean;
  /** The exact sentence the Plant and inspector both use. */
  headline: string;
  /** The completeness sentence, empty when the read is complete. */
  detail: string;
}

export interface PlantRiskInput {
  model: FactoryFloorModel;
  readState: PlantReadState;
}

/** Derives the completeness modifiers from the model and the page's own read. */
export function plantRiskCompleteness({
  model,
  readState,
}: PlantRiskInput): PlantRiskCompleteness {
  return {
    dataUnknown: readState.data.kind === "unknown",
    degraded: readState.query === "degraded",
    lagging: readState.data.kind === "lagging",
    offline: readState.transport === "offline",
    observedTopology:
      model.lanes.some((lane) => lane.source === "observed") ||
      (model.topologyReadFailures?.length ?? 0) > 0,
    partial: readState.data.kind === "partial",
    pollingFallback: readState.transport === "polling-fallback",
    reconnecting: readState.transport === "reconnecting",
    stale: readState.query === "refreshing",
    transportStale: readState.transport === "stale",
    truncated: model.runsTruncated,
    unknownCapacity:
      model.capacity.unknownLimits > 0 || model.capacity.limit === undefined,
    unreadSignals:
      model.counts.unreadRuns > 0 ||
      model.carriers.some((carrier) => !carrier.confirmed) ||
      model.stations.some(
        (station) => station.status === "unknown" || station.unknownCount > 0,
      ),
  };
}

function emptyCounts(): PlantRiskCounts {
  return { blocked: 0, healthy: 0, held: 0, impeded: 0, unknown: 0 };
}

/**
 * Summarizes the whole floor.
 *
 * The headline is a closed set. It states a confirmed hazard when one exists,
 * and otherwise states either a genuine all-clear or, when the read is
 * incomplete, that nothing was confirmed *in what could be read* — never that
 * the floor is clear.
 */
export function summarizePlantRisk(input: PlantRiskInput): PlantRiskSummary {
  const { model, readState } = input;
  const completeness = plantRiskCompleteness(input);
  const stations = emptyCounts();
  const carriers = emptyCounts();
  let level: PlantRiskLevel = "healthy";

  for (const station of model.stations) {
    const verdict = assessStationRisk(station);
    stations[verdict.level] += 1;
    level = maxPlantRiskLevel(level, verdict.level);
  }
  for (const carrier of model.carriers) {
    const verdict = assessCarrierRisk(carrier);
    carriers[verdict.level] += 1;
    level = maxPlantRiskLevel(level, verdict.level);
  }

  const confirmed =
    stations.blocked +
    stations.held +
    stations.impeded +
    carriers.blocked +
    carriers.held +
    carriers.impeded;
  const complete = plantRiskIsComplete(completeness);
  if (level === "healthy" && !complete) {
    level = "unknown";
  }
  const allClear = level === "healthy" && complete;
  const gaps = plantReadGaps(readState, completeness, model);
  const detail = gaps.length === 0 ? "" : `Incomplete read: ${gaps.join(", ")}.`;

  return {
    allClear,
    carriers,
    complete,
    completeness,
    confirmed,
    detail,
    headline: riskHeadline({ carriers, complete, confirmed, level, stations }),
    level,
    stations,
  };
}

function riskHeadline({
  carriers,
  complete,
  confirmed,
  level,
  stations,
}: {
  carriers: PlantRiskCounts;
  complete: boolean;
  confirmed: number;
  level: PlantRiskLevel;
  stations: PlantRiskCounts;
}): string {
  if (level === "blocked") {
    return `${stations.blocked + carriers.blocked} confirmed blocked`;
  }
  if (level === "held") {
    return `${stations.held + carriers.held} confirmed on human hold`;
  }
  if (level === "impeded") {
    return `${stations.impeded + carriers.impeded} confirmed impeded`;
  }
  if (level === "unknown") {
    const unread = stations.unknown + carriers.unknown;
    return unread > 0
      ? `Current risk cannot be confirmed · ${unread} unread`
      : "Current risk cannot be confirmed";
  }
  if (!complete) {
    return "Current risk cannot be confirmed";
  }
  return confirmed === 0
    ? "No confirmed current risk"
    : `${confirmed} confirmed at risk`;
}

/**
 * How an entity is drawn in the Risk lens.
 *
 * `primary` keeps full legibility and gains a status marker, `unknown` gets the
 * neutral incomplete treatment, and `context` stays visible but desaturated so
 * the operator can still see *where* on the floor the hazard is.
 */
export type PlantRiskEmphasis = "primary" | "unknown" | "context";

export function plantRiskEmphasis(verdict: PlantRiskVerdict): PlantRiskEmphasis {
  if (isConfirmedRiskLevel(verdict.level) && verdict.confirmed) {
    return "primary";
  }
  if (verdict.level === "unknown" || verdict.incomplete) {
    return "unknown";
  }
  return "context";
}

/**
 * The minimum legibility healthy context keeps in the Risk lens.
 *
 * The previous lens erased the plant to 28% over a near-white background, which
 * removed the map the hazard was supposed to be located on. Context is
 * desaturated, not deleted.
 */
export const PLANT_RISK_CONTEXT_OPACITY = 0.82;
export const PLANT_RISK_CONTEXT_DESATURATION = 0.72;
export const PLANT_RISK_CONTEXT_MIN_OPACITY = 0.75;

/** Human-readable label for a level, matching the inspector's wording. */
export function plantRiskLevelLabel(level: PlantRiskLevel): string {
  switch (level) {
    case "blocked":
      return "Blocked";
    case "held":
      return "Human hold";
    case "impeded":
      return "Impeded";
    case "unknown":
      return "Unread";
    case "healthy":
      return "No confirmed risk";
  }
}

/** Non-colour marker vocabulary shared by WebGL, DOM fallbacks and tests. */
export type PlantRiskMarkerShape =
  | "stop-octagon"
  | "pause-bars"
  | "warning-triangle"
  | "open-diamond"
  | "none";

export function plantRiskMarkerShape(
  level: PlantRiskLevel,
): PlantRiskMarkerShape {
  switch (level) {
    case "blocked":
      return "stop-octagon";
    case "held":
      return "pause-bars";
    case "impeded":
      return "warning-triangle";
    case "unknown":
      return "open-diamond";
    case "healthy":
      return "none";
  }
}

function plantReadGaps(
  readState: PlantReadState,
  completeness: PlantRiskCompleteness,
  model: FactoryFloorModel,
): string[] {
  const gaps: string[] = [];
  if (completeness.degraded) {
    gaps.push("last page refresh failed");
  } else if (completeness.stale) {
    gaps.push("page refresh in flight");
  }
  switch (readState.data.kind) {
    case "unknown":
      gaps.push("data freshness unknown");
      break;
    case "lagging": {
      const reasons =
        readState.data.degraded.length > 0
          ? ` (${readState.data.degraded.join("; ")})`
          : "";
      gaps.push(`data lagging by up to ${formatLag(readState.data.lagSeconds)}${reasons}`);
      break;
    }
    case "partial":
      gaps.push(
        `partial read model: ${readState.data.missing
          .map((entry) => entry.name)
          .join(", ")}`,
      );
      break;
    case "current":
      break;
  }
  switch (readState.transport) {
    case "offline":
      gaps.push("transport offline");
      break;
    case "reconnecting":
      gaps.push("transport reconnecting");
      break;
    case "polling-fallback":
      gaps.push("transport in polling fallback");
      break;
    case "stale":
      gaps.push("transport refreshing a full snapshot");
      break;
    case "connected":
      break;
  }
  if (model.counts.unreadRuns > 0 || completeness.unreadSignals) {
    gaps.push("signals unread");
  }
  if (completeness.truncated) {
    gaps.push("run list truncated");
  }
  if (completeness.observedTopology) {
    gaps.push("topology observed, not declared");
  }
  if (completeness.unknownCapacity) {
    gaps.push("capacity limits unread");
  }
  return [...new Set(gaps)];
}

function formatLag(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) {
    return "an unknown interval";
  }
  if (seconds < 60) {
    return `${Math.round(seconds)}s`;
  }
  return `${Math.round(seconds / 60)}m`;
}
