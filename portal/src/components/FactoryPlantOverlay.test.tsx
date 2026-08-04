import { act, cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import {
  buildPlantOverlayItems,
  type PlantOverlayItem,
} from "../factoryPlantOverlay";
import { buildFactoryPlantLayout } from "../factoryPlantLayout";
import {
  PLANT_HIT_TARGET_MIN,
  PLANT_TOUCH_HIT_TARGET_MIN,
} from "../plantLabelPacking";
import {
  PLANT_IDENTITY_MATRIX,
  createPlantProjector,
  plantPointToViewport,
  plantRectsOverlap,
  plantScreenRect,
  type PlantAnimatedProjection,
  type PlantProjectedPoint,
  type PlantProjectionController,
  type PlantProjectionState,
  type PlantViewProjectionMatrix,
} from "../plantProjection";
import { plantFixture, scalablePlantFixture } from "../test/plantFixtures";
import {
  FactoryPlantOverlay,
  packOverlay,
  resolvePlantChipAction,
} from "./FactoryPlantOverlay";

/**
 * A plan-view matrix fitted to the items, standing in for the runtime camera.
 *
 * The identity matrix would leave the plant off-canvas: the world spans tens of
 * units and NDC only reaches one. Fitting keeps these tests about packing and
 * occlusion rather than about arithmetic that the projection tests already own.
 */
function planViewMatrix(
  items: readonly PlantOverlayItem[],
): PlantViewProjectionMatrix {
  const xs = items.map((item) => item.world.x);
  const zs = items.map((item) => item.world.z);
  const spanX = Math.max(1, Math.max(...xs) - Math.min(...xs));
  const spanZ = Math.max(1, Math.max(...zs) - Math.min(...zs));
  const midX = (Math.max(...xs) + Math.min(...xs)) / 2;
  const midZ = (Math.max(...zs) + Math.min(...zs)) / 2;
  const scaleX = 1.6 / spanX;
  const scaleZ = 1.6 / spanZ;
  const matrix = new Array(16).fill(0);
  matrix[0] = scaleX;
  matrix[9] = -scaleZ;
  matrix[6] = 0.01;
  matrix[12] = -midX * scaleX;
  matrix[13] = midZ * scaleZ;
  matrix[15] = 1;
  return matrix;
}

function projectionState(
  items: readonly PlantOverlayItem[],
  overrides: Partial<PlantProjectionState> = {},
): PlantProjectionState {
  const canvas = plantScreenRect(0, 0, 1450, 950);
  return {
    canvas,
    matrix: planViewMatrix(items),
    revision: 1,
    safeArea: canvas,
    source: "webgl",
    ...overrides,
  };
}

function overlayItems(selection = { kind: "overview" } as const) {
  const { layout, model } = plantFixture();
  return buildPlantOverlayItems({
    animateTransitions: false,
    layout,
    lens: "world",
    model,
    selection,
  });
}

function animationController(
  state: PlantProjectionState,
): {
  controller: PlantProjectionController;
  emit: (entry: PlantAnimatedProjection) => void;
  setPoint: (id: string, point: PlantProjectedPoint) => void;
} {
  const projector = createPlantProjector(state);
  const points = new Map<string, PlantProjectedPoint>();
  const listeners = new Set<
    (entries: readonly PlantAnimatedProjection[]) => void
  >();
  return {
    controller: {
      pick: () => undefined,
      project: projector.project,
      projectEntity: (id, point) => points.get(id) ?? projector.project(point),
      projection: () => state,
      subscribe: () => () => {},
      subscribeAnimation: (listener) => {
        listeners.add(listener);
        return () => listeners.delete(listener);
      },
    },
    emit: (entry) => {
      for (const listener of listeners) {
        listener([entry]);
      }
    },
    setPoint: (id, point) => {
      points.set(id, point);
    },
  };
}

afterEach(() => {
  cleanup();
});

describe("packOverlay", () => {
  it("projects every item through the supplied projection", () => {
    const items = overlayItems();
    const packed = packOverlay({
      items,
      projection: projectionState(items),
      touch: false,
    });
    expect(packed.entries.length).toBeGreaterThan(0);
    expect(packed.rendered.length).toBe(items.length);
    for (const entry of packed.entries) {
      expect(Number.isFinite(entry.projected.x)).toBe(true);
      expect(Number.isFinite(entry.projected.y)).toBe(true);
    }
  });

  it("reports no drift, because the DOM anchor is the projected point", () => {
    const items = overlayItems();
    const packed = packOverlay({
      items,
      projection: projectionState(items),
      touch: false,
    });
    expect(packed.drift.max).toBe(0);
    expect(packed.drift.mean).toBe(0);
  });

  it("moves every anchor when the projection changes", () => {
    const items = overlayItems();
    const identity = packOverlay({
      items,
      projection: projectionState(items),
      touch: false,
    });
    const shifted = [...PLANT_IDENTITY_MATRIX];
    shifted[12] = 0.5;
    const moved = packOverlay({
      items,
      projection: projectionState(items, { matrix: shifted, revision: 2 }),
      touch: false,
    });
    const before = new Map(
      identity.entries.map((entry) => [entry.id, entry.projected.x]),
    );
    let changed = 0;
    for (const entry of moved.entries) {
      const previous = before.get(entry.id);
      if (previous !== undefined && Math.abs(previous - entry.projected.x) > 1) {
        changed += 1;
      }
    }
    expect(changed).toBe(moved.entries.length);
  });

  it("keeps hit targets at the desktop minimum", () => {
    const items = overlayItems();
    const packed = packOverlay({
      items,
      projection: projectionState(items),
      touch: false,
    });
    expect(packed.hitTargets.belowMinimum).toBe(0);
    expect(packed.hitTargets.minWidth).toBeGreaterThanOrEqual(
      PLANT_HIT_TARGET_MIN,
    );
    expect(packed.hitTargets.minHeight).toBeGreaterThanOrEqual(
      PLANT_HIT_TARGET_MIN,
    );
  });

  it("grows hit targets to the touch minimum in touch mode", () => {
    const items = overlayItems();
    const packed = packOverlay({
      items,
      projection: projectionState(items),
      touch: true,
    });
    expect(packed.hitTargets.belowMinimum).toBe(0);
    expect(packed.hitTargets.minWidth).toBeGreaterThanOrEqual(
      PLANT_TOUCH_HIT_TARGET_MIN,
    );
    expect(packed.hitTargets.minHeight).toBeGreaterThanOrEqual(
      PLANT_TOUCH_HIT_TARGET_MIN,
    );
  });

  it("leaves no overlapping or clipped labels", () => {
    const items = overlayItems();
    const packed = packOverlay({
      items,
      projection: projectionState(items),
      touch: false,
    });
    expect(packed.metrics.overlaps).toBe(0);
    expect(packed.metrics.clipped).toBe(0);
  });

  it("never collapses a critical label", () => {
    const { layout, model } = plantFixture();
    const station = model.stations[0];
    const items = buildPlantOverlayItems({
      animateTransitions: false,
      layout,
      lens: "world",
      model,
      selection: { id: station.id, kind: "station" },
    });
    // A safe rectangle too small for the labels forces the collapse path.
    const packed = packOverlay({
      items,
      projection: projectionState(items, { safeArea: plantScreenRect(400, 300, 90, 60) }),
      touch: false,
    });
    const critical = items.filter((item) => item.critical).map((item) => item.id);
    for (const id of critical) {
      expect(packed.chips.some((chip) => chip.ids.includes(id))).toBe(false);
    }
  });

  it("reports collapsed labels as a truthful aggregate", () => {
    const items = overlayItems();
    const packed = packOverlay({
      items,
      projection: projectionState(items, {
        safeArea: plantScreenRect(300, 200, 200, 140),
      }),
      touch: false,
    });

    const chipped = packed.chips.reduce((sum, chip) => sum + chip.count, 0);
    expect(chipped).toBe(packed.metrics.collapsed);
    for (const chip of packed.chips) {
      expect(chip.count).toBe(chip.ids.length);
    }
  });

  it("keeps every capped critical label in a truthful canonical aggregate", () => {
    const model = scalablePlantFixture({
      stagesPerWorkflow: 8,
      statusAt: () => "blocked",
      workflowCount: 125,
    });
    const layout = buildFactoryPlantLayout(model);
    const items = buildPlantOverlayItems({
      animateTransitions: false,
      layout,
      lens: "risk",
      model,
      selection: { kind: "overview" },
    });
    const packed = packOverlay({
      items,
      maxControls: 240,
      projection: projectionState(items, {
        safeArea: plantScreenRect(0, 0, 656, 484),
      }),
      touch: false,
    });

    expect(packed.rendered.length + packed.chips.length).toBeLessThanOrEqual(240);
    expect(
      packed.chips.reduce((sum, chip) => sum + chip.count, 0),
    ).toBe(packed.metrics.collapsed);
    expect(packed.metrics.overlaps).toBe(0);
    for (const chip of packed.chips) {
      const action = packed.chipActions.get(chip.id);
      if (
        action?.selection.kind === "lane" ||
        action?.selection.kind === "station" ||
        action?.selection.kind === "run" ||
        action?.selection.kind === "worker"
      ) {
        expect(action.focusId).toBeTruthy();
        expect(action.focusId).not.toMatch(/^chip:/);
      }
    }
  }, 15_000);

  it("keeps packed labels clear of one another", () => {
    const items = overlayItems();
    const packed = packOverlay({
      items,
      projection: projectionState(items),
      touch: false,
    });
    const rects = packed.rendered
      .filter((entry) => entry.placement !== undefined)
      .map((entry) => entry.placement!.rect);
    expect(rects.length).toBeGreaterThan(0);
    for (let left = 0; left < rects.length; left += 1) {
      for (let right = left + 1; right < rects.length; right += 1) {
        expect(plantRectsOverlap(rects[left], rects[right])).toBe(false);
      }
    }
  });

  it("counts nothing as occluded when the inspector is closed", () => {
    const items = overlayItems();
    const packed = packOverlay({
      items,
      projection: projectionState(items),
      touch: false,
    });
    expect(packed.occlusion).toEqual({ critical: 0, selected: 0, total: 0 });
  });

  it("counts occlusion honestly when the inspector covers the stage", () => {
    const items = overlayItems();
    const packed = packOverlay({
      inspectorRect: plantScreenRect(0, 0, 1450, 950),
      items,
      projection: projectionState(items),
      touch: false,
    });
    expect(packed.occlusion.total).toBeGreaterThan(0);
  });

  it("never leaves the selected anchor under the inspector after a safe-area fit", () => {
    const { layout, model } = plantFixture();
    const station = model.stations[0];
    const items = buildPlantOverlayItems({
      animateTransitions: false,
      layout,
      lens: "world",
      model,
      selection: { id: station.id, kind: "station" },
    });
    // The safe area excludes the inspector, exactly as the camera fits it.
    const packed = packOverlay({
      inspectorRect: plantScreenRect(1100, 0, 350, 950),
      items,
      projection: projectionState(items, {
        safeArea: plantScreenRect(0, 0, 1080, 950),
      }),
      touch: false,
    });
    expect(packed.occlusion.selected).toBe(0);
    expect(packed.occlusion.critical).toBe(0);
  });

  it("drops items the camera cannot see rather than pinning them to an edge", () => {
    const items = overlayItems();
    const packed = packOverlay({
      items,
      projection: projectionState(items, {
        safeArea: plantScreenRect(0, 0, 20, 20),
      }),
      touch: false,
    });
    expect(packed.metrics.offscreen).toBeGreaterThan(0);
  });

  it("is deterministic for the same projection and items", () => {
    const items = overlayItems();
    const first = packOverlay({
      items,
      projection: projectionState(items),
      touch: false,
    });
    const second = packOverlay({
      items,
      projection: projectionState(items),
      touch: false,
    });
    expect(second.entries).toEqual(first.entries);
    expect(second.chips).toEqual(first.chips);
  });

  it("marks the projection source on every entry set it packs", () => {
    const items = overlayItems();
    const classic = packOverlay({
      items,
      projection: projectionState(items, { source: "classic" }),
      touch: false,
    });
    expect(classic.entries.length).toBeGreaterThan(0);
  });

  it.each([0.6, 0.8, 1, 1.25, 2])(
    "packs in final CSS viewport coordinates at zoom %s",
    (zoom) => {
      const items = overlayItems();
      const projection = projectionState(items);
      const viewport = {
        safeRect: plantScreenRect(37, 23, projection.canvas.width * zoom, projection.canvas.height * zoom),
        x: 37,
        y: 23,
        zoom,
      };
      const packed = packOverlay({
        items,
        projection,
        touch: false,
        viewport,
      });
      const first = items[0];
      const local = createPlantProjector(projection).project(first.world);
      const expected = plantPointToViewport(local, viewport);
      const entry = packed.entries.find((candidate) => candidate.id === first.id);

      expect(entry?.projected.x).toBeCloseTo(expected.x, 6);
      expect(entry?.projected.y).toBeCloseTo(expected.y, 6);
      expect(packed.metrics.overlaps).toBe(0);
      expect(packed.metrics.clipped).toBe(0);
      expect(packed.hitTargets.belowMinimum).toBe(0);
      expect(packed.hitTargets.minWidth).toBeGreaterThanOrEqual(
        PLANT_HIT_TARGET_MIN,
      );
    },
  );

  it("derives chip actions from the hidden aggregate and sizes the chip as a control", () => {
    const items = overlayItems();
    const laneGroup = items.find((item) => item.groupId.startsWith("bay:"))!
      .groupId;
    const laneItems = items
      .filter((item) => item.groupId === laneGroup && item.label)
      .slice(0, 2);
    const laneId = laneGroup.slice("bay:".length);
    const laneAction = resolvePlantChipAction(
      {
        count: laneItems.length,
        groupId: laneGroup,
        ids: laneItems.map((item) => item.id),
      },
      items,
    );
    expect(laneAction.selection).toEqual({ id: laneId, kind: "lane" });
    expect(laneAction.ariaLabel).toContain(`Select workflow line ${laneId}.`);

    for (const touch of [false, true]) {
      const packed = packOverlay({
        items,
        projection: projectionState(items, {
          safeArea: plantScreenRect(300, 200, 200, 140),
        }),
        touch,
      });
      expect(packed.chips.length).toBeGreaterThan(0);
      const minimum = touch
        ? PLANT_TOUCH_HIT_TARGET_MIN
        : PLANT_HIT_TARGET_MIN;
      for (const chip of packed.chips) {
        expect(chip.rect.width).toBeGreaterThanOrEqual(minimum);
        expect(chip.rect.height).toBeGreaterThanOrEqual(minimum);
        const action = packed.chipActions.get(chip.id);
        expect(action?.ariaLabel).toContain(
          action?.selection.kind === "overview"
            ? "Select the factory overview."
            : "Select",
        );
      }
    }
  });

  it("renders each chip with an accessible name that describes its aggregate action", () => {
    const items = overlayItems();
    const projection = projectionState(items, {
      safeArea: plantScreenRect(300, 200, 200, 140),
    });
    const harness = animationController(projection);
    const view = render(
      <FactoryPlantOverlay
        animateTransitions={false}
        controller={harness.controller}
        inspectorOpen={false}
        items={items}
        onFocus={() => {}}
        onSelect={() => {}}
        scale={1}
        touch
        viewport={{
          safeRect: projection.safeArea,
          x: 0,
          y: 0,
          zoom: 1,
        }}
      />,
    );
    const chips = Array.from(
      view.container.querySelectorAll<HTMLButtonElement>(
        ".factory-plant-overlay-chip",
      ),
    );
    expect(chips.length).toBeGreaterThan(0);
    for (const chip of chips) {
      expect(chip.dataset.plantFocusId).toBeTruthy();
      if (chip.dataset.selectionKind !== "overview") {
        expect(chip.dataset.plantFocusId).not.toMatch(/^chip:/);
      }
      expect(Number.parseFloat(chip.style.width)).toBeGreaterThanOrEqual(
        PLANT_TOUCH_HIT_TARGET_MIN,
      );
      expect(Number.parseFloat(chip.style.height)).toBeGreaterThanOrEqual(
        PLANT_TOUCH_HIT_TARGET_MIN,
      );
      const action = chip.dataset.selectionKind;
      const name = chip.getAttribute("aria-label") ?? "";
      if (action === "lane") {
        expect(name).toContain("Select workflow line");
      } else if (action === "station") {
        expect(name).toContain("Select stage");
      } else {
        expect(name).toContain("Select the factory overview");
      }
    }
  });

  it("moves a carrier DOM anchor from animation projections without a React render", () => {
    const items = overlayItems();
    const carrier = items.find((item) => item.kind === "carrier")!;
    const projection = projectionState(items);
    const harness = animationController(projection);
    const initial = harness.controller.projectEntity(
      carrier.entityId,
      carrier.world,
    );
    const view = render(
      <FactoryPlantOverlay
        animateTransitions
        controller={harness.controller}
        inspectorOpen={false}
        items={[carrier]}
        onFocus={() => {}}
        onSelect={() => {}}
        scale={1}
        touch={false}
        viewport={{
          safeRect: projection.safeArea,
          x: 0,
          y: 0,
          zoom: 1,
        }}
      />,
    );
    const button = view.container.querySelector<HTMLButtonElement>(
      ".factory-plant-overlay-item",
    )!;
    expect(button.style.left).toBe(`${initial.x}px`);
    const moved = { ...initial, x: initial.x + 73, y: initial.y - 19 };
    harness.setPoint(carrier.entityId, moved);

    act(() => {
      harness.emit({ id: carrier.entityId, point: moved });
    });

    expect(
      view.container.querySelector(".factory-plant-overlay-item"),
    ).toBe(button);
    expect(button.style.left).toBe(`${moved.x}px`);
    expect(button.style.top).toBe(`${moved.y}px`);
    expect(
      view.container.querySelector(".factory-plant-overlay")?.getAttribute(
        "data-revision",
      ),
    ).toBe(String(projection.revision));
  });
});
