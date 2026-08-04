import * as THREE from "three";
import { beforeEach, describe, expect, it, vi } from "vitest";

import {
  createFactoryWebGLRuntime,
  type FactoryWebGLRuntimeUpdate,
  type PlantRenderer,
  type PlantRuntimeHost,
} from "./factoryWebGLRuntime";
import { plantEntityRegistryKey } from "./factoryPlantEntities";
import {
  plantFixture,
  plantFixtureLayout,
  plantStageChangeFixture,
  scalablePlantFixture,
} from "../../test/plantFixtures";
import type {
  PlantEntityStats,
  PlantProbeSink,
  PlantProjectionEntry,
} from "../../plantProbeSink";
import { createPlantProjector, plantScreenRect } from "../../plantProjection";
import type { FactoryLens } from "../../factoryModel";

/**
 * The runtime is exercised without a GPU.
 *
 * Everything the runtime does not own — the renderer, the frame clock, the
 * observers — is injected, which is what makes "one renderer for the life of
 * the canvas" and "one animation frame" provable instead of assumed.
 */

interface FakeRendererHandle extends PlantRenderer {
  disposals: number;
  renders: number;
  sizes: Array<{ width: number; height: number }>;
}

function createFakeContext(): WebGL2RenderingContext {
  return {
    getExtension: () => null,
  } as unknown as WebGL2RenderingContext;
}

function createFakeRenderer(): FakeRendererHandle {
  const context = createFakeContext();
  const renderer: FakeRendererHandle = {
    disposals: 0,
    dispose: () => {
      renderer.disposals += 1;
    },
    getContext: () => context,
    info: {
      memory: { geometries: 0, textures: 0 },
      programs: { length: 0 },
      render: { calls: 0, triangles: 0 },
    },
    render: () => {
      renderer.renders += 1;
    },
    renders: 0,
    setPixelRatio: () => {},
    setSize: (width, height) => {
      renderer.sizes.push({ height, width });
    },
    sizes: [],
  };
  return renderer;
}

function createFakeHost() {
  let time = 0;
  let nextHandle = 1;
  const queue = new Map<number, (time: number) => void>();
  let requests = 0;
  const teardowns: string[] = [];
  let visibility: ((visible: boolean) => void) | undefined;
  let intersection: ((visible: boolean) => void) | undefined;
  let resize: (() => void) | undefined;

  const host: PlantRuntimeHost = {
    cancelAnimationFrame: (handle) => {
      queue.delete(handle);
    },
    now: () => time,
    observeDocumentVisibility: (callback) => {
      visibility = callback;
      return () => {
        teardowns.push("visibility");
        visibility = undefined;
      };
    },
    observeIntersection: (_target, callback) => {
      intersection = callback;
      return () => {
        teardowns.push("intersection");
        intersection = undefined;
      };
    },
    observeResize: (_target, callback) => {
      resize = callback;
      return () => {
        teardowns.push("resize");
        resize = undefined;
      };
    },
    pixelRatio: () => 1,
    requestAnimationFrame: (callback) => {
      requests += 1;
      const handle = nextHandle;
      nextHandle += 1;
      queue.set(handle, callback);
      return handle;
    },
  };

  return {
    host,
    teardowns,
    get outstanding() {
      return queue.size;
    },
    get requests() {
      return requests;
    },
    setVisible(visible: boolean) {
      visibility?.(visible);
    },
    setIntersecting(visible: boolean) {
      intersection?.(visible);
    },
    triggerResize() {
      resize?.();
    },
    flush(ms = 16) {
      time += ms;
      const pending = [...queue.entries()];
      queue.clear();
      for (const [, callback] of pending) {
        callback(time);
      }
      return pending.length;
    },
  };
}

function createFakeProbe() {
  const events: string[] = [];
  const entities: PlantEntityStats[] = [];
  let frames = 0;
  let projections: readonly PlantProjectionEntry[] = [];
  const probe = {
    contextLost: () => events.push("contextLost"),
    contextRestored: () => events.push("contextRestored"),
    entities: (stats: PlantEntityStats) => {
      events.push("entities");
      entities.push(stats);
    },
    environment: () => {},
    frame: () => {
      frames += 1;
    },
    layout: () => {},
    model: () => {},
    motion: () => {},
    overlay: () => {},
    projections: (entries) => {
      projections = entries;
    },
    rafScheduled: () => {},
    registerMeasure: () => () => {},
    rendererCreated: () => events.push("rendererCreated"),
    rendererDisposed: () => events.push("rendererDisposed"),
    rendererState: (state: string) => events.push(`state:${state}`),
    sceneBuilt: () => events.push("sceneBuilt"),
    sceneDisposed: () => events.push("sceneDisposed"),
    viewport: () => {},
    visual: () => {},
  } satisfies PlantProbeSink;
  return {
    entities,
    events,
    probe,
    get frames() {
      return frames;
    },
    get projections() {
      return projections;
    },
  };
}

function createCanvas(): HTMLCanvasElement {
  const parent = document.createElement("div");
  Object.defineProperty(parent, "clientWidth", { configurable: true, value: 800 });
  Object.defineProperty(parent, "clientHeight", { configurable: true, value: 600 });
  const canvas = document.createElement("canvas");
  parent.append(canvas);
  document.body.append(parent);
  return canvas;
}

function update(
  overrides: Partial<FactoryWebGLRuntimeUpdate> = {},
): FactoryWebGLRuntimeUpdate {
  const { layout, model } = plantFixture();
  return {
    animateTransitions: true,
    freshness: "live",
    layout,
    lens: "world",
    model,
    reducedMotion: false,
    theme: "light",
    ...overrides,
  };
}

function setup(options: { probe?: PlantProbeSink } = {}) {
  const canvas = createCanvas();
  const renderer = createFakeRenderer();
  const clock = createFakeHost();
  const runtime = createFactoryWebGLRuntime({
    canvas,
    createRenderer: () => renderer,
    host: clock.host,
    ...(options.probe ? { probe: options.probe } : {}),
  });
  if (!runtime) {
    throw new Error("runtime was not created");
  }
  return { canvas, clock, renderer, runtime };
}

beforeEach(() => {
  document.body.replaceChildren();
});

describe("Factory WebGL runtime lifecycle", () => {
  it("creates one renderer and keeps it across model, lens, theme and motion changes", () => {
    const created: PlantRenderer[] = [];
    const canvas = createCanvas();
    const clock = createFakeHost();
    const runtime = createFactoryWebGLRuntime({
      canvas,
      createRenderer: () => {
        const renderer = createFakeRenderer();
        created.push(renderer);
        return renderer;
      },
      host: clock.host,
    });
    expect(runtime).toBeDefined();
    runtime?.update(update());
    expect(created).toHaveLength(1);

    const scene = runtime?.scene;
    runtime?.update(update({ theme: "dark" }));
    runtime?.update(update({ lens: "flow" }));
    runtime?.update(update({ lens: "risk" }));
    runtime?.update(update({ reducedMotion: true }));
    runtime?.update(update());
    runtime?.resize();

    expect(created).toHaveLength(1);
    expect(runtime?.scene).toBe(scene);
    expect(runtime?.inspect().reconciles).toBe(6);

    runtime?.dispose();
  });

  it("retains Object3D identity for unchanged entities and disposes removals once", () => {
    const { clock, runtime } = setup();
    const first = update();
    runtime.update(first);

    const crateKey = plantEntityRegistryKey("crate", first.layout.carriers[0]!.id);
    const machineKey = plantEntityRegistryKey(
      "machine",
      first.layout.machines[0]!.id,
    );
    const crate = runtime.scene.getObjectByName(crateKey);
    const machine = runtime.scene.getObjectByName(machineKey);
    expect(crate).toBeDefined();
    expect(machine).toBeDefined();

    runtime.update(update({ theme: "dark" }));
    runtime.update(update({ lens: "risk" }));
    expect(runtime.scene.getObjectByName(crateKey)).toBe(crate);
    expect(runtime.scene.getObjectByName(machineKey)).toBe(machine);
    expect(runtime.inspect().resources.doubleDisposals).toBe(0);

    const trimmed = update();
    const trimmedModel = {
      ...trimmed.model,
      carriers: trimmed.model.carriers.slice(1),
    };
    const withoutCrate = {
      ...trimmed,
      layout: plantFixtureLayout(trimmedModel, trimmed.layout),
      model: trimmedModel,
    };
    runtime.update(withoutCrate);
    expect(runtime.scene.getObjectByName(crateKey)).toBeUndefined();
    expect(runtime.inspect().lastReconcile.removed).toBe(1);
    expect(runtime.inspect().resources.doubleDisposals).toBe(0);
    expect(runtime.scene.getObjectByName(machineKey)).toBe(machine);

    clock.flush();
    runtime.dispose();
    expect(runtime.inspect().resources.doubleDisposals).toBe(0);
  });

  it("renders bays, machines, and exact tracks through bounded instance batches", () => {
    const { runtime } = setup();
    const next = update();
    runtime.update(next);

    const batchGroup = runtime.scene.getObjectByName("plant:instance-batches");
    expect(batchGroup).toBeDefined();
    expect(batchGroup?.children.length).toBe(runtime.inspect().batches);
    expect(batchGroup?.children.every((child) => child instanceof THREE.InstancedMesh)).toBe(
      true,
    );
    expect(runtime.inspect().batches).toBeLessThanOrEqual(
      next.layout.instanceBatches.length,
    );

    const machine = runtime.scene.getObjectByName(
      plantEntityRegistryKey("machine", next.layout.machines[0]!.id),
    );
    expect(machine).toBeDefined();
    expect(machine?.children).toHaveLength(0);

    runtime.dispose();
  });

  it("refits the retained orthographic camera when dynamic bounds grow", () => {
    const { renderer, runtime } = setup();
    runtime.update(update());
    const first = runtime.inspect().cameraFit;
    const scene = runtime.scene;

    const largeModel = scalablePlantFixture({
      workflowCount: 12,
      stagesPerWorkflow: 12,
    });
    const largeLayout = plantFixtureLayout(largeModel);
    runtime.update(
      update({
        layout: largeLayout,
        model: largeModel,
      }),
    );
    const second = runtime.inspect().cameraFit;

    expect(runtime.scene).toBe(scene);
    expect(renderer.disposals).toBe(0);
    expect(second?.viewWidth).toBeGreaterThan(first?.viewWidth ?? 0);
    expect(second?.viewHeight).toBeGreaterThan(first?.viewHeight ?? 0);

    runtime.dispose();
  });

  it("keeps at most one animation frame outstanding while operating", () => {
    const { clock, runtime } = setup();
    runtime.update(update());
    expect(runtime.inspect().motion).toBe(true);
    expect(clock.outstanding).toBe(1);

    runtime.update(update({ theme: "dark" }));
    runtime.update(update({ lens: "flow" }));
    runtime.resize();
    expect(clock.outstanding).toBe(1);

    clock.flush();
    expect(clock.outstanding).toBe(1);

    runtime.dispose();
    expect(clock.outstanding).toBe(0);
  });

  it("stops motion for reduced motion, the Risk lens, hidden documents and offscreen canvases", () => {
    const { clock, runtime } = setup();
    runtime.update(update());
    expect(runtime.inspect().motion).toBe(true);

    runtime.update(update({ reducedMotion: true }));
    expect(runtime.inspect().motion).toBe(false);

    runtime.update(update());
    expect(runtime.inspect().motion).toBe(true);

    runtime.update(update({ lens: "risk" }));
    expect(runtime.inspect().motion).toBe(false);

    runtime.update(update());
    clock.setVisible(false);
    expect(runtime.inspect().motion).toBe(false);
    clock.setVisible(true);
    expect(runtime.inspect().motion).toBe(true);

    clock.setIntersecting(false);
    expect(runtime.inspect().motion).toBe(false);
    clock.setIntersecting(true);
    expect(runtime.inspect().motion).toBe(true);

    runtime.dispose();
  });

  it("stops motion when no confirmed carrier is working", () => {
    const { runtime } = setup();
    const idle = plantFixture({
      signals: {
        "01RUNIMPLEMENT1": { confirmed: true, reason: "human-gate", state: "paused" },
        "01RUNREVIEW0001": { confirmed: true, reason: "human-gate", state: "paused" },
      },
    });
    runtime.update(update({ layout: idle.layout, model: idle.model }));

    expect(runtime.inspect().animatedCount).toBe(0);
    expect(runtime.inspect().motion).toBe(false);

    runtime.dispose();
  });
});

describe("Factory WebGL runtime context state machine", () => {
  it("falls back on loss and reports ready only after a successful restored frame", () => {
    const sink = createFakeProbe();
    const { canvas, clock, runtime } = setup({ probe: sink.probe });
    runtime.update(update());
    expect(runtime.inspect().state).toBe("ready");

    canvas.dispatchEvent(new Event("webglcontextlost", { cancelable: true }));
    expect(runtime.inspect().state).toBe("fallback");
    expect(runtime.inspect().contextLost).toBe(true);
    expect(clock.outstanding).toBe(0);
    expect(sink.events).toContain("contextLost");

    const framesAtLoss = sink.frames;
    clock.flush();
    expect(sink.frames).toBe(framesAtLoss);

    canvas.dispatchEvent(new Event("webglcontextrestored"));
    // Restoration alone is not readiness; pixels are.
    expect(runtime.inspect().state).toBe("pending");
    expect(clock.outstanding).toBe(1);

    clock.flush();
    expect(runtime.inspect().state).toBe("ready");
    expect(sink.frames).toBeGreaterThan(framesAtLoss);
    expect(sink.events).toContain("contextRestored");

    runtime.dispose();
  });

  it("survives repeated loss and restoration without rebuilding the scene", () => {
    const sink = createFakeProbe();
    const { canvas, clock, runtime } = setup({ probe: sink.probe });
    runtime.update(update());
    const scene = runtime.scene;
    const entityKeys = runtime.inspect().entityKeys;

    for (let cycle = 0; cycle < 3; cycle += 1) {
      canvas.dispatchEvent(new Event("webglcontextlost", { cancelable: true }));
      expect(runtime.inspect().state).toBe("fallback");
      canvas.dispatchEvent(new Event("webglcontextrestored"));
      clock.flush();
      expect(runtime.inspect().state).toBe("ready");
    }

    expect(runtime.scene).toBe(scene);
    expect(runtime.inspect().entityKeys).toEqual(entityKeys);
    expect(sink.events.filter((event) => event === "sceneBuilt")).toHaveLength(1);
    expect(sink.events.filter((event) => event === "sceneDisposed")).toHaveLength(0);
    expect(sink.events.filter((event) => event === "rendererCreated")).toHaveLength(1);

    runtime.dispose();
  });

  it("keeps updating the retained scene while the context is lost", () => {
    const { canvas, clock, renderer, runtime } = setup();
    runtime.update(update());
    canvas.dispatchEvent(new Event("webglcontextlost", { cancelable: true }));

    const rendersAtLoss = renderer.renders;
    runtime.update(update({ lens: "flow", theme: "dark" }));
    expect(renderer.renders).toBe(rendersAtLoss);
    expect(runtime.inspect().entityKeys.length).toBeGreaterThan(0);
    expect(runtime.inspect().state).toBe("fallback");

    canvas.dispatchEvent(new Event("webglcontextrestored"));
    clock.flush();
    expect(runtime.inspect().state).toBe("ready");
    expect(runtime.inspect().motion).toBe(true);
    expect(clock.outstanding).toBe(1);

    runtime.dispose();
  });
});

describe("Factory WebGL runtime transfers", () => {
  const stageChange = (): {
    before: FactoryWebGLRuntimeUpdate;
    after: FactoryWebGLRuntimeUpdate;
  } => {
    const { after, before } = plantStageChangeFixture();
    return {
      after: update({ layout: after.layout, model: after.model }),
      before: update({ layout: before.layout, model: before.model }),
    };
  };

  it("plays a confirmed stage change once and never replays it", () => {
    const { clock, runtime } = setup();
    const { after, before } = stageChange();
    runtime.update(before);
    runtime.update(after);
    expect(runtime.inspect().activeTransfers).toBe(1);

    // The transfer must finish on its own, not be reset by unrelated churn.
    runtime.update({ ...after, theme: "dark" });
    runtime.update({ ...after, lens: "flow" });
    expect(runtime.inspect().activeTransfers).toBe(1);

    for (let frame = 0; frame < 60; frame += 1) {
      clock.flush(16);
    }
    expect(runtime.inspect().activeTransfers).toBe(0);

    runtime.update({ ...after, theme: "light" });
    expect(runtime.inspect().activeTransfers).toBe(0);

    runtime.dispose();
  });

  it("publishes the exact per-frame crate transfer position to DOM consumers", () => {
    const { after, before } = stageChange();
    const sink = createFakeProbe();
    const { canvas, clock, runtime } = setup({ probe: sink.probe });
    const carrier = after.model.carriers.find(
      (candidate) => candidate.transition?.kind === "stage-change",
    )!;
    const anchor = after.layout.carriers.find(
      (candidate) => candidate.id === carrier.runId,
    )!;
    const overlayAnchor = after.layout.overlayAnchors.find(
      (candidate) => candidate.id === anchor.overlayAnchorId,
    )!;
    const published: Array<{ x: number; y: number }> = [];
    const canvasHost = canvas.parentElement!;
    canvasHost.dataset.plantCanvas = "";
    canvasHost.getBoundingClientRect = () =>
      ({
        bottom: 600,
        height: 600,
        left: 0,
        right: 800,
        top: 0,
        width: 800,
        x: 0,
        y: 0,
        toJSON: () => ({}),
      }) as DOMRect;
    const control = document.createElement("button");
    control.dataset.plantProbeId = carrier.runId;
    const origin = document.createElement("span");
    origin.dataset.plantAnchorOrigin = "";
    control.append(origin);
    canvasHost.append(control);
    let domPoint = { x: 0, y: 0 };
    origin.getBoundingClientRect = () =>
      ({
        bottom: domPoint.y,
        height: 0,
        left: domPoint.x,
        right: domPoint.x,
        top: domPoint.y,
        width: 0,
        x: domPoint.x,
        y: domPoint.y,
        toJSON: () => ({}),
      }) as DOMRect;
    runtime.subscribeAnimation((entries) => {
      const entry = entries.find((candidate) => candidate.id === carrier.runId);
      if (entry) {
        domPoint = { x: entry.point.x, y: entry.point.y };
        published.push({ x: entry.point.x, y: entry.point.y });
      }
    });

    runtime.update(before);
    runtime.update(after);
    clock.flush(100);
    clock.flush(100);

    const crate = runtime.scene.getObjectByName(
      plantEntityRegistryKey("crate", carrier.runId),
    )!;
    const projectedCrate = runtime.project({
      x: crate.position.x,
      y: overlayAnchor.position.y,
      z: crate.position.z,
    });
    const projectedAnchor = runtime.projectEntity(
      carrier.runId,
      overlayAnchor.position,
    );
    expect(projectedAnchor.x).toBeCloseTo(projectedCrate.x, 6);
    expect(projectedAnchor.y).toBeCloseTo(projectedCrate.y, 6);
    expect(published.at(-1)?.x).toBeCloseTo(projectedCrate.x, 6);
    expect(published.at(-1)?.y).toBeCloseTo(projectedCrate.y, 6);
    const probeEntry = sink.projections.find(
      (entry) => entry.id === carrier.runId,
    );
    expect(probeEntry?.drift.distance).toBeLessThan(0.001);
    expect(runtime.inspect().activeTransfers).toBe(1);

    runtime.dispose();
  });

  it("does not replay a transfer that happened before the runtime mounted", () => {
    const { runtime } = setup();
    const { after } = stageChange();

    // A fresh mount receives the completed move as its first snapshot.
    runtime.update({ ...after, animateTransitions: false });
    expect(runtime.inspect().activeTransfers).toBe(0);

    runtime.update({ ...after, animateTransitions: true });
    expect(runtime.inspect().activeTransfers).toBe(0);

    runtime.dispose();
  });

  it("suppresses transfers under reduced motion and the Risk lens", () => {
    const { runtime } = setup();
    const { after, before } = stageChange();
    runtime.update({ ...before, reducedMotion: true });
    runtime.update({ ...after, reducedMotion: true });
    expect(runtime.inspect().activeTransfers).toBe(0);

    const risk = stageChange();
    const { runtime: riskRuntime } = setup();
    riskRuntime.update({ ...risk.before, lens: "risk" as FactoryLens });
    riskRuntime.update({ ...risk.after, lens: "risk" as FactoryLens });
    expect(riskRuntime.inspect().activeTransfers).toBe(0);

    runtime.dispose();
    riskRuntime.dispose();
  });
});

describe("Factory WebGL runtime disposal", () => {
  it("releases every owned resource exactly once and is idempotent", () => {
    const sink = createFakeProbe();
    const { clock, renderer, runtime } = setup({ probe: sink.probe });
    runtime.update(update());
    clock.flush();

    const disposals = runtime.inspect().resources.disposals;
    expect(disposals).toBe(0);

    runtime.dispose();
    runtime.dispose();
    runtime.dispose();

    expect(renderer.disposals).toBe(1);
    expect(runtime.inspect().resources.doubleDisposals).toBe(0);
    expect(runtime.inspect().resources.disposals).toBeGreaterThan(0);
    expect(runtime.inspect().entityKeys).toHaveLength(0);
    expect(runtime.scene.children).toHaveLength(0);
    expect(clock.outstanding).toBe(0);
    expect(sink.events.filter((event) => event === "rendererDisposed")).toHaveLength(1);
    expect(sink.events.filter((event) => event === "sceneDisposed")).toHaveLength(1);
    expect(clock.teardowns.sort()).toEqual(["intersection", "resize", "visibility"]);

    // Post-disposal calls are inert instead of resurrecting the runtime.
    runtime.update(update());
    runtime.resize();
    expect(renderer.renders).toBeGreaterThan(0);
    expect(runtime.inspect().disposed).toBe(true);
  });

  it("disposes geometries, materials and shadow maps of the static hall", () => {
    const { runtime } = setup();
    runtime.update(update());

    const geometries = new Map<THREE.BufferGeometry, string>();
    const materials = new Map<THREE.Material, string>();
    const instancedMeshes = new Map<THREE.InstancedMesh, string>();
    runtime.scene.traverse((object) => {
      const mesh = object as Partial<THREE.Mesh> & { name?: string };
      const label = `${object.type}:${object.name || object.parent?.name || "static"}`;
      if (object instanceof THREE.InstancedMesh) {
        instancedMeshes.set(object, label);
      }
      if (mesh.geometry) {
        geometries.set(mesh.geometry as THREE.BufferGeometry, label);
      }
      const material = mesh.material;
      if (Array.isArray(material)) {
        for (const entry of material) {
          materials.set(entry, label);
        }
      } else if (material) {
        materials.set(material, label);
      }
    });
    expect(geometries.size).toBeGreaterThan(0);
    expect(materials.size).toBeGreaterThan(0);

    const spies = new Map<string, ReturnType<typeof vi.spyOn>>();
    for (const [geometry, label] of geometries) {
      spies.set(`geometry ${label}`, vi.spyOn(geometry, "dispose"));
    }
    for (const [material, label] of materials) {
      spies.set(`material ${label}`, vi.spyOn(material, "dispose"));
    }
    for (const [mesh, label] of instancedMeshes) {
      spies.set(`instanced mesh ${label}`, vi.spyOn(mesh, "dispose"));
    }
    let shadows = 0;
    runtime.scene.traverse((object) => {
      const light = object as THREE.Light & { shadow?: THREE.LightShadow };
      // Only a shadow-casting light ever allocates a depth map to leak.
      if (light.castShadow && light.shadow) {
        shadows += 1;
        spies.set(`shadow ${object.type}`, vi.spyOn(light.shadow, "dispose"));
      }
    });
    expect(shadows).toBeGreaterThan(0);

    runtime.dispose();

    const leaked = [...spies]
      .filter(([, spy]) => spy.mock.calls.length !== 1)
      .map(([label, spy]) => `${label} disposed ${spy.mock.calls.length} times`);
    expect(leaked).toEqual([]);
  });
});

describe("Factory WebGL runtime projection contract", () => {
  it("publishes a live WebGL projection once the scene exists", () => {
    const { runtime } = setup();
    runtime.update(update());
    const projection = runtime.projection();
    expect(projection.source).toBe("webgl");
    expect(projection.canvas.width).toBeGreaterThan(0);
    expect(projection.canvas.height).toBeGreaterThan(0);
    expect(projection.matrix).toHaveLength(16);
    expect(projection.revision).toBeGreaterThan(0);
  });

  it("projects world anchors into the canvas the camera actually drew", () => {
    const { runtime } = setup();
    runtime.update(update());
    const projection = runtime.projection();
    const centre = runtime.project({ x: 0, y: 0, z: 0 });
    expect(Number.isFinite(centre.x)).toBe(true);
    expect(Number.isFinite(centre.y)).toBe(true);
    expect(centre.x).toBeGreaterThanOrEqual(0);
    expect(centre.x).toBeLessThanOrEqual(projection.canvas.width);
    expect(centre.y).toBeGreaterThanOrEqual(0);
    expect(centre.y).toBeLessThanOrEqual(projection.canvas.height);
    expect(centre.visible).toBe(true);
  });

  it("agrees with the published matrix, so overlay and runtime cannot diverge", () => {
    const { runtime } = setup();
    runtime.update(update());
    const projector = createPlantProjector(runtime.projection());
    for (const point of [
      { x: 0, y: 0, z: 0 },
      { x: 4, y: 1, z: -3 },
      { x: -7, y: 2, z: 5 },
    ]) {
      const direct = runtime.project(point);
      const viaState = projector.project(point);
      expect(viaState.x).toBeCloseTo(direct.x, 6);
      expect(viaState.y).toBeCloseTo(direct.y, 6);
    }
  });

  it("notifies subscribers when the projection changes and not when it does not", () => {
    const { runtime } = setup();
    runtime.update(update());
    const revisions: number[] = [];
    const unsubscribe = runtime.subscribe((state) => {
      revisions.push(state.revision);
    });
    const before = revisions.length;
    // The same update cannot move the camera, so it must not churn React.
    runtime.update(update());
    expect(revisions.length).toBe(before);
    runtime.update(update({ safeArea: plantScreenRect(10, 20, 400, 300) }));
    expect(revisions.length).toBeGreaterThan(before);
    expect(new Set(revisions).size).toBe(revisions.length);
    unsubscribe();
    const afterUnsubscribe = revisions.length;
    runtime.update(update({ safeArea: plantScreenRect(0, 0, 200, 200) }));
    expect(revisions.length).toBe(afterUnsubscribe);
  });

  it("fits the camera into the safe area rather than the whole canvas", () => {
    const { runtime } = setup();
    runtime.update(update());
    const full = runtime.project({ x: 0, y: 0, z: 0 });
    runtime.update(
      update({ safeArea: plantScreenRect(0, 0, 300, 300) }),
    );
    const constrained = runtime.project({ x: 0, y: 0, z: 0 });
    const safeArea = runtime.projection().safeArea;
    expect(safeArea.width).toBeCloseTo(300, 3);
    expect(safeArea.height).toBeCloseTo(300, 3);
    expect(
      Math.hypot(constrained.x - full.x, constrained.y - full.y),
    ).toBeGreaterThan(1);
  });

  it("keeps the fitted world inside the safe rectangle", () => {
    const { runtime } = setup();
    const { layout } = plantFixture();
    runtime.update(
      update({ safeArea: plantScreenRect(40, 30, 420, 260) }),
    );
    const bounds = layout.worldBounds;
    const corners = [
      { x: bounds.min.x, y: 0, z: bounds.min.z },
      { x: bounds.max.x, y: 0, z: bounds.min.z },
      { x: bounds.min.x, y: 0, z: bounds.max.z },
      { x: bounds.max.x, y: 0, z: bounds.max.z },
    ];
    for (const corner of corners) {
      const point = runtime.project(corner);
      expect(point.x).toBeGreaterThanOrEqual(40 - 1);
      expect(point.x).toBeLessThanOrEqual(40 + 420 + 1);
      expect(point.y).toBeGreaterThanOrEqual(30 - 1);
      expect(point.y).toBeLessThanOrEqual(30 + 260 + 1);
    }
  });

  it("republishes the projection after a resize", () => {
    const { clock, runtime } = setup();
    runtime.update(update());
    const before = runtime.projection().revision;
    clock.triggerResize();
    runtime.resize();
    expect(runtime.projection().revision).toBeGreaterThanOrEqual(before);
    expect(runtime.projection().source).toBe("webgl");
  });

  it("resolves a semantic key from a scene object, so picking can select", () => {
    const { runtime } = setup();
    runtime.update(update());
    const stamped: Array<{ key: string; kind: string }> = [];
    runtime.scene.traverse((object) => {
      const data = object.userData as {
        plantEntityKey?: string;
        plantEntityKind?: string;
      };
      if (data.plantEntityKey && data.plantEntityKind) {
        stamped.push({ key: data.plantEntityKey, kind: data.plantEntityKind });
      }
    });
    expect(stamped.length).toBeGreaterThan(0);
    expect(stamped.some((entry) => entry.kind === "station")).toBe(true);
  });

  it("reports the projection source and safe area on every report", () => {
    const { runtime } = setup();
    runtime.update(update({ safeArea: plantScreenRect(5, 6, 360, 240) }));
    const report = runtime.inspect();
    expect(report.projectionRevision).toBeGreaterThan(0);
    expect(report.safeArea).toEqual(plantScreenRect(5, 6, 360, 240));
  });
});