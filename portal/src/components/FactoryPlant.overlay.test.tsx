import { act } from "react";
import { cleanup, fireEvent, render, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { FactorySelection } from "../factorySelection";
import {
  FACTORY_INSPECTOR_WIDTH,
  factoryViewportSafeArea,
} from "../factoryViewportSafeArea";
import { plantFixture, scalablePlantFixture } from "../test/plantFixtures";
import type { FactoryFloorModel, FactoryLens } from "../factoryModel";
import { FactoryPlant } from "./FactoryPlant";
import { FactoryViewport } from "./FactoryViewport";
import type {
  FactoryWebGLRuntimeOptions,
  PlantRenderer,
  PlantRuntimeHost,
} from "./plant/factoryWebGLRuntime";
import { createFactoryWebGLRuntime } from "./plant/factoryWebGLRuntime";

/**
 * The renderer and the frame clock are injected, so "the overlay uses the live
 * camera" and "the fallback keeps classic coordinates" are provable in jsdom,
 * where there is no GPU to ask.
 */
function createFakeRenderer(): PlantRenderer {
  return {
    dispose: () => {},
    getContext: () =>
      ({ getExtension: () => null }) as unknown as WebGL2RenderingContext,
    info: {
      memory: { geometries: 0, textures: 0 },
      programs: { length: 0 },
      render: { calls: 0, triangles: 0 },
    },
    render: () => {},
    setPixelRatio: () => {},
    setSize: () => {},
  };
}

function createImmediateHost(): PlantRuntimeHost {
  let handle = 1;
  return {
    cancelAnimationFrame: () => {},
    now: () => 0,
    observeDocumentVisibility: () => () => {},
    observeIntersection: (_target, callback) => {
      callback(true);
      return () => {};
    },
    observeResize: () => () => {},
    pixelRatio: () => 1,
    requestAnimationFrame: (callback) => {
      const current = handle;
      handle += 1;
      callback(0);
      return current;
    },
  };
}

function webGLRuntime(options: FactoryWebGLRuntimeOptions) {
  return createFactoryWebGLRuntime({
    ...options,
    createRenderer: createFakeRenderer,
    host: createImmediateHost(),
  });
}

function pickingRuntime(key: string) {
  return (options: FactoryWebGLRuntimeOptions) => {
    const runtime = webGLRuntime(options);
    if (!runtime) {
      return undefined;
    }
    return {
      ...runtime,
      pick: () => ({ distance: 1, entity: "station", key }),
    };
  };
}

/** Stands in for a browser without WebGL: the classic bitmap must take over. */
function noRuntime() {
  return undefined;
}

let container: HTMLElement;

function sizeViewport(width: number, height: number) {
  const viewport = container.querySelector(".factory-viewport");
  if (!viewport) {
    return;
  }
  Object.defineProperty(viewport, "clientWidth", {
    configurable: true,
    value: width,
  });
  Object.defineProperty(viewport, "clientHeight", {
    configurable: true,
    value: height,
  });
}

async function renderPlant({
  createRuntime,
  inspectorOpen = false,
  lens = "world",
  loadRenderer,
  model: suppliedModel,
  onSelect = () => {},
  selection = { kind: "overview" } as FactorySelection,
  waitForCanvas = true,
}: {
  createRuntime?: (
    options: FactoryWebGLRuntimeOptions,
  ) => ReturnType<typeof createFactoryWebGLRuntime>;
  inspectorOpen?: boolean;
  lens?: FactoryLens;
  loadRenderer?: React.ComponentProps<typeof FactoryPlant>["loadRenderer"];
  model?: FactoryFloorModel;
  onSelect?: (next: FactorySelection) => void;
  selection?: FactorySelection;
  waitForCanvas?: boolean;
} = {}) {
  cleanup();
  const model = suppliedModel ?? plantFixture().model;
  let result: ReturnType<typeof render> | undefined;
  await act(async () => {
    result = render(
      <FactoryViewport
        inspectorOpen={inspectorOpen}
        label="Factory plant"
        worldHeight={950}
        worldWidth={1450}
      >
        <FactoryPlant
          animateTransitions={false}
          {...(createRuntime ? { createRuntime } : {})}
          {...(loadRenderer ? { loadRenderer } : {})}
          lens={lens}
          model={model}
          onSelect={onSelect}
          reducedMotion
          selection={selection}
        />
      </FactoryViewport>,
    );
  });
  container = result!.container;
  // The scene is a lazy chunk; until it resolves the Suspense fallback is what
  // is on screen, and asserting against that would prove nothing.
  await act(async () => {
    await Promise.resolve();
  });
  if (waitForCanvas) {
    await waitFor(() => {
      // The canvas only exists once the lazy renderer chunk has resolved; the
      // Suspense fallback would otherwise be what every assertion below reads.
      expect(container.querySelector(".factory-plant-webgl")).not.toBeNull();
    });
  }
  await act(async () => {
    await Promise.resolve();
  });
  return { model };
}

afterEach(() => {
  cleanup();
});

describe("FactoryPlant coordinate source", () => {
  it(
    "hard-caps the semantic overlay when every station is critical",
    async () => {
      const model = scalablePlantFixture({
        stagesPerWorkflow: 20,
        statusAt: () => "blocked",
        workflowCount: 50,
      });

      await renderPlant({ createRuntime: webGLRuntime, lens: "risk", model });

      expect(
        container.querySelectorAll(
          ".factory-plant-overlay-item, .factory-plant-overlay-chip",
        ).length,
      ).toBeLessThanOrEqual(240);
    },
    15_000,
  );

  it("uses the classic bitmap and classic controls without WebGL", async () => {
    await renderPlant({ createRuntime: noRuntime });
    expect(
      container
        .querySelector(".factory-plant-renderer")
        ?.getAttribute("data-webgl"),
    ).toBe("fallback");
    expect(
      container.querySelector(".factory-plant")?.getAttribute("data-projection"),
    ).toBe("classic");
    expect(container.querySelector(".factory-plant-overlay")).toBeNull();
    expect(container.querySelector(".factory-plant-backdrop")).not.toBeNull();
  });

  it("switches to the projected overlay when the renderer is ready", async () => {
    await renderPlant({ createRuntime: webGLRuntime });
    expect(
      container
        .querySelector(".factory-plant-renderer")
        ?.getAttribute("data-webgl"),
    ).toBe("ready");
    expect(
      container.querySelector(".factory-plant")?.getAttribute("data-projection"),
    ).toBe("webgl");
    expect(container.querySelector(".factory-plant-overlay")).not.toBeNull();
  });

  it("never renders both coordinate systems at once", async () => {
    await renderPlant({ createRuntime: webGLRuntime });
    expect(
      container.querySelectorAll(".factory-plant-overlay-item").length,
    ).toBeGreaterThan(0);
    expect(container.querySelectorAll(".factory-station").length).toBe(0);

    await renderPlant({ createRuntime: noRuntime });
    expect(container.querySelectorAll(".factory-plant-overlay-item").length).toBe(
      0,
    );
    expect(
      container.querySelectorAll(".factory-station").length,
    ).toBeGreaterThan(0);
  });

  it("keeps one accessible control per semantic in either mode", async () => {
    await renderPlant({ createRuntime: webGLRuntime });
    const webglNames = [...container.querySelectorAll("button[aria-label]")].map(
      (node) => node.getAttribute("aria-label"),
    );
    expect(new Set(webglNames).size).toBe(webglNames.length);

    await renderPlant({ createRuntime: noRuntime });
    const classicNames = [
      ...container.querySelectorAll("button[aria-label]"),
    ].map((node) => node.getAttribute("aria-label"));
    expect(new Set(classicNames).size).toBe(classicNames.length);
  });
});

describe("FactoryPlant renderer-state switching", () => {
  it("keeps the complete fallback on import rejection and retries the chunk", async () => {
    let attempts = 0;
    const loadRenderer: NonNullable<
      React.ComponentProps<typeof FactoryPlant>["loadRenderer"]
    > = async () => {
      attempts += 1;
      if (attempts === 1) {
        throw new Error("synthetic chunk rejection");
      }
      return import("./FactoryWebGLScene");
    };

    await renderPlant({
      createRuntime: webGLRuntime,
      loadRenderer,
      waitForCanvas: false,
    });
    await waitFor(() => {
      expect(container.querySelector(".factory-plant-renderer-status")).not.toBeNull();
    });
    expect(
      container.querySelectorAll(".factory-station").length,
    ).toBeGreaterThan(0);
    expect(container.querySelector(".factory-plant-backdrop")).not.toBeNull();

    await act(async () => {
      container
        .querySelector<HTMLButtonElement>(".factory-plant-renderer-status button")
        ?.click();
    });
    await waitFor(() => {
      expect(
        container
          .querySelector(".factory-plant-renderer")
          ?.getAttribute("data-webgl"),
      ).toBe("ready");
    });
    expect(attempts).toBe(2);
    expect(container.querySelector(".factory-plant-renderer-status")).toBeNull();
  });

  it("keeps the selection when the coordinate source changes", async () => {
    const { model } = plantFixture();
    const station = model.stations[0];
    const selection: FactorySelection = { id: station.id, kind: "station" };

    await renderPlant({ createRuntime: webGLRuntime, selection });
    const projected = container.querySelector(
      '.factory-plant-overlay-item[data-selected="true"]',
    );
    expect(projected).not.toBeNull();
    expect(projected?.getAttribute("data-plant-probe-id")).toBe(station.id);

    await renderPlant({ createRuntime: noRuntime, selection });
    expect(container.querySelector(".factory-plant-overlay-item")).toBeNull();
    expect(
      container.querySelector('.factory-station[aria-pressed="true"]'),
    ).not.toBeNull();
  });

  it("keeps focus on the same anchor when the source switches back", async () => {
    const { model } = plantFixture();
    const station = model.stations[0];
    const selection: FactorySelection = { id: station.id, kind: "station" };

    await renderPlant({ createRuntime: webGLRuntime, selection });
    const anchorId = container
      .querySelector('.factory-plant-overlay-item[data-selected="true"]')
      ?.getAttribute("data-plant-anchor-id");
    expect(anchorId).toBeTruthy();

    await renderPlant({ createRuntime: noRuntime, selection });
    await renderPlant({ createRuntime: webGLRuntime, selection });
    expect(
      container
        .querySelector('.factory-plant-overlay-item[data-selected="true"]')
        ?.getAttribute("data-plant-anchor-id"),
    ).toBe(anchorId);
  });

  it("reports the same selection from either coordinate source", async () => {
    const { model } = plantFixture();
    const station = model.stations[0];
    const selections: FactorySelection[] = [];

    await renderPlant({
      createRuntime: webGLRuntime,
      onSelect: (next) => selections.push(next),
    });
    const overlayButton = container.querySelector<HTMLButtonElement>(
      `.factory-plant-overlay-item[data-plant-probe-id="${station.id}"]`,
    );
    expect(overlayButton).not.toBeNull();
    await act(async () => {
      overlayButton?.click();
    });

    await renderPlant({
      createRuntime: noRuntime,
      onSelect: (next) => selections.push(next),
    });
    const classicButton = container.querySelector<HTMLButtonElement>(
      ".factory-station",
    );
    expect(classicButton).not.toBeNull();
    await act(async () => {
      classicButton?.click();
    });

    expect(selections).toHaveLength(2);
    expect(selections[0]).toEqual({ id: station.id, kind: "station" });
    expect(selections[1].kind).toBe("station");
  });

  it("uses raycast depth instead of the later overlapping DOM button for pointers", async () => {
    const { model } = plantFixture();
    const visible = model.stations[0];
    const laterDomTarget = model.stations[1];
    const selections: FactorySelection[] = [];
    await renderPlant({
      createRuntime: pickingRuntime(visible.id),
      onSelect: (next) => selections.push(next),
    });
    const scene = container.querySelector<HTMLElement>(".factory-plant-scene")!;
    scene.getBoundingClientRect = () =>
      ({
        bottom: 950,
        height: 950,
        left: 0,
        right: 1450,
        top: 0,
        width: 1450,
        x: 0,
        y: 0,
        toJSON: () => ({}),
      }) as DOMRect;
    const target = container.querySelector<HTMLButtonElement>(
      `.factory-plant-overlay-item[data-plant-probe-id="${laterDomTarget.id}"]`,
    )!;

    fireEvent.pointerDown(target, {
      button: 0,
      clientX: 400,
      clientY: 300,
      pointerId: 1,
    });
    fireEvent.click(target, { clientX: 400, clientY: 300, detail: 1 });
    expect(selections).toEqual([{ id: visible.id, kind: "station" }]);

    // Keyboard/programmatic activation has no pointer depth and remains bound
    // to the focused semantic DOM control.
    target.click();
    expect(selections.at(-1)).toEqual({
      id: laterDomTarget.id,
      kind: "station",
    });
  });

  it("keeps a displaced aggregate chip's own pointer action", async () => {
    const { model } = plantFixture();
    const visible = model.stations[0];
    const selections: FactorySelection[] = [];
    await renderPlant({
      createRuntime: pickingRuntime(visible.id),
      onSelect: (next) => selections.push(next),
    });
    const scene = container.querySelector<HTMLElement>(".factory-plant-scene")!;
    const chip = document.createElement("button");
    chip.className = "factory-plant-overlay-chip";
    chip.addEventListener("click", () => selections.push({ kind: "overview" }));
    scene.append(chip);

    fireEvent.pointerDown(chip, {
      button: 0,
      clientX: 400,
      clientY: 300,
      pointerId: 1,
    });
    fireEvent.click(chip, { clientX: 400, clientY: 300, detail: 1 });

    expect(selections).toEqual([{ kind: "overview" }]);
  });

  it("preserves document.activeElement by semantic anchor across renderer switching", async () => {
    await renderPlant({ createRuntime: webGLRuntime });
    const projected = container.querySelector<HTMLButtonElement>(
      '.factory-plant-overlay-item[data-kind="station"]',
    )!;
    const focusId = projected.dataset.plantFocusId;
    expect(focusId).toBeTruthy();
    act(() => {
      projected.focus();
    });
    expect(document.activeElement).toBe(projected);

    const canvas = container.querySelector("canvas")!;
    act(() => {
      canvas.dispatchEvent(new Event("webglcontextlost", { cancelable: true }));
    });
    await waitFor(() => {
      expect(
        container.querySelector(".factory-plant")?.getAttribute(
          "data-projection",
        ),
      ).toBe("classic");
    });
    const classic = Array.from(
      container.querySelectorAll<HTMLButtonElement>("[data-plant-focus-id]"),
    ).find((element) => element.dataset.plantFocusId === focusId);
    expect(classic).toBeTruthy();
    expect(document.activeElement).toBe(classic);

    act(() => {
      canvas.dispatchEvent(new Event("webglcontextrestored"));
    });
    await waitFor(() => {
      expect(
        container.querySelector(".factory-plant")?.getAttribute(
          "data-projection",
        ),
      ).toBe("webgl");
    });
    const restored = Array.from(
      container.querySelectorAll<HTMLButtonElement>("[data-plant-focus-id]"),
    ).find((element) => element.dataset.plantFocusId === focusId);
    expect(restored).toBeTruthy();
    expect(document.activeElement).toBe(restored);
  });

  it("keeps the outer zoom and pan pose rigid while the WebGL camera stays fitted", async () => {
    let runtime: ReturnType<typeof createFactoryWebGLRuntime>;
    await renderPlant({
      createRuntime: (options) => {
        runtime = webGLRuntime(options);
        return runtime;
      },
    });
    const viewport = container.querySelector<HTMLElement>(".factory-viewport")!;
    const world = container.querySelector<HTMLElement>(
      ".factory-viewport-world",
    )!;
    const revision = runtime!.projection().revision;
    const localAnchor = container.querySelector<HTMLElement>(
      ".factory-plant-overlay-item",
    )!.style.left;
    const initialPose = world.style.transform;

    fireEvent.click(
      container.querySelector<HTMLButtonElement>('[aria-label="Zoom in"]')!,
    );
    const zoomed = world.style.transform;
    expect(zoomed).not.toBe(initialPose);
    expect(runtime!.projection().revision).toBe(revision);
    expect(
      container.querySelector<HTMLElement>(".factory-plant-overlay-item")!
        .style.left,
    ).toBe(localAnchor);

    viewport.setPointerCapture = () => {};
    viewport.releasePointerCapture = () => {};
    fireEvent.pointerDown(viewport, {
      button: 0,
      clientX: 30,
      clientY: 40,
      pointerId: 7,
    });
    fireEvent.pointerMove(viewport, {
      clientX: 70,
      clientY: 65,
      pointerId: 7,
    });
    fireEvent.pointerUp(viewport, {
      clientX: 70,
      clientY: 65,
      pointerId: 7,
    });
    expect(world.style.transform).not.toBe(zoomed);
    expect(runtime!.projection().revision).toBe(revision);

    const pose = world.style.transform;
    act(() => {
      container
        .querySelector("canvas")!
        .dispatchEvent(new Event("webglcontextlost", { cancelable: true }));
    });
    await waitFor(() => {
      expect(
        container.querySelector(".factory-plant")?.getAttribute(
          "data-projection",
        ),
      ).toBe("classic");
    });
    expect(world.style.transform).toBe(pose);
  });
});

describe("FactoryViewport inspector safe area", () => {
  it("fits the world into the unobscured rectangle when the inspector is open", async () => {
    await renderPlant({ createRuntime: noRuntime, inspectorOpen: true });
    sizeViewport(1440, 1000);
    const world = container.querySelector<HTMLElement>(
      ".factory-viewport-world",
    );
    expect(world).not.toBeNull();
    const safe = factoryViewportSafeArea({
      height: 1000,
      inspectorOpen: true,
      width: 1440,
    });
    expect(safe.width).toBeLessThan(1440 - FACTORY_INSPECTOR_WIDTH / 2);
  });

  it("marks the stage as inspector-aware only while the inspector is open", async () => {
    await renderPlant({ createRuntime: noRuntime, inspectorOpen: true });
    expect(
      container
        .querySelector(".factory-viewport")
        ?.getAttribute("data-inspector"),
    ).toBe("open");

    await renderPlant({ createRuntime: noRuntime, inspectorOpen: false });
    expect(
      container
        .querySelector(".factory-viewport")
        ?.getAttribute("data-inspector"),
    ).toBe("closed");
  });
});
