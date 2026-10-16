import { describe, expect, it } from "vitest";
import type { RunEvent, WorkflowGraph } from "./api/types";
import {
  deriveBranchStates,
  deriveMapVisits,
  deriveNodeStates,
  eventHeading,
  eventNodeId,
  eventSummary,
  mapVisitSummary,
  shortDigest,
} from "./runDetailData";

const DIGEST_A = `sha256:${"a1".repeat(32)}`;
const DIGEST_B = `sha256:${"b2".repeat(32)}`;
const DIGEST_C = `sha256:${"c3".repeat(32)}`;

function event(seq: number, type: RunEvent["type"], fields: Partial<RunEvent>): RunEvent {
  return {
    schema: "v1",
    seq,
    type,
    branch: 0,
    time: `2026-10-14T00:00:${String(seq).padStart(2, "0")}Z`,
    knownSchema: true,
    ...fields,
  };
}

function item(seq: number, type: "branch.started" | "branch.finished", branch: number, identity: string, digest: string, fields: Partial<RunEvent> = {}): RunEvent {
  return event(seq, type, {
    branch,
    parallel: "run-shards",
    branchName: identity,
    mapItemIdentity: identity,
    mapItemDigest: digest,
    ...fields,
  });
}

const mapStarted = event(3, "map.started", {
  parallel: "run-shards",
  mapRoster: {
    sourceTask: "plan",
    sourceSlot: "shards",
    sourceDigest: "sha256:source",
    integrity: "derived",
    failurePolicy: "fail_fast",
    maxConcurrent: 1,
    items: [
      { branch: 1, identity: "unit-a", digest: DIGEST_A },
      { branch: 2, identity: "unit-b", digest: DIGEST_B },
      { branch: 3, identity: "unit-c", digest: DIGEST_C },
    ],
  },
});

const mapEvents: RunEvent[] = [
  event(1, "stage.started", { stage: "plan" }),
  event(2, "stage.finished", { stage: "plan", status: "success" }),
  mapStarted,
  item(4, "branch.started", 1, "unit-a", DIGEST_A),
  item(5, "branch.finished", 1, "unit-a", DIGEST_A, { branchStatus: "failed" }),
  item(6, "branch.started", 2, "unit-b", DIGEST_B),
  item(7, "branch.finished", 2, "unit-b", DIGEST_B, { branchStatus: "cancelled" }),
  event(8, "map.finished", {
    parallel: "run-shards",
    completeness: [
      { branch: 1, name: "unit-a", status: "failed", artifacts: 0, mapItemIdentity: "unit-a", mapItemDigest: DIGEST_A },
      { branch: 2, name: "unit-b", status: "cancelled", artifacts: 0, mapItemIdentity: "unit-b", mapItemDigest: DIGEST_B },
      { branch: 3, name: "unit-c", status: "cancelled", artifacts: 0, mapItemIdentity: "unit-c", mapItemDigest: DIGEST_C },
    ],
  }),
];

describe("map item lineage (#7386)", () => {
  it("tracks each frozen roster item by identity and digest as of the playhead", () => {
    const early = deriveMapVisits(mapEvents, 4).get("run-shards");
    expect(early?.items.map((entry) => [entry.identity, entry.digest, entry.state])).toEqual([
      ["unit-a", DIGEST_A, "running"],
      ["unit-b", DIGEST_B, "pending"],
      ["unit-c", DIGEST_C, "pending"],
    ]);
    expect(early && mapVisitSummary(early)).toBe("3 items — 1 running, 2 pending");

    // An item fail_fast never started settles from map.finished's completeness.
    const settled = deriveMapVisits(mapEvents, 8).get("run-shards");
    expect(settled?.finishedSeq).toBe(8);
    expect(settled?.items.map((entry) => entry.state)).toEqual(["failed", "cancelled", "cancelled"]);
    expect(settled && mapVisitSummary(settled)).toBe("3 items — 1 failed, 2 cancelled");
    expect(deriveMapVisits(mapEvents, 2).size).toBe(0);
  });

  it("starts a fresh visit when the map is re-entered", () => {
    const reentered = event(9, "map.started", {
      parallel: "run-shards",
      mapRoster: { ...mapStarted.mapRoster!, items: [{ branch: 1, identity: "unit-z", digest: DIGEST_A }] },
    });
    const visit = deriveMapVisits([...mapEvents, reentered], 9).get("run-shards");
    expect(visit?.startedSeq).toBe(9);
    expect(visit?.items.map((entry) => [entry.identity, entry.state])).toEqual([["unit-z", "pending"]]);
    expect(visit && mapVisitSummary({ ...visit, items: [] })).toBe("0 items");
  });

  it("keeps map items out of the declared-branch states", () => {
    expect(deriveBranchStates(mapEvents, 8).size).toBe(0);
  });

  it("treats the map as its own graph node", () => {
    const graph: WorkflowGraph = {
      name: "mapped",
      version: 1,
      digest: "sha256:pinned",
      start: "plan",
      nodes: [
        { id: "plan", kind: "agentic" },
        { id: "run-shard", kind: "deterministic" },
        { id: "run-shards", kind: "map" },
      ],
      edges: [
        { source: "plan", target: "run-shards" },
        { source: "run-shards", target: "run-shard" },
      ],
    };
    expect(eventNodeId(mapStarted)).toBe("run-shards");
    expect(deriveNodeStates(graph, mapEvents, 4)["run-shards"]).toBe("running");
    expect(deriveNodeStates(graph, mapEvents, 8)["run-shards"]).toBe("completed");
  });

  it("describes map events with item identity and digest", () => {
    expect(eventHeading(mapStarted)).toBe("Map started");
    expect(eventSummary(mapStarted)).toBe("Map run-shards started with 3 items frozen from plan.shards.");
    expect(eventSummary(mapEvents[3])).toBe("Map item unit-a (a1a1a1a1a1a1) started.");
    expect(eventSummary(mapEvents[4])).toBe("Map item unit-a (a1a1a1a1a1a1) finished as failed.");
    expect(eventHeading(mapEvents[7])).toBe("Map finished");
    expect(eventSummary(mapEvents[7])).toBe(
      "Map run-shards finished — 0/3 items succeeded (unit-a: failed, unit-b: cancelled, unit-c: cancelled).",
    );
    expect(eventSummary(event(10, "map.finished", { parallel: "run-shards", completeness: [] }))).toBe(
      "Map run-shards finished with no items.",
    );
    expect(shortDigest(DIGEST_B)).toBe("b2b2b2b2b2b2");
  });
});
