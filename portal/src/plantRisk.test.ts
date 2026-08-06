import { describe, expect, it } from "vitest";

import type {
  FactoryCarrier,
  FactoryFloorModel,
  FactoryLane,
  FactoryStation,
} from "./factoryModel";
import {
  assessCarrierRisk,
  assessLaneRisk,
  assessStationRisk,
  isConfirmedRiskLevel,
  maxPlantRiskLevel,
  PLANT_READ_CURRENT,
  PLANT_RISK_COMPLETE,
  PLANT_RISK_CONTEXT_MIN_OPACITY,
  PLANT_RISK_CONTEXT_OPACITY,
  PLANT_RISK_LEVELS,
  plantCompletenessGaps,
  plantRiskCompleteness,
  plantRiskEmphasis,
  plantRiskIsComplete,
  plantRiskLevelLabel,
  plantRiskMarkerShape,
  plantRiskRank,
  summarizePlantRisk,
  type PlantFreshness,
  type PlantReadState,
  type PlantRiskLevel,
} from "./plantRisk";

function readState(query: PlantFreshness = "live"): PlantReadState {
  return { ...PLANT_READ_CURRENT, query };
}

function station(overrides: Partial<FactoryStation> = {}): FactoryStation {
  return {
    blockedCount: 0,
    column: 0,
    gaggle: "core",
    hardBlockedCount: 0,
    height: 1,
    id: "station-1",
    isStart: true,
    kind: "deterministic",
    laneId: "lane-1",
    limit: 4,
    pausedCount: 0,
    renderedRunIds: [],
    renderedWorkerIds: [],
    overflowRunCount: 0,
    row: 0,
    runIds: [],
    source: "declared",
    stageId: "stage-1",
    status: "running",
    unknownCount: 0,
    width: 1,
    wip: 1,
    workerIds: [],
    workerOverflowCount: 0,
    workflow: "build",
    workflowDisplayName: "Build",
    x: 0,
    y: 0,
    ...overrides,
  };
}

function carrier(overrides: Partial<FactoryCarrier> = {}): FactoryCarrier {
  return {
    confirmed: true,
    durationMillis: 1,
    gaggle: "core",
    infraRetryCount: 0,
    laneId: "lane-1",
    lastActivityAt: "2024-01-01T00:00:00Z",
    ownerWorkerId: undefined,
    phase: "running",
    policyRetryCount: 0,
    queueIndex: 0,
    rendered: true,
    repassCount: 0,
    retryCount: 0,
    runId: "run-1",
    startedAt: "2024-01-01T00:00:00Z",
    state: "running",
    stationId: "station-1",
    triggerKind: "manual",
    workflow: "build",
    workflowDisplayName: "Build",
    x: 0,
    y: 0,
    ...overrides,
  };
}

function lane(overrides: Partial<FactoryLane> = {}): FactoryLane {
  return {
    activeRuns: 1,
    blockedRuns: 0,
    conveyors: [],
    displayName: "Build",
    docks: [],
    gaggle: "core",
    gaggleDisplayName: "Core",
    height: 1,
    id: "lane-1",
    limit: 4,
    source: "declared",
    stageCount: 1,
    stations: [],
    unreadRuns: 0,
    width: 1,
    workflow: "build",
    x: 0,
    y: 0,
    yard: {
      height: 1,
      id: "yard-1",
      laneId: "lane-1",
      overflowRunCount: 0,
      renderedRunIds: [],
      runIds: [],
      width: 1,
      x: 0,
      y: 0,
    },
    ...overrides,
  };
}

function model(overrides: Partial<FactoryFloorModel> = {}): FactoryFloorModel {
  return {
    attention: [],
    capacity: { limit: 4, saturation: 0.25, unknownLimits: 0, wip: 1 },
    carriers: [],
    commons: {
      height: 1,
      overflowWorkerCount: 0,
      renderedWorkerIds: [],
      width: 1,
      workerIds: [],
      x: 0,
      y: 0,
    },
    counts: {
      activeRuns: 1,
      blockedRuns: 0,
      blockedStages: 0,
      gaggles: 1,
      goobers: 1,
      heldStages: 0,
      idleGoobers: 0,
      queuedRuns: 0,
      unreadRuns: 0,
      workflows: 1,
    },
    emptyReason: undefined,
    gaggles: [],
    height: 1,
    lanes: [lane()],
    runsTruncated: false,
    scope: { gaggle: undefined, workflow: undefined },
    stations: [station()],
    width: 1,
    workers: [],
    workflows: [],
    ...overrides,
  };
}

describe("plant risk precedence", () => {
  it("orders blocked above held above impeded above unknown above healthy", () => {
    const ranks = PLANT_RISK_LEVELS.map(plantRiskRank);
    expect(ranks).toEqual([4, 3, 2, 1, 0]);
    expect(maxPlantRiskLevel("held", "blocked")).toBe("blocked");
    expect(maxPlantRiskLevel("impeded", "held")).toBe("held");
    expect(maxPlantRiskLevel("unknown", "impeded")).toBe("impeded");
    expect(maxPlantRiskLevel("healthy", "unknown")).toBe("unknown");
    expect(maxPlantRiskLevel("healthy", "healthy")).toBe("healthy");
  });

  it("counts only real hazards as confirmed levels", () => {
    expect(isConfirmedRiskLevel("blocked")).toBe(true);
    expect(isConfirmedRiskLevel("held")).toBe(true);
    expect(isConfirmedRiskLevel("impeded")).toBe(true);
    // Not knowing is not a hazard. This is the whole point of the lens.
    expect(isConfirmedRiskLevel("unknown")).toBe(false);
    expect(isConfirmedRiskLevel("healthy")).toBe(false);
  });

  it("labels every level with the inspector's own wording", () => {
    const labels = PLANT_RISK_LEVELS.map(plantRiskLevelLabel);
    expect(labels).toEqual([
      "Blocked",
      "Human hold",
      "Impeded",
      "Unread",
      "No confirmed risk",
    ]);
  });
});

describe("station risk", () => {
  it("confirms blocked, held and impeded stages", () => {
    for (const [status, level] of [
      ["blocked", "blocked"],
      ["held", "held"],
      ["impeded", "impeded"],
    ] as const) {
      const verdict = assessStationRisk(station({ status }));
      expect(verdict.level).toBe<PlantRiskLevel>(level);
      expect(verdict.confirmed).toBe(true);
    }
  });

  it("treats an unread stage as incomplete, never as a hazard", () => {
    const verdict = assessStationRisk(station({ status: "unknown" }));
    expect(verdict.level).toBe("unknown");
    expect(verdict.confirmed).toBe(false);
    expect(verdict.incomplete).toBe(true);
    expect(verdict.reasons).toContain("stage state unread");
  });

  it("reports observed topology and unread capacity as completeness only", () => {
    const observed = assessStationRisk(
      station({ source: "observed", status: "running" }),
    );
    expect(observed.level).toBe("unknown");
    expect(observed.confirmed).toBe(false);
    expect(observed.incomplete).toBe(true);
    expect(observed.reasons).toContain("stage observed, not declared");

    const unlimited = assessStationRisk(
      station({ limit: undefined, status: "running" }),
    );
    expect(unlimited.level).toBe("unknown");
    expect(unlimited.reasons).toContain("stage capacity unread");
  });

  /**
   * Completeness is orthogonal: a confirmed hazard keeps its confirmed level
   * even when the same stage also has an unread signal on it.
   */
  it("keeps a confirmed hazard confirmed while also flagging incompleteness", () => {
    const verdict = assessStationRisk(
      station({ source: "observed", status: "blocked", unknownCount: 2 }),
    );
    expect(verdict.level).toBe("blocked");
    expect(verdict.confirmed).toBe(true);
    expect(verdict.incomplete).toBe(true);
    expect(verdict.reasons).toContain("2 run signals unread");
  });

  it("returns a plain healthy verdict for a fully read running stage", () => {
    const verdict = assessStationRisk(station());
    expect(verdict).toEqual({
      confirmed: false,
      incomplete: false,
      level: "healthy",
      reasons: [],
    });
  });
});

describe("carrier risk", () => {
  it("never paints an unconfirmed carrier as a confirmed hazard", () => {
    for (const state of ["blocked", "paused", "running", "unknown"] as const) {
      const verdict = assessCarrierRisk(carrier({ confirmed: false, state }));
      expect(verdict.level, state).toBe("unknown");
      expect(verdict.confirmed, state).toBe(false);
      expect(verdict.incomplete, state).toBe(true);
      expect(verdict.reasons).toContain("run signal unread");
    }
  });

  it("confirms blocked and gate-held runs that were actually read", () => {
    const blocked = assessCarrierRisk(carrier({ state: "blocked" }));
    expect(blocked.level).toBe("blocked");
    expect(blocked.confirmed).toBe(true);

    const held = assessCarrierRisk(carrier({ state: "paused" }));
    expect(held.level).toBe("held");
    expect(held.confirmed).toBe(true);
  });

  it("treats a confirmed-but-unreadable run state as unknown", () => {
    const verdict = assessCarrierRisk(carrier({ state: "unknown" }));
    expect(verdict.level).toBe("unknown");
    expect(verdict.confirmed).toBe(false);
  });
});

describe("lane risk", () => {
  it("takes the strongest verdict of the stages inside the bay", () => {
    const stations = [
      station({ id: "a", status: "running" }),
      station({ id: "b", status: "held" }),
      station({ id: "c", status: "blocked" }),
    ];
    const verdict = assessLaneRisk(lane(), stations);
    expect(verdict.level).toBe("blocked");
    expect(verdict.confirmed).toBe(true);
  });

  it("ignores stages that belong to another bay", () => {
    const stations = [station({ id: "other", laneId: "lane-2", status: "blocked" })];
    expect(assessLaneRisk(lane(), stations).level).toBe("healthy");
  });

  it("reports an observed or unread bay as incomplete, not as a hazard", () => {
    const verdict = assessLaneRisk(lane({ source: "observed", unreadRuns: 3 }), []);
    expect(verdict.level).toBe("unknown");
    expect(verdict.confirmed).toBe(false);
    expect(verdict.incomplete).toBe(true);
    expect(verdict.reasons).toContain("line observed, not declared");
    expect(verdict.reasons).toContain("3 run signals unread");
  });
});

describe("completeness modifiers", () => {
  it("is complete only when nothing at all is missing", () => {
    expect(plantRiskIsComplete(PLANT_RISK_COMPLETE)).toBe(true);
    for (const key of Object.keys(PLANT_RISK_COMPLETE) as (keyof typeof PLANT_RISK_COMPLETE)[]) {
      expect(
        plantRiskIsComplete({ ...PLANT_RISK_COMPLETE, [key]: true }),
        key,
      ).toBe(false);
    }
  });

  it("derives page freshness and model gaps separately", () => {
    expect(
      plantRiskCompleteness({ model: model(), readState: readState("degraded") }).degraded,
    ).toBe(true);
    expect(
      plantRiskCompleteness({ model: model(), readState: readState("refreshing") }).stale,
    ).toBe(true);
    const complete = plantRiskCompleteness({
      model: model(),
      readState: readState(),
    });

    expect(plantRiskIsComplete(complete)).toBe(true);

    expect(
      plantRiskCompleteness({
        model: model({ runsTruncated: true }),
        readState: readState(),
      }).truncated,
    ).toBe(true);
    expect(
      plantRiskCompleteness({
        model: model({ lanes: [lane({ source: "observed" })] }),
        readState: readState(),
      }).observedTopology,
    ).toBe(true);
    expect(
      plantRiskCompleteness({
        model: model({
          capacity: { limit: undefined, unknownLimits: 1, wip: 1 },
        }),
        readState: readState(),
      }).unknownCapacity,
    ).toBe(true);
    expect(
      plantRiskCompleteness({
        model: model({ carriers: [carrier({ confirmed: false })] }),
        readState: readState(),
      }).unreadSignals,
    ).toBe(true);
  });

  it("refuses completeness for every uncertain data and transport state", () => {
    const uncertain: PlantReadState[] = [
      { ...PLANT_READ_CURRENT, data: { kind: "unknown" } },
      {
        ...PLANT_READ_CURRENT,
        data: { kind: "lagging", lagSeconds: 90, degraded: ["sweep behind"] },
      },
      {
        ...PLANT_READ_CURRENT,
        data: {
          kind: "partial",
          lagSeconds: 2,
          missing: [
            {
              expectedBy: "2026-08-03T22:01:00Z",
              name: "run-signals",
              reason: "partition unavailable",
            },
          ],
        },
      },
      { ...PLANT_READ_CURRENT, transport: "reconnecting" },
      { ...PLANT_READ_CURRENT, transport: "offline" },
      { ...PLANT_READ_CURRENT, transport: "stale" },
      { ...PLANT_READ_CURRENT, transport: "polling-fallback" },
    ];
    for (const state of uncertain) {
      const summary = summarizePlantRisk({ model: model(), readState: state });
      expect(summary.complete, JSON.stringify(state)).toBe(false);
      expect(summary.allClear, JSON.stringify(state)).toBe(false);
      expect(summary.headline).toBe("Current risk cannot be confirmed");
      expect(summary.detail).toContain("Incomplete read:");
    }
  });

  it("names every gap in a closed vocabulary", () => {
    const gaps = plantCompletenessGaps({
      ...PLANT_RISK_COMPLETE,
      degraded: true,
      observedTopology: true,
      stale: true,
      truncated: true,
      unknownCapacity: true,
      unreadSignals: true,
    });
    expect(gaps).toEqual([
      "last refresh failed",
      "refresh in flight",
      "signals unread",
      "run list truncated",
      "topology observed, not declared",
      "capacity limits unread",
    ]);
    expect(plantCompletenessGaps(PLANT_RISK_COMPLETE)).toEqual([]);
  });
});

describe("floor summary", () => {
  it("says there is no confirmed current risk only when the read is complete", () => {
    const summary = summarizePlantRisk({ model: model(), readState: readState() });
    expect(summary.level).toBe("healthy");
    expect(summary.allClear).toBe(true);
    expect(summary.complete).toBe(true);
    expect(summary.confirmed).toBe(0);
    expect(summary.headline).toBe("No confirmed current risk");
    expect(summary.detail).toBe("");
  });

  it("refuses the all-clear while the page is refreshing or degraded", () => {
    for (const freshness of ["refreshing", "degraded"] as const) {
      const summary = summarizePlantRisk({
        model: model(),
        readState: readState(freshness),
      });
      expect(summary.allClear, freshness).toBe(false);
      expect(summary.complete, freshness).toBe(false);
      expect(summary.headline).toBe("Current risk cannot be confirmed");
      expect(summary.detail).toContain("Incomplete read");
    }
  });

  it("refuses the all-clear when the floor itself is incomplete", () => {
    const summary = summarizePlantRisk({
      model: model({ runsTruncated: true }),
      readState: readState(),
    });
    expect(summary.allClear).toBe(false);
    expect(summary.headline).toBe("Current risk cannot be confirmed");
    expect(summary.detail).toContain("run list truncated");
  });

  it("refuses the all-clear when retained topology failed to refresh", () => {
    const summary = summarizePlantRisk({
      model: model({ topologyReadFailures: ["core/build"] }),
      readState: readState(),
    });
    expect(summary.allClear).toBe(false);
    expect(summary.complete).toBe(false);
    expect(summary.detail).toContain("topology observed, not declared");
  });

  it("counts confirmed hazards and never counts unknowns among them", () => {
    const summary = summarizePlantRisk({
      model: model({
        carriers: [
          carrier({ runId: "r1", state: "blocked" }),
          carrier({ confirmed: false, runId: "r2", state: "blocked" }),
        ],
        stations: [
          station({ id: "s1", status: "blocked" }),
          station({ id: "s2", status: "unknown" }),
          station({ id: "s3", status: "held" }),
        ],
      }),
      readState: readState(),
    });
    expect(summary.level).toBe("blocked");
    expect(summary.confirmed).toBe(3);
    expect(summary.stations.blocked).toBe(1);
    expect(summary.stations.held).toBe(1);
    expect(summary.stations.unknown).toBe(1);
    expect(summary.carriers.blocked).toBe(1);
    expect(summary.carriers.unknown).toBe(1);
    expect(summary.headline).toBe("2 confirmed blocked");
  });

  it("headlines an unread-only floor without implying a hazard", () => {
    const summary = summarizePlantRisk({
      model: model({
        carriers: [carrier({ confirmed: false })],
        stations: [station({ status: "unknown" })],
      }),
      readState: readState(),
    });
    expect(summary.level).toBe("unknown");
    expect(summary.confirmed).toBe(0);
    expect(summary.headline).toBe("Current risk cannot be confirmed · 2 unread");
    expect(summary.allClear).toBe(false);
  });

  it("headlines hold and impeded floors at their own level", () => {
    const held = summarizePlantRisk({
      model: model({ stations: [station({ status: "held" })] }),
      readState: readState(),
    });
    expect(held.headline).toBe("1 confirmed on human hold");

    const impeded = summarizePlantRisk({
      model: model({ stations: [station({ status: "impeded" })] }),
      readState: readState(),
    });
    expect(impeded.headline).toBe("1 confirmed impeded");
  });
});

describe("risk emphasis", () => {
  it("promotes only confirmed hazards to primary", () => {
    expect(plantRiskEmphasis(assessStationRisk(station({ status: "blocked" })))).toBe(
      "primary",
    );
    expect(plantRiskEmphasis(assessStationRisk(station({ status: "held" })))).toBe(
      "primary",
    );
    expect(
      plantRiskEmphasis(assessCarrierRisk(carrier({ confirmed: false, state: "blocked" }))),
    ).toBe("unknown");
    expect(plantRiskEmphasis(assessStationRisk(station({ status: "unknown" })))).toBe(
      "unknown",
    );
    expect(plantRiskEmphasis(assessStationRisk(station()))).toBe("context");
  });

  it("keeps healthy context legible instead of erasing it", () => {
    expect(PLANT_RISK_CONTEXT_OPACITY).toBeGreaterThanOrEqual(
      PLANT_RISK_CONTEXT_MIN_OPACITY,
    );
    expect(PLANT_RISK_CONTEXT_MIN_OPACITY).toBeGreaterThanOrEqual(0.75);
  });

  it("uses a distinct non-hue silhouette for each non-healthy level", () => {
    const shapes = [
      plantRiskMarkerShape("blocked"),
      plantRiskMarkerShape("held"),
      plantRiskMarkerShape("impeded"),
      plantRiskMarkerShape("unknown"),
    ];
    expect(new Set(shapes).size).toBe(4);
    expect(plantRiskMarkerShape("unknown")).toBe("open-diamond");
    expect(plantRiskMarkerShape("held")).not.toBe(
      plantRiskMarkerShape("unknown"),
    );
  });
});
