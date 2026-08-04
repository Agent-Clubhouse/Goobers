import {
  setPlantProbeSink,
  type PlantCanvasMeasurement,
  type PlantEntityStats,
  type PlantLayoutMeasurement,
  type PlantModelMeasurement,
  type PlantOverlayEntry,
  type PlantOverlayMeasurement,
  type PlantProbeSink,
  type PlantProjectionEntry,
  type PlantRendererInfo,
  type PlantRendererState,
  type PlantViewportMeasurement,
  type PlantViewportCameraPose,
  type PlantViewportControl,
  type PlantVisualMeasurement,
} from "./plantProbeSink";

export interface PlantProjectionMeasurement {
  entries: readonly PlantProjectionEntry[];
  maxDrift: number;
  meanDrift: number;
}

export interface PlantProbeSnapshot {
  capturedAt: number;
  renderer: {
    state: PlantRendererState;
    contexts: number;
    activeContexts: number;
    disposals: number;
    losses: number;
    restores: number;
    info: PlantRendererInfo;
  };
  scene: {
    builds: number;
    disposals: number;
  };
  /** Keyed reconciliation totals since the last reset. */
  entities: PlantEntityStats & { reconciles: number };
  /** Model generations delivered to the Plant since the last reset. */
  modelUpdates: number;
  animation: {
    frames: number;
    rafCallbacks: number;
    rafRequests: number;
    motion: boolean;
    animatedCount: number;
  };
  canvas?: PlantCanvasMeasurement;
  model?: PlantModelMeasurement;
  layout?: PlantLayoutMeasurement;
  viewport?: PlantViewportMeasurement;
  projection: PlantProjectionMeasurement;
  overlay?: PlantOverlayMeasurement;
  /** The palette, contrast and marker truth behind the last frame. */
  visual?: PlantVisualMeasurement;
}

export interface PlantProbe {
  snapshot: () => PlantProbeSnapshot;
  reset: () => PlantProbeSnapshot;
  waitForFrames: (count?: number, timeoutMs?: number) => Promise<PlantProbeSnapshot>;
  /**
   * Re-runs every registered measurement synchronously and returns the result.
   *
   * The runtime publishes its camera before React commits the overlay, so a
   * measurement taken in the same tick as a camera change reports one frame of
   * React latency as drift. Callers that have settled the page — the harness
   * between viewport changes — ask for a fresh reading instead of trusting
   * whatever the last change happened to leave behind.
   */
  remeasure: () => PlantProbeSnapshot;
  /** Applies an exact outer camera pose for deterministic browser checks. */
  setViewportCamera: (pose: PlantViewportCameraPose) => boolean;
  loseContext: () => boolean;
  restoreContext: () => boolean;
}

interface FrameWaiter {
  target: number;
  resolve: (snapshot: PlantProbeSnapshot) => void;
  reject: (error: Error) => void;
  timer: ReturnType<typeof globalThis.setTimeout>;
}

interface PlantProbeController {
  api: PlantProbe;
  sink: PlantProbeSink;
  dispose: () => void;
}

declare global {
  interface Window {
    __plantProbe?: PlantProbe;
  }
}

const EMPTY_RENDERER_INFO: PlantRendererInfo = {
  calls: 0,
  triangles: 0,
  programs: 0,
  geometries: 0,
  textures: 0,
};

const EMPTY_ENTITY_STATS: PlantEntityStats = {
  created: 0,
  replaced: 0,
  updated: 0,
  removed: 0,
  live: 0,
};

export function summarizePlantProjections(
  entries: readonly PlantProjectionEntry[],
): PlantProjectionMeasurement {
  if (entries.length === 0) {
    return { entries: [], maxDrift: 0, meanDrift: 0 };
  }
  const total = entries.reduce((sum, entry) => sum + entry.drift.distance, 0);
  return {
    entries: entries.map(copyProjectionEntry),
    maxDrift: Math.max(...entries.map((entry) => entry.drift.distance)),
    meanDrift: total / entries.length,
  };
}

export function createPlantProbeController(): PlantProbeController {
  let rendererState: PlantRendererState = "pending";
  let contexts = 0;
  let activeContext:
    | {
        canvas: HTMLCanvasElement;
        context: WebGLRenderingContext | WebGL2RenderingContext;
      }
    | undefined;
  const contextRefs = new Map<
    WebGLRenderingContext | WebGL2RenderingContext,
    { canvas: HTMLCanvasElement; count: number }
  >();
  /**
   * WEBGL_lose_context handles, captured while the context is still live.
   *
   * getExtension() returns null on a LOST context, so a probe that looked the
   * extension up on demand could lose a context and then never restore it —
   * which is exactly what the WP-0 harness measured.
   */
  const loseContextExtensions = new Map<
    WebGLRenderingContext | WebGL2RenderingContext,
    WEBGL_lose_context
  >();
  let activeContexts = 0;
  let rendererDisposals = 0;
  let losses = 0;
  let restores = 0;
  let rendererInfo = { ...EMPTY_RENDERER_INFO };
  let sceneBuilds = 0;
  let sceneDisposals = 0;
  let entities = { ...EMPTY_ENTITY_STATS };
  let reconciles = 0;
  let modelUpdates = 0;
  let frames = 0;
  let rafCallbacks = 0;
  let rafRequests = 0;
  let motion = false;
  let animatedCount = 0;
  let canvas: PlantCanvasMeasurement | undefined;
  let model: PlantModelMeasurement | undefined;
  let layout: PlantLayoutMeasurement | undefined;
  let viewport: PlantViewportMeasurement | undefined;
  let viewportControl: PlantViewportControl | undefined;
  let projection = summarizePlantProjections([]);
  let overlay: PlantOverlayMeasurement | undefined;
  let visual: PlantVisualMeasurement | undefined;
  const measures = new Set<() => void>();
  const waiters = new Set<FrameWaiter>();

  const snapshot = (): PlantProbeSnapshot => ({
    capturedAt: performance.now(),
    renderer: {
      state: rendererState,
      contexts,
      activeContexts,
      disposals: rendererDisposals,
      losses,
      restores,
      info: { ...rendererInfo },
    },
    scene: {
      builds: sceneBuilds,
      disposals: sceneDisposals,
    },
    entities: { ...entities, reconciles },
    modelUpdates,
    animation: {
      frames,
      rafCallbacks,
      rafRequests,
      motion,
      animatedCount,
    },
    ...(canvas ? { canvas: { ...canvas } } : {}),
    ...(model ? { model: copyModel(model) } : {}),
    ...(layout ? { layout: copyLayout(layout) } : {}),
    ...(viewport ? { viewport: copyViewport(viewport) } : {}),
    projection: {
      entries: projection.entries.map(copyProjectionEntry),
      maxDrift: projection.maxDrift,
      meanDrift: projection.meanDrift,
    },
    ...(overlay ? { overlay: copyOverlay(overlay) } : {}),
    ...(visual ? { visual: copyVisual(visual) } : {}),
  });

  const settleFrameWaiters = () => {
    for (const waiter of waiters) {
      if (frames < waiter.target) {
        continue;
      }
      globalThis.clearTimeout(waiter.timer);
      waiters.delete(waiter);
      waiter.resolve(snapshot());
    }
  };

  const sink: PlantProbeSink = {
    rendererState: (state) => {
      rendererState = state;
    },
    rendererCreated: (input) => {
      contexts += 1;
      const registered = contextRefs.get(input.context);
      contextRefs.set(input.context, {
        canvas: input.canvas,
        count: (registered?.count ?? 0) + 1,
      });
      const extension = input.context.getExtension("WEBGL_lose_context");
      if (extension) {
        loseContextExtensions.set(input.context, extension);
      }
      activeContexts = contextRefs.size;
      activeContext = input;
    },
    rendererDisposed: (context) => {
      rendererDisposals += 1;
      const registered = contextRefs.get(context);
      if (registered && registered.count > 1) {
        contextRefs.set(context, { ...registered, count: registered.count - 1 });
      } else {
        contextRefs.delete(context);
        loseContextExtensions.delete(context);
      }
      activeContexts = contextRefs.size;
      if (activeContext?.context === context && !contextRefs.has(context)) {
        const fallback = [...contextRefs.entries()].at(-1);
        activeContext = fallback
          ? {
              canvas: fallback[1].canvas,
              context: fallback[0],
            }
          : undefined;
      }
    },
    contextLost: () => {
      losses += 1;
    },
    contextRestored: () => {
      restores += 1;
    },
    sceneBuilt: () => {
      sceneBuilds += 1;
    },
    sceneDisposed: () => {
      sceneDisposals += 1;
    },
    entities: (stats) => {
      reconciles += 1;
      entities = {
        created: entities.created + stats.created,
        live: stats.live,
        removed: entities.removed + stats.removed,
        replaced: entities.replaced + stats.replaced,
        updated: entities.updated + stats.updated,
      };
    },
    motion: (enabled, count) => {
      motion = enabled;
      animatedCount = count;
    },
    rafScheduled: () => {
      rafRequests += 1;
    },
    frame: (input) => {
      frames += 1;
      rafCallbacks += input.raf ? 1 : 0;
      rendererInfo = { ...input.info };
      canvas = { ...input.canvas };
      settleFrameWaiters();
    },
    environment: (environment) => {
      if (model) {
        model = { ...model, ...environment };
      }
    },
    model: (nextModel) => {
      modelUpdates += 1;
      model = copyModel(nextModel);
    },
    layout: (nextLayout) => {
      layout = copyLayout(nextLayout);
    },
    viewport: (nextViewport) => {
      viewport = copyViewport(nextViewport);
    },
    viewportControl: (control) => {
      viewportControl = control;
    },
    projections: (entries) => {
      projection = summarizePlantProjections(entries);
    },
    overlay: (measurement) => {
      overlay = copyOverlay(measurement);
    },
    visual: (measurement) => {
      visual = copyVisual(measurement);
    },
    registerMeasure: (measure) => {
      measures.add(measure);
      return () => {
        measures.delete(measure);
      };
    },
  };

  const api: PlantProbe = {
    snapshot,
    remeasure: () => {
      for (const measure of [...measures]) {
        measure();
      }
      return snapshot();
    },
    reset: () => {
      contexts = 0;
      activeContexts = contextRefs.size;
      rendererDisposals = 0;
      losses = 0;
      restores = 0;
      sceneBuilds = 0;
      sceneDisposals = 0;
      entities = { ...EMPTY_ENTITY_STATS, live: entities.live };
      reconciles = 0;
      modelUpdates = 0;
      frames = 0;
      rafCallbacks = 0;
      rafRequests = 0;
      return snapshot();
    },
    setViewportCamera: (pose) => {
      if (!viewportControl) {
        return false;
      }
      viewportControl.setCamera(pose);
      return true;
    },
    waitForFrames: (count = 1, timeoutMs = 5_000) => {
      if (!Number.isInteger(count) || count < 1) {
        return Promise.reject(new RangeError("Frame count must be a positive integer."));
      }
      if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) {
        return Promise.reject(new RangeError("Frame timeout must be positive."));
      }
      return new Promise<PlantProbeSnapshot>((resolve, reject) => {
        const target = frames + count;
        const waiter: FrameWaiter = {
          target,
          resolve,
          reject,
          timer: globalThis.setTimeout(() => {
            waiters.delete(waiter);
            reject(
              new Error(
                `Timed out waiting for ${count} Plant frame${count === 1 ? "" : "s"}.`,
              ),
            );
          }, timeoutMs),
        };
        waiters.add(waiter);
      });
    },
    loseContext: () => toggleContext("loseContext"),
    restoreContext: () => toggleContext("restoreContext"),
  };

  const dispose = () => {
    viewportControl = undefined;
    measures.clear();
    for (const waiter of waiters) {
      globalThis.clearTimeout(waiter.timer);
      waiter.reject(new Error("Plant probe was disposed."));
    }
    waiters.clear();
  };

  function toggleContext(action: "loseContext" | "restoreContext"): boolean {
    const context = activeContext?.context;
    if (!context) {
      return false;
    }
    const extension =
      loseContextExtensions.get(context) ?? context.getExtension("WEBGL_lose_context");
    if (!extension) {
      return false;
    }
    loseContextExtensions.set(context, extension);
    extension[action]();
    return true;
  }

  return { api, sink, dispose };
}

export function installPlantProbe(target: Window = window): PlantProbe {
  if (target.__plantProbe) {
    return target.__plantProbe;
  }
  const controller = createPlantProbeController();
  setPlantProbeSink(controller.sink);
  target.__plantProbe = controller.api;
  return controller.api;
}

function copyProjectionEntry(entry: PlantProjectionEntry): PlantProjectionEntry {
  return {
    id: entry.id,
    ...(entry.anchorId === undefined ? {} : { anchorId: entry.anchorId }),
    kind: entry.kind,
    expected: { ...entry.expected },
    actual: { ...entry.actual },
    drift: { ...entry.drift },
    ...(entry.visible === undefined ? {} : { visible: entry.visible }),
  };
}

function copyOverlay(overlay: PlantOverlayMeasurement): PlantOverlayMeasurement {
  return {
    ...overlay,
    canvas: { ...overlay.canvas },
    safeArea: { ...overlay.safeArea },
    ...(overlay.inspector ? { inspector: { ...overlay.inspector } } : {}),
    entries: overlay.entries.map(copyOverlayEntry),
    drift: { ...overlay.drift },
    occlusion: { ...overlay.occlusion },
    hitTargets: { ...overlay.hitTargets },
  };
}

function copyOverlayEntry(entry: PlantOverlayEntry): PlantOverlayEntry {
  return {
    ...entry,
    projected: { ...entry.projected },
    ...(entry.dom ? { dom: { ...entry.dom } } : {}),
    ...(entry.drift ? { drift: { ...entry.drift } } : {}),
    hit: { ...entry.hit },
    ...(entry.label ? { label: { ...entry.label } } : {}),
  };
}

function copyModel(model: PlantModelMeasurement): PlantModelMeasurement {
  return {
    ...model,
    counts: { ...model.counts },
    risk: { ...model.risk },
  };
}

function copyVisual(visual: PlantVisualMeasurement): PlantVisualMeasurement {
  return {
    ...visual,
    contrast: { ...visual.contrast },
    palette: { ...visual.palette },
  };
}

function copyLayout(layout: PlantLayoutMeasurement): PlantLayoutMeasurement {
  return {
    ...layout,
    counts: { ...layout.counts },
    bounds: {
      world: { ...layout.bounds.world },
      projected: { ...layout.bounds.projected },
    },
    collisions: { ...layout.collisions },
    drawCalls: { ...layout.drawCalls },
    dom: { ...layout.dom },
  };
}

function copyViewport(viewport: PlantViewportMeasurement): PlantViewportMeasurement {
  return {
    ...viewport,
    camera: { ...viewport.camera },
    viewport: { ...viewport.viewport },
    world: { ...viewport.world },
    document: { ...viewport.document },
    ...(viewport.safeArea ? { safeArea: { ...viewport.safeArea } } : {}),
    ...(viewport.inspector ? { inspector: { ...viewport.inspector } } : {}),
  };
}
