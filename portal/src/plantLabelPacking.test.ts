import { describe, expect, it } from "vitest";

import {
  PLANT_HIT_TARGET_MIN,
  PLANT_LABEL_TIER_ORDER,
  PLANT_LABEL_TIER_PRIORITY,
  PLANT_TOUCH_HIT_TARGET_MIN,
  estimatePlantLabelWidth,
  packPlantLabels,
  plantHitRect,
  plantLabelOffsets,
  plantLabelPriority,
  type PlantLabelCandidate,
  type PlantLabelTier,
} from "./plantLabelPacking";
import { plantRectsOverlap, plantScreenRect } from "./plantProjection";

const SAFE = plantScreenRect(0, 0, 800, 600);

function candidate(
  id: string,
  tier: PlantLabelTier,
  x: number,
  y: number,
  overrides: Partial<PlantLabelCandidate> = {},
): PlantLabelCandidate {
  return {
    groupId: `group:${tier}`,
    id,
    origin: { x, y },
    point: { x, y: y + 24 },
    size: { height: 20, width: 80 },
    tier,
    ...overrides,
  };
}

describe("plantLabelPriority", () => {
  it("orders selected and focused above alarm, and idle last", () => {
    const ordered = [...PLANT_LABEL_TIER_ORDER];
    expect(ordered).toEqual([
      "selected",
      "focused",
      "alarm",
      "attention",
      "active",
      "sign",
      "idle",
    ]);
    for (let index = 1; index < ordered.length; index += 1) {
      expect(PLANT_LABEL_TIER_PRIORITY[ordered[index - 1]]).toBeGreaterThan(
        PLANT_LABEL_TIER_PRIORITY[ordered[index]],
      );
    }
  });

  it("honours an explicit priority override", () => {
    expect(plantLabelPriority("idle")).toBe(PLANT_LABEL_TIER_PRIORITY.idle);
    expect(plantLabelPriority("idle", 5000)).toBe(5000);
    expect(plantLabelPriority("selected")).toBeGreaterThan(
      plantLabelPriority("alarm"),
    );
  });
});

describe("estimatePlantLabelWidth", () => {
  it("grows with the text and stays inside the clamp", () => {
    const short = estimatePlantLabelWidth("A");
    const long = estimatePlantLabelWidth("A".repeat(400));
    expect(long).toBeGreaterThan(short);
    expect(short).toBeGreaterThanOrEqual(34);
    expect(long).toBeLessThanOrEqual(168);
  });
});

describe("plantLabelOffsets", () => {
  it("is deterministic and starts at the preferred point", () => {
    const first = plantLabelOffsets({ height: 20, width: 80 });
    const second = plantLabelOffsets({ height: 20, width: 80 });
    expect(first).toEqual(second);
    expect(first.length).toBeGreaterThan(8);
    expect(first[0]).toEqual({ x: 0, y: 0 });
    expect(new Set(first.map((offset) => `${offset.x}:${offset.y}`)).size).toBe(
      first.length,
    );
  });
});

describe("packPlantLabels", () => {
  it("places isolated labels at their preferred point", () => {
    const result = packPlantLabels(
      [
        candidate("a", "active", 100, 100),
        candidate("b", "active", 400, 100),
        candidate("c", "active", 100, 400),
      ],
      { safeRect: SAFE },
    );
    expect(result.metrics.placed).toBe(3);
    expect(result.metrics.collapsed).toBe(0);
    expect(result.metrics.overlaps).toBe(0);
    for (const placement of result.placements) {
      expect(placement.offsetIndex).toBe(0);
      expect(placement.clamped).toBe(false);
    }
  });

  it("never leaves a residual overlap, however dense the cluster", () => {
    const candidates: PlantLabelCandidate[] = [];
    for (let index = 0; index < 60; index += 1) {
      candidates.push(
        candidate(`dense-${index}`, "active", 400 + (index % 3), 300 + index * 0.5),
      );
    }
    const result = packPlantLabels(candidates, { safeRect: SAFE });
    expect(result.metrics.overlaps).toBe(0);
    expect(result.metrics.placed + result.metrics.collapsed).toBe(
      candidates.length,
    );
    const rects = [
      ...result.placements.map((placement) => placement.rect),
      ...result.chips.map((chip) => chip.rect),
    ];
    for (let left = 0; left < rects.length; left += 1) {
      for (let right = left + 1; right < rects.length; right += 1) {
        expect(plantRectsOverlap(rects[left], rects[right])).toBe(false);
      }
    }
  });

  it("keeps the highest priority label and collapses the rest truthfully", () => {
    const candidates = [
      candidate("idle-1", "idle", 300, 300, { groupId: "bay" }),
      candidate("idle-2", "idle", 301, 301, { groupId: "bay" }),
      candidate("selected-1", "selected", 302, 302, { groupId: "bay" }),
      candidate("idle-3", "idle", 303, 303, { groupId: "bay" }),
      candidate("alarm-1", "alarm", 304, 304, { groupId: "bay" }),
    ];
    const result = packPlantLabels(candidates, {
      gap: 4,
      safeRect: plantScreenRect(280, 280, 120, 60),
    });
    const placedIds = result.placements.map((placement) => placement.id);
    expect(placedIds).toContain("selected-1");
    expect(result.metrics.overlaps).toBe(0);
    const reported =
      result.placements.length +
      result.chips.reduce((sum, chip) => sum + chip.count, 0);
    expect(reported).toBe(candidates.length);
    for (const chip of result.chips) {
      expect(chip.count).toBe(chip.ids.length);
      expect(chip.count).toBeGreaterThan(0);
    }
  });

  it("orders placement strictly by priority when space is scarce", () => {
    const candidates = [
      candidate("idle", "idle", 100, 100, { groupId: "shared" }),
      candidate("sign", "sign", 100, 100, { groupId: "shared" }),
      candidate("active", "active", 100, 100, { groupId: "shared" }),
      candidate("attention", "attention", 100, 100, { groupId: "shared" }),
      candidate("alarm", "alarm", 100, 100, { groupId: "shared" }),
      candidate("focused", "focused", 100, 100, { groupId: "shared" }),
      candidate("selected", "selected", 100, 100, { groupId: "shared" }),
    ];
    const result = packPlantLabels(candidates, { safeRect: SAFE });
    const priorities = result.placements.map((placement) => placement.priority);
    expect(result.placements[0].id).toBe("selected");
    for (let index = 1; index < priorities.length; index += 1) {
      expect(priorities[index - 1]).toBeGreaterThanOrEqual(priorities[index]);
    }
  });

  it("clamps every placement inside the safe rectangle", () => {
    const candidates = [
      candidate("edge-left", "active", -40, 300),
      candidate("edge-right", "active", 830, 300),
      candidate("edge-top", "active", 400, -40),
      candidate("edge-bottom", "active", 400, 640),
    ];
    const result = packPlantLabels(candidates, { safeRect: SAFE });
    expect(result.metrics.clipped).toBe(0);
    for (const placement of [...result.placements, ...result.chips]) {
      expect(placement.rect.left).toBeGreaterThanOrEqual(SAFE.left - 0.001);
      expect(placement.rect.top).toBeGreaterThanOrEqual(SAFE.top - 0.001);
      expect(placement.rect.right).toBeLessThanOrEqual(SAFE.right + 0.001);
      expect(placement.rect.bottom).toBeLessThanOrEqual(SAFE.bottom + 0.001);
    }
  });

  it("routes labels around obstacles such as hit targets", () => {
    const obstacle = plantScreenRect(360, 300, 80, 80);
    const result = packPlantLabels([candidate("blocked", "active", 400, 300)], {
      obstacles: [obstacle],
      safeRect: SAFE,
    });
    expect(result.placements).toHaveLength(1);
    expect(plantRectsOverlap(result.placements[0].rect, obstacle)).toBe(false);
  });

  it("is deterministic: the same input always packs identically", () => {
    const candidates = [
      candidate("a", "active", 120, 140, { groupId: "g" }),
      candidate("b", "idle", 124, 146, { groupId: "g" }),
      candidate("c", "alarm", 128, 150, { groupId: "g" }),
      candidate("d", "sign", 132, 154, { groupId: "g" }),
    ];
    const first = packPlantLabels(candidates, { safeRect: SAFE });
    const second = packPlantLabels([...candidates], { safeRect: SAFE });
    expect(second.placements).toEqual(first.placements);
    expect(second.chips).toEqual(first.chips);
    expect(second.collapsedIds).toEqual(first.collapsedIds);
  });

  it("counts anchors outside the safe rectangle as offscreen without placing them", () => {
    const result = packPlantLabels(
      [
        candidate("inside", "active", 400, 300),
        candidate("outside", "active", 4000, 3000),
      ],
      { safeRect: SAFE },
    );
    expect(result.metrics.offscreen).toBe(1);
    expect(result.placements.map((placement) => placement.id)).toEqual([
      "inside",
    ]);
  });

  it("places a non-collapsible label even when its anchor is off the safe rect", () => {
    const result = packPlantLabels(
      [
        candidate("inside", "active", 400, 300),
        candidate("critical", "selected", 4000, 3000, { collapsible: false }),
      ],
      { safeRect: SAFE },
    );
    expect(result.metrics.offscreen).toBe(1);
    expect(result.collapsedIds).not.toContain("critical");
    const placed = result.placements.find(
      (placement) => placement.id === "critical",
    );
    expect(placed).toBeDefined();
    expect(placed!.rect.left).toBeGreaterThanOrEqual(SAFE.left);
    expect(placed!.rect.right).toBeLessThanOrEqual(SAFE.right);
    expect(result.metrics.overlaps).toBe(0);
  });

  it("uses a group origin for the chip when one is supplied", () => {
    const candidates = [
      candidate("keep", "selected", 300, 300, { groupId: "bay-a" }),
      candidate("drop-1", "idle", 300, 300, { groupId: "bay-a" }),
      candidate("drop-2", "idle", 300, 300, { groupId: "bay-a" }),
    ];
    const result = packPlantLabels(candidates, {
      groupOrigins: { "bay-a": { x: 200, y: 200 } },
      safeRect: plantScreenRect(150, 150, 220, 90),
    });
    for (const chip of result.chips) {
      expect(chip.origin).toEqual({ x: 200, y: 200 });
    }
  });

  it("packs a large scene quickly enough for a frame budget", () => {
    const candidates: PlantLabelCandidate[] = [];
    for (let index = 0; index < 600; index += 1) {
      candidates.push(
        candidate(`n-${index}`, index % 7 === 0 ? "alarm" : "idle", (index * 13) % 800, (index * 29) % 600),
      );
    }
    const started = performance.now();
    const result = packPlantLabels(candidates, { safeRect: SAFE });
    expect(performance.now() - started).toBeLessThan(400);
    expect(result.metrics.overlaps).toBe(0);
  });
});

describe("plantHitRect", () => {
  it("never falls below the desktop minimum", () => {
    const rect = plantHitRect({ x: 100, y: 100 }, 10, false);
    expect(rect.width).toBeGreaterThanOrEqual(PLANT_HIT_TARGET_MIN);
    expect(rect.height).toBeGreaterThanOrEqual(PLANT_HIT_TARGET_MIN);
  });

  it("never falls below the touch minimum in touch mode", () => {
    const rect = plantHitRect({ x: 100, y: 100 }, 10, true);
    expect(rect.width).toBeGreaterThanOrEqual(PLANT_TOUCH_HIT_TARGET_MIN);
    expect(rect.height).toBeGreaterThanOrEqual(PLANT_TOUCH_HIT_TARGET_MIN);
  });

  it("stays centred on the anchor", () => {
    const rect = plantHitRect({ x: 250, y: 175 }, 60, false);
    expect(rect.left + rect.width / 2).toBeCloseTo(250, 6);
    expect(rect.top + rect.height / 2).toBeCloseTo(175, 6);
    expect(rect.width).toBe(60);
  });

  it("leaves a comfortable target alone", () => {
    expect(plantHitRect({ x: 0, y: 0 }, 46).width).toBe(46);
    expect(plantHitRect({ x: 0, y: 0 }, 46, true).width).toBe(46);
  });
});
