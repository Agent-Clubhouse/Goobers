import { describe, expect, it } from "vitest";

import { buildFactoryPlantLayout } from "./factoryPlantLayout";
import {
  buildPlantOverlayItems,
  findPlantOverlayAnchorId,
  type PlantOverlayItem,
} from "./factoryPlantOverlay";
import {
  PLANT_HIT_TARGET_MIN,
  PLANT_LABEL_TIER_PRIORITY,
} from "./plantLabelPacking";
import { plantFixture, scalablePlantFixture } from "./test/plantFixtures";

function build(
  overrides: Partial<Parameters<typeof buildPlantOverlayItems>[0]> = {},
): PlantOverlayItem[] {
  const { layout, model } = plantFixture();
  return buildPlantOverlayItems({
    animateTransitions: false,
    layout,
    lens: "world",
    model,
    selection: { kind: "overview" },
    ...overrides,
  });
}

describe("buildPlantOverlayItems", () => {
  it("emits one item per positioned semantic, keyed by its anchor", () => {
    const items = build();
    expect(items.length).toBeGreaterThan(0);
    for (const item of items) {
      expect(item.id).toBe(item.anchorId);
      expect(item.entityId).not.toBe("");
      expect(Number.isFinite(item.world.x)).toBe(true);
      expect(Number.isFinite(item.world.y)).toBe(true);
      expect(Number.isFinite(item.world.z)).toBe(true);
    }
    expect(new Set(items.map((item) => item.id)).size).toBe(items.length);
  });

  it("covers every positioned kind the plant renders", () => {
    const kinds = new Set(build().map((item) => item.kind));
    expect(kinds.has("station")).toBe(true);
    expect(kinds.has("carrier")).toBe(true);
    expect(kinds.has("worker")).toBe(true);
    expect(kinds.has("bay")).toBe(true);
  });

  it("marks truncated worker names and disambiguates duplicate labels", () => {
    const model = scalablePlantFixture({
      stagesPerWorkflow: 1,
      workflowCount: 1,
      workersPerWorkflow: 2,
    });
    model.workers[0].displayName = "Implementation specialist alpha";
    model.workers[1].displayName = "Implementation specialist beta";

    const labels = buildPlantOverlayItems({
      animateTransitions: false,
      layout: buildFactoryPlantLayout(model),
      lens: "world",
      model,
      selection: { kind: "overview" },
    })
      .filter((item) => item.kind === "worker")
      .map((item) => item.label);

    expect(labels).toHaveLength(2);
    expect(new Set(labels).size).toBe(2);
    expect(labels.every((label) => label?.includes("…"))).toBe(true);
  });

  it("is deterministic across builds", () => {
    expect(build().map((item) => item.id)).toEqual(
      build().map((item) => item.id),
    );
  });

  it("keeps ids stable when the selection changes", () => {
    const none = build().map((item) => item.id);
    const { layout, model } = plantFixture();
    const station = model.stations[0];
    const selected = buildPlantOverlayItems({
      animateTransitions: false,
      layout,
      lens: "world",
      model,
      selection: { id: station.id, kind: "station" },
    }).map((item) => item.id);
    expect(selected).toEqual(none);
  });

  it("marks the selected item critical and promotes its tier", () => {
    const { layout, model } = plantFixture();
    const station = model.stations[0];
    const items = buildPlantOverlayItems({
      animateTransitions: false,
      layout,
      lens: "world",
      model,
      selection: { id: station.id, kind: "station" },
    });
    const selected = items.filter((item) => item.selected);
    expect(selected.length).toBeGreaterThan(0);
    for (const item of selected) {
      expect(item.critical).toBe(true);
      expect(item.tier).toBe("selected");
      expect(PLANT_LABEL_TIER_PRIORITY[item.tier]).toBe(
        PLANT_LABEL_TIER_PRIORITY.selected,
      );
    }
  });

  it("marks the focused item without claiming it is selected", () => {
    const { layout, model } = plantFixture();
    const base = buildPlantOverlayItems({
      animateTransitions: false,
      layout,
      lens: "world",
      model,
      selection: { kind: "overview" },
    });
    const target = base.find((item) => item.kind === "station");
    expect(target).toBeDefined();
    const items = buildPlantOverlayItems({
      animateTransitions: false,
      focusId: target!.id,
      layout,
      lens: "world",
      model,
      selection: { kind: "overview" },
    });
    const focused = items.find((item) => item.id === target!.id);
    expect(focused?.focused).toBe(true);
    expect(focused?.selected).toBe(false);
    expect(focused?.critical).toBe(true);
    expect(focused?.tier).toBe("focused");
  });

  it("gives every item an accessible name", () => {
    for (const item of build()) {
      expect(item.ariaLabel.length).toBeGreaterThan(0);
    }
  });

  it("never sizes a hit target below the desktop minimum", () => {
    for (const item of build()) {
      expect(item.hit.width).toBeGreaterThanOrEqual(PLANT_HIT_TARGET_MIN);
      expect(item.hit.height).toBeGreaterThanOrEqual(PLANT_HIT_TARGET_MIN);
    }
  });

  it("groups items so a collapse reports into a real aggregate", () => {
    for (const item of build()) {
      expect(item.groupId.length).toBeGreaterThan(0);
    }
  });

  it("carries a truthful selection for every item", () => {
    for (const item of build()) {
      expect(item.selection.kind).not.toBe("overview");
    }
  });

  it("emits overflow affordances when a bay holds more than it can show", () => {
    const model = scalablePlantFixture({
      carriersPerWorkflow: 2,
      stagesPerWorkflow: 3,
      workersPerWorkflow: 2,
      workflowCount: 2,
    });
    // Truncation is a fact the read reports, so it is set on the model rather
    // than inferred from geometry.
    for (const lane of model.lanes) {
      lane.yard.overflowRunCount = 4;
      lane.stations[0].overflowRunCount = 3;
      lane.stations[0].workerOverflowCount = 2;
    }
    const layout = buildFactoryPlantLayout(model);
    const items = buildPlantOverlayItems({
      animateTransitions: false,
      layout,
      lens: "world",
      model,
      selection: { kind: "overview" },
    });
    const overflow = items.filter((item) => item.kind === "overflow");
    expect(overflow.length).toBeGreaterThanOrEqual(6);
    for (const item of overflow) {
      expect(item.label).toMatch(/^\+\d+/);
      expect(item.ariaLabel.length).toBeGreaterThan(0);
    }
    const kinds = new Set(
      overflow.map((item) =>
        item.data.kind === "overflow" ? item.data.overflow : undefined,
      ),
    );
    expect(kinds.has("queued")).toBe(true);
    expect(kinds.has("runs")).toBe(true);
    expect(kinds.has("staff")).toBe(true);
  });

  it("scales to a large plant without duplicating anchors", () => {
    const model = scalablePlantFixture({
      carriersPerWorkflow: 2,
      stagesPerWorkflow: 6,
      workersPerWorkflow: 1,
      workflowCount: 12,
    });
    const layout = buildFactoryPlantLayout(model);
    const items = buildPlantOverlayItems({
      animateTransitions: false,
      layout,
      lens: "world",
      model,
      selection: { kind: "overview" },
    });
    expect(items.length).toBeGreaterThan(50);
    expect(new Set(items.map((item) => item.id)).size).toBe(items.length);
  });
});

describe("findPlantOverlayAnchorId", () => {
  it("finds the anchor for the current selection", () => {
    const { layout, model } = plantFixture();
    const station = model.stations[0];
    const selection = { id: station.id, kind: "station" } as const;
    const items = buildPlantOverlayItems({
      animateTransitions: false,
      layout,
      lens: "world",
      model,
      selection,
    });
    const anchorId = findPlantOverlayAnchorId(items, selection);
    expect(anchorId).toBeDefined();
    expect(items.find((item) => item.id === anchorId)?.selected).toBe(true);
  });

  it("returns undefined when nothing is selected", () => {
    expect(findPlantOverlayAnchorId(build(), { kind: "overview" })).toBeUndefined();
  });
});
