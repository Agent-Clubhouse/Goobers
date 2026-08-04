import { describe, expect, it } from "vitest";
import {
  createPlantProbeController,
  summarizePlantProjections,
} from "./plantProbe";
import { plantProbeRequested } from "./plantProbeSink";
import { PLANT_READ_CURRENT } from "./plantRisk";

describe("Plant probe gate", () => {
  it("requires an exact opt-in query value", () => {
    expect(plantProbeRequested("?plant-probe=1")).toBe(true);
    expect(plantProbeRequested("other=1&plant-probe=1")).toBe(true);
    expect(plantProbeRequested("?plant-probe=true")).toBe(false);
    expect(plantProbeRequested("?plant-probe=0")).toBe(false);
    expect(plantProbeRequested("")).toBe(false);
  });
});

describe("Plant probe counters", () => {
  it("tracks frames, renderer lifecycle, and reset deltas", async () => {
    const controller = createPlantProbeController();
    const canvas = document.createElement("canvas");
    const context = {
      getExtension: () => null,
    } as unknown as WebGLRenderingContext;

    controller.sink.rendererCreated({ canvas, context });
    controller.sink.sceneBuilt();
    controller.sink.motion(true, 7);
    controller.sink.rafScheduled();
    const frame = controller.api.waitForFrames();
    controller.sink.frame({
      raf: true,
      canvas: {
        cssWidth: 100,
        cssHeight: 50,
        renderedWidth: 80,
        renderedHeight: 40,
        backingWidth: 200,
        backingHeight: 100,
        devicePixelRatio: 2,
      },
      info: {
        calls: 9,
        triangles: 42,
        programs: 3,
        geometries: 5,
        textures: 2,
      },
    });

    expect((await frame).animation.frames).toBe(1);
    expect(controller.api.snapshot()).toMatchObject({
      renderer: {
        contexts: 1,
        activeContexts: 1,
        info: { calls: 9, triangles: 42 },
      },
      scene: { builds: 1, disposals: 0 },
      animation: {
        frames: 1,
        rafCallbacks: 1,
        rafRequests: 1,
        motion: true,
        animatedCount: 7,
      },
    });

    expect(controller.api.reset()).toMatchObject({
      renderer: { contexts: 0, activeContexts: 1, disposals: 0 },
      scene: { builds: 0, disposals: 0 },
      animation: { frames: 0, rafCallbacks: 0, rafRequests: 0 },
    });

    controller.sink.sceneDisposed();
    controller.sink.rendererDisposed(context);
    expect(controller.api.snapshot()).toMatchObject({
      renderer: { activeContexts: 0, disposals: 1 },
      scene: { disposals: 1 },
    });
    controller.dispose();
  });

  it("keeps a shared canvas context active while a replacement renderer owns it", () => {
    const controller = createPlantProbeController();
    const canvas = document.createElement("canvas");
    const context = {
      getExtension: () => null,
    } as unknown as WebGLRenderingContext;

    controller.sink.rendererCreated({ canvas, context });
    controller.sink.rendererCreated({ canvas, context });
    controller.sink.rendererDisposed(context);

    expect(controller.api.snapshot().renderer.activeContexts).toBe(1);
    controller.sink.rendererDisposed(context);
    expect(controller.api.snapshot().renderer.activeContexts).toBe(0);
    controller.dispose();
  });

  it("totals keyed reconciliation work and model generations", () => {
    const controller = createPlantProbeController();

    controller.sink.entities({ created: 12, live: 12, removed: 0, replaced: 0, updated: 0 });
    controller.sink.entities({ created: 1, live: 12, removed: 1, replaced: 0, updated: 11 });
    controller.sink.model({
      counts: {
        activeRuns: 1,
        blockedRuns: 0,
        blockedStages: 0,
        carriers: 1,
        gaggles: 1,
        heldStages: 0,
        lanes: 1,
        renderedCarriers: 1,
        renderedWorkers: 1,
        stations: 3,
        unreadRuns: 0,
        workers: 1,
        workflows: 1,
      },
      lens: "world",
      reducedMotion: false,
      theme: "light",
      working: true,
      freshness: "live",
      forcedColors: false,
      readState: PLANT_READ_CURRENT,
      risk: {
        allClear: true,
        complete: true,
        confirmed: 0,
        detail: "",
        headline: "No confirmed current risk",
        level: "healthy",
        unknownCarriers: 0,
        unknownStations: 0,
      },
    });
    controller.sink.layout({
      counts: {
        workflows: 1,
        bayCells: 1,
        stations: 3,
        tracks: 3,
        trackSegments: 3,
        carriers: 1,
        workers: 1,
        batches: 8,
        instances: 12,
      },
      bounds: {
        world: {
          minX: -4,
          minY: -0.5,
          minZ: -4,
          maxX: 36,
          maxY: 4.8,
          maxZ: 36,
          width: 40,
          height: 5.3,
          depth: 40,
        },
        projected: {
          minX: -20,
          minY: -12,
          maxX: 20,
          maxY: 12,
          width: 40,
          height: 24,
        },
      },
      collisions: {
        bayCells: 0,
        machines: 0,
        duplicateStationCoordinates: 0,
      },
      unresolvedTracks: 0,
      boundsFinite: true,
      drawCalls: {
        instancedPlan: 16,
        currentRendererUpperBound: 18,
        actual: 14,
      },
      dom: {
        detailCandidates: 5,
        detailLimit: 240,
        baySummaries: 1,
        overview: 1,
        maxAtAnyLod: 5,
      },
    });

    expect(controller.api.snapshot()).toMatchObject({
      entities: { created: 13, live: 12, reconciles: 2, removed: 1, updated: 11 },
      modelUpdates: 1,
      layout: {
        counts: { workflows: 1, stations: 3, batches: 8 },
        collisions: { bayCells: 0, machines: 0 },
        drawCalls: { actual: 14 },
      },
    });

    // A reset measures the next window, but the live entity count is a fact
    // about the retained scene, not a counter.
    expect(controller.api.reset()).toMatchObject({
      entities: { created: 0, live: 12, reconciles: 0, removed: 0, updated: 0 },
      modelUpdates: 0,
    });
    controller.dispose();
  });

  it("keeps the context-loss extension it captured while the context was alive", () => {
    const controller = createPlantProbeController();
    const canvas = document.createElement("canvas");
    let lost = false;
    const extension = {
      loseContext: () => {
        lost = true;
      },
      restoreContext: () => {
        lost = false;
      },
    };
    const context = {
      // A lost context returns null from getExtension, which is exactly how a
      // probe that looks the extension up on demand loses the ability to
      // restore what it just lost.
      getExtension: (name: string) => (lost || name !== "WEBGL_lose_context" ? null : extension),
    } as unknown as WebGLRenderingContext;

    controller.sink.rendererCreated({ canvas, context });
    expect(controller.api.loseContext()).toBe(true);
    expect(lost).toBe(true);
    expect(controller.api.restoreContext()).toBe(true);
    expect(lost).toBe(false);
    controller.dispose();
  });

  it("reports a browser without the loss extension instead of pretending", () => {
    const controller = createPlantProbeController();
    const canvas = document.createElement("canvas");
    const context = {
      getExtension: () => null,
    } as unknown as WebGLRenderingContext;

    controller.sink.rendererCreated({ canvas, context });
    expect(controller.api.loseContext()).toBe(false);
    expect(controller.api.restoreContext()).toBe(false);
    controller.dispose();
  });

  it("re-runs registered measurements on demand and stops after disposal", () => {
    const controller = createPlantProbeController();
    let drift = 800;
    const stop = controller.sink.registerMeasure(() => {
      controller.sink.projections([
        {
          id: "station:a",
          kind: "station",
          expected: { x: 0, y: 0 },
          actual: { x: drift, y: 0 },
          drift: { x: drift, y: 0, distance: drift },
        },
      ]);
    });

    expect(controller.api.snapshot().projection.maxDrift).toBe(0);
    expect(controller.api.remeasure().projection.maxDrift).toBe(800);

    // A settled page reports the settled truth, not whatever the last
    // camera change happened to leave behind.
    drift = 0.02;
    expect(controller.api.remeasure().projection.maxDrift).toBeCloseTo(0.02);

    stop();
    drift = 900;
    expect(controller.api.remeasure().projection.maxDrift).toBeCloseTo(0.02);
    controller.dispose();
  });

  it("applies exact outer camera poses only while a viewport is registered", () => {
    const controller = createPlantProbeController();
    const poses: Array<{ x: number; y: number; zoom: number }> = [];
    expect(
      controller.api.setViewportCamera({ x: 10, y: 20, zoom: 0.8 }),
    ).toBe(false);
    controller.sink.viewportControl?.({
      setCamera: (pose) => poses.push(pose),
    });
    expect(
      controller.api.setViewportCamera({ x: 10, y: 20, zoom: 0.8 }),
    ).toBe(true);
    expect(poses).toEqual([{ x: 10, y: 20, zoom: 0.8 }]);
    controller.sink.viewportControl?.(undefined);
    expect(
      controller.api.setViewportCamera({ x: 0, y: 0, zoom: 1 }),
    ).toBe(false);
    controller.dispose();
  });
});

describe("Plant projection summary", () => {
  it("computes mean and maximum drift without mutating entries", () => {
    const entries = [
      {
        id: "station:a",
        kind: "station" as const,
        expected: { x: 10, y: 10 },
        actual: { x: 13, y: 14 },
        drift: { x: 3, y: 4, distance: 5 },
      },
      {
        id: "run:b",
        kind: "carrier" as const,
        expected: { x: 20, y: 20 },
        actual: { x: 20, y: 20 },
        drift: { x: 0, y: 0, distance: 0 },
      },
    ];

    expect(summarizePlantProjections(entries)).toMatchObject({
      maxDrift: 5,
      meanDrift: 2.5,
      entries,
    });
  });
});
