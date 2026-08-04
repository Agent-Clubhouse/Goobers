/**
 * The persistent WebGL Plant runtime.
 *
 * One mounted canvas owns exactly one renderer, one scene, one camera, one set
 * of listeners, and one frame scheduler for its whole life. React props change
 * the runtime; they never rebuild it. That is the difference between a plant
 * that keeps running while an operator flips theme or lens and one that tears
 * down the GPU context, restarts every animation phase, and leaks whatever the
 * teardown missed.
 *
 * The renderer, the frame clock, and the observers are injected so the runtime
 * can be exercised without a GPU.
 */

import * as THREE from "three";
import type { FactoryFloorModel, FactoryLens } from "../../factoryModel";
import {
  fitFactoryPlantCameraToSafeArea,
  type FactoryPlantLayout,
  type PlantCameraFit,
} from "../../factoryPlantLayout";
import { webGLMotionEnabled } from "../../factoryWebGL";
import {
  createPlantProjector,
  plantProjectionSignature,
  plantScreenRect,
  plantScreenToNdc,
  projectPlantWorldPoint,
  type PlantAnimatedProjection,
  type PlantPickResult,
  type PlantProjectedPoint,
  type PlantProjectionController,
  type PlantProjectionState,
  type PlantProjectionWorldPoint,
  type PlantScreenPoint,
  type PlantScreenRect,
} from "../../plantProjection";
import {
  measurePlantCanvas,
  type PlantLayoutMeasurement,
  type PlantProbeSink,
  type PlantProjectionEntry,
  type PlantRendererInfo,
  type PlantRendererState,
  type PlantVisualMeasurement,
} from "../../plantProbeSink";
import { measurePlantContrast, PLANT_CONTRAST_GATES, PLANT_LUMINANCE_BANDS } from "../../plantPalette";
import {
  PLANT_RISK_CONTEXT_OPACITY,
  type PlantFreshness,
} from "../../plantRisk";
import {
  buildPlantEntitySpecs,
  plantEntityRegistryKey,
  reconcilePlantEntities,
  type PlantEntityRecord,
  type PlantEntitySpec,
  type PlantReconcileStats,
  type PlantWorldPoint,
} from "./factoryPlantEntities";
import {
  createPlantEntityObject,
  createPlantInstanceScene,
  createPlantStatics,
  PlantGeometryCache,
  PlantResourceLedger,
  readPlantPalette,
  type PlantEntityObject,
  type PlantPalette,
} from "./factoryPlantSceneGraph";
import {
  createPlantScheduler,
  type PlantFrame,
  type PlantSchedulerHost,
} from "./factoryPlantScheduler";

/** The renderer surface the runtime depends on, satisfied by THREE.WebGLRenderer. */
export interface PlantRenderer {
  getContext: () => WebGLRenderingContext | WebGL2RenderingContext;
  setPixelRatio: (ratio: number) => void;
  setSize: (width: number, height: number, updateStyle?: boolean) => void;
  render: (scene: THREE.Scene, camera: THREE.Camera) => void;
  dispose: () => void;
  readonly info: {
    readonly memory: { readonly geometries: number; readonly textures: number };
    readonly programs?: { readonly length: number } | null;
    readonly render: { readonly calls: number; readonly triangles: number };
  };
}

export interface PlantRuntimeHost extends PlantSchedulerHost {
  pixelRatio: () => number;
  observeResize: (target: Element, callback: () => void) => (() => void) | undefined;
  observeDocumentVisibility: (
    callback: (visible: boolean) => void,
  ) => (() => void) | undefined;
  observeIntersection: (
    target: Element,
    callback: (visible: boolean) => void,
  ) => (() => void) | undefined;
}

export interface FactoryWebGLRuntimeUpdate {
  animateTransitions: boolean;
  layout: FactoryPlantLayout;
  lens: FactoryLens;
  model: FactoryFloorModel;
  reducedMotion: boolean;
  theme: string;
  /**
   * How the page's own read is doing.
   *
   * Passed in, never inferred. The Risk lens has to distinguish "nothing is
   * wrong" from "we could not read the floor", and a renderer cannot work that
   * out by looking at its own colours.
   */
  freshness: PlantFreshness;
  /**
   * The unobscured part of the canvas, in canvas CSS pixels.
   *
   * Optional for direct canvas embeddings. FactoryViewport normally navigates
   * the whole Plant rigidly, so its retained WebGL camera fits the full canvas
   * and does not react to outer pan or zoom poses.
   */
  safeArea?: PlantScreenRect;
}

export interface FactoryWebGLRuntimeOptions {
  canvas: HTMLCanvasElement;
  createRenderer?: (canvas: HTMLCanvasElement) => PlantRenderer;
  host?: PlantRuntimeHost;
  probe?: PlantProbeSink;
  onState?: (state: PlantRendererState) => void;
  readPalette?: () => PlantPalette;
}

export interface FactoryWebGLRuntimeReport {
  state: PlantRendererState;
  contextLost: boolean;
  documentHidden: boolean;
  offscreen: boolean;
  motion: boolean;
  animatedCount: number;
  activeTransfers: number;
  frames: number;
  scheduled: boolean;
  disposed: boolean;
  entityKeys: string[];
  reconciles: number;
  lastReconcile: PlantReconcileStats;
  resources: {
    geometries: number;
    disposals: number;
    doubleDisposals: number;
  };
  batches: number;
  cameraFit?: PlantCameraFit;
  projectionRevision: number;
  safeArea: PlantScreenRect;
}

export interface FactoryWebGLRuntime extends PlantProjectionController {
  update: (next: FactoryWebGLRuntimeUpdate) => void;
  resize: () => void;
  dispose: () => void;
  inspect: () => FactoryWebGLRuntimeReport;
  /** The single object under test in unit tests; not used by the adapter. */
  readonly scene: THREE.Scene;
}

const TRANSFER_SECONDS = 0.45;

/**
 * The GPU backing-store ceiling, in pixels.
 *
 * A 4K panel at devicePixelRatio 2 would otherwise ask for a 30-megapixel
 * buffer for a diagram that reads perfectly at a third of that.
 */
export const PLANT_MAX_BACKING_PIXELS = 4_000_000;

/** Clamps the device pixel ratio so the backing store stays inside budget. */
export function plantPixelRatio(
  requested: number,
  width: number,
  height: number,
): number {
  const safe = Math.max(0.5, Math.min(Number.isFinite(requested) ? requested : 1, 2));
  const area = Math.max(1, width * height);
  if (area * safe * safe <= PLANT_MAX_BACKING_PIXELS) {
    return safe;
  }
  return Math.max(0.5, Math.sqrt(PLANT_MAX_BACKING_PIXELS / area));
}

export function createPlantRuntimeHost(view: Window = window): PlantRuntimeHost {
  return {
    cancelAnimationFrame: (handle) => view.cancelAnimationFrame(handle),
    now: () => performance.now(),
    observeDocumentVisibility: (callback) => {
      if (typeof document === "undefined") {
        return undefined;
      }
      const listener = () => callback(document.visibilityState !== "hidden");
      document.addEventListener("visibilitychange", listener);
      return () => document.removeEventListener("visibilitychange", listener);
    },
    observeIntersection: (target, callback) => {
      if (typeof IntersectionObserver === "undefined") {
        return undefined;
      }
      const observer = new IntersectionObserver((entries) => {
        const entry = entries.at(-1);
        if (entry) {
          callback(entry.isIntersecting);
        }
      });
      observer.observe(target);
      return () => observer.disconnect();
    },
    observeResize: (target, callback) => {
      if (typeof ResizeObserver === "undefined") {
        const listener = () => callback();
        view.addEventListener("resize", listener);
        return () => view.removeEventListener("resize", listener);
      }
      const observer = new ResizeObserver(() => callback());
      observer.observe(target);
      return () => observer.disconnect();
    },
    pixelRatio: () => Math.min(view.devicePixelRatio || 1, 1.6),
    requestAnimationFrame: (callback) => view.requestAnimationFrame(callback),
  };
}

export function createDefaultPlantRenderer(canvas: HTMLCanvasElement): PlantRenderer {
  const renderer = new THREE.WebGLRenderer({
    alpha: true,
    antialias: true,
    canvas,
    powerPreference: "high-performance",
  });
  renderer.shadowMap.enabled = true;
  renderer.shadowMap.type = THREE.PCFSoftShadowMap;
  renderer.outputColorSpace = THREE.SRGBColorSpace;
  // Diagram-grade, not cinematic. A filmic curve quietly rolls off exactly the
  // highlights a status beacon depends on and drags a dark theme toward black.
  renderer.toneMapping = THREE.NoToneMapping;
  return renderer;
}

/**
 * Creates the runtime, or returns undefined when this browser cannot give us a
 * renderer. The caller keeps the approved image fallback mounted either way.
 */
export function createFactoryWebGLRuntime(
  options: FactoryWebGLRuntimeOptions,
): FactoryWebGLRuntime | undefined {
  const { canvas } = options;
  const host = options.host ?? createPlantRuntimeHost();
  const probe = options.probe;
  const readPalette = options.readPalette ?? (() => readPlantPalette());
  let renderer: PlantRenderer;
  try {
    renderer = (options.createRenderer ?? createDefaultPlantRenderer)(canvas);
  } catch {
    return undefined;
  }
  const context = renderer.getContext();
  probe?.rendererCreated({ canvas, context });

  const world = new THREE.Scene();
  const camera = new THREE.OrthographicCamera(-15, 15, 10, -10, 0.1, 100);
  camera.position.set(18, 19, 22);
  camera.lookAt(0, 0, 0);
  renderer.setPixelRatio(host.pixelRatio());

  const ledger = new PlantResourceLedger();
  const geometryCache = new PlantGeometryCache(ledger);
  const statics = createPlantStatics(world, ledger);
  const instanceScene = createPlantInstanceScene(world, geometryCache, ledger);
  const entities = new Map<string, PlantEntityRecord<PlantEntityObject>>();
  const animated: PlantEntityObject[] = [];
  /** Transitions already accounted for, so a remount cannot replay them. */
  const playedTransfers = new Map<string, string>();
  const activeTransfers = new Map<
    string,
    {
      elevation: number;
      from: PlantWorldPoint;
      id: string;
      start: number;
      to: PlantWorldPoint;
    }
  >();
  const animatedWorldPositions = new Map<string, PlantProjectionWorldPoint>();
  let projectionTargets: PlantEntitySpec[] = [];

  let palette = readPalette();
  statics.applyPalette(palette);

  let latest: FactoryWebGLRuntimeUpdate | undefined;
  let state: PlantRendererState = "pending";
  let contextLost = false;
  let documentHidden = false;
  let offscreen = false;
  let readyPending = false;
  let started = false;
  let disposed = false;
  let frames = 0;
  let reconciles = 0;
  let lastReconcile: PlantReconcileStats = {
    created: 0,
    live: 0,
    removed: 0,
    replaced: 0,
    updated: 0,
  };
  let width = 1;
  let height = 1;
  let cameraFit: PlantCameraFit | undefined;
  let safeArea: PlantScreenRect = plantScreenRect(0, 0, 1, 1);
  let projectionRevision = 0;
  let projectionState: PlantProjectionState = {
    canvas: plantScreenRect(0, 0, 1, 1),
    matrix: readViewProjection(camera),
    revision: 0,
    safeArea,
    source: "webgl",
  };
  let projectionSignature = plantProjectionSignature(projectionState);
  const projectionListeners = new Set<(state: PlantProjectionState) => void>();
  const animationListeners = new Set<
    (entries: readonly PlantAnimatedProjection[]) => void
  >();
  const raycaster = new THREE.Raycaster();

  const setState = (next: PlantRendererState) => {
    if (state === next) {
      return;
    }
    state = next;
    options.onState?.(next);
    probe?.rendererState(next);
  };

  const scheduler = createPlantScheduler({
    host,
    onSchedule: () => probe?.rafScheduled(),
    render: (frame) => renderFrame(frame),
  });

  function renderFrame(frame: PlantFrame) {
    if (disposed || contextLost) {
      return;
    }
    const transferFrame = advanceTransfers(frame.elapsed);
    for (const entity of animated) {
      entity.animate(frame.elapsed);
    }
    instanceScene.animate(frame.elapsed);
    renderer.render(world, camera);
    publishAnimatedProjections(transferFrame.ids);
    frames += 1;
    if (latest) {
      probe?.layout(
        measurePlantLayout(latest.layout, renderer.info.render.calls),
      );
    }
    probe?.frame({
      canvas: measurePlantCanvas(canvas),
      info: readRendererInfo(renderer),
      raf: frame.raf,
    });
    if (readyPending) {
      readyPending = false;
      setState("ready");
    }
    if (transferFrame.settled) {
      // The last confirmed transfer finished, so the only remaining reason to
      // keep a frame loop alive may have gone with it.
      syncMotion();
    }
    if (probe && transferFrame.ids.length > 0) {
      measure();
    }
  }

  function advanceTransfers(
    elapsed: number,
  ): { ids: string[]; settled: boolean } {
    if (activeTransfers.size === 0) {
      return { ids: [], settled: false };
    }
    const ids: string[] = [];
    for (const [key, transfer] of [...activeTransfers]) {
      const record = entities.get(key);
      if (!record) {
        activeTransfers.delete(key);
        instanceScene.setTransfer(transfer.id, undefined);
        animatedWorldPositions.delete(transfer.id);
        ids.push(transfer.id);
        continue;
      }
      const progress = Math.min(1, (elapsed - transfer.start) / TRANSFER_SECONDS);
      if (progress >= 1) {
        activeTransfers.delete(key);
        record.entity.setTransfer(undefined);
        instanceScene.setTransfer(transfer.id, undefined);
        animatedWorldPositions.delete(transfer.id);
        ids.push(transfer.id);
        continue;
      }
      const remaining = 1 - easeOut(progress);
      const offset = {
        x: (transfer.from.x - transfer.to.x) * remaining,
        z: (transfer.from.z - transfer.to.z) * remaining,
      };
      record.entity.setTransfer(offset);
      instanceScene.setTransfer(transfer.id, offset);
      animatedWorldPositions.set(transfer.id, {
        x: transfer.to.x + offset.x,
        y: transfer.elevation,
        z: transfer.to.z + offset.z,
      });
      ids.push(transfer.id);
    }
    return { ids, settled: activeTransfers.size === 0 };
  }

  function publishAnimatedProjections(ids: readonly string[]) {
    if (ids.length === 0 || animationListeners.size === 0) {
      return;
    }
    const unique = [...new Set(ids)].sort();
    const entries = unique.flatMap((id) => {
      const target = projectionTargets.find(
        (candidate) => candidate.projection?.id === id,
      );
      const spec = target?.projection;
      if (!target || !spec) {
        return [];
      }
      return [
        {
          id,
          point: projectEntity(id, {
            x: target.position.x,
            y: spec.elevation,
            z: target.position.z,
          }),
        },
      ];
    });
    for (const listener of [...animationListeners]) {
      listener(entries);
    }
  }

  function syncMotion() {
    const enabled =
      latest !== undefined &&
      !documentHidden &&
      !offscreen &&
      !contextLost &&
      webGLMotionEnabled(
        latest.lens,
        latest.reducedMotion,
        animated.length + activeTransfers.size,
      );
    scheduler.setMotion(enabled);
    probe?.motion(enabled, animated.length);
  }

  function reconcile(next: FactoryWebGLRuntimeUpdate) {
    const specs = buildPlantEntitySpecs({
      layout: next.layout,
      lens: next.lens,
      model: next.model,
    });
    const stats = reconcilePlantEntities(entities, specs, {
      create: (spec) => {
        const entity = createPlantEntityObject(spec, palette, geometryCache, ledger);
        // Identity stays visible in the scene graph, so a retained object can be
        // recognised without a side table.
        entity.object.name = plantEntityRegistryKey(spec.entity, spec.key);
        // Picking resolves geometry back to a semantic selection, so a raycast
        // hit and a DOM control click produce the identical selection.
        entity.object.userData.plantEntityKey = spec.projection?.id ?? spec.key;
        entity.object.userData.plantEntityKind = spec.projection?.kind ?? spec.entity;
        world.add(entity.object);
        return entity;
      },
      dispose: (entity) => entity.dispose(),
      update: (entity, spec) => entity.apply(spec, palette),
    });
    reconciles += 1;
    lastReconcile = stats;
    probe?.entities(stats);

    animated.length = 0;
    projectionTargets = [];
    for (const record of entities.values()) {
      if (record.spec.active) {
        animated.push(record.entity);
      }
      if (record.spec.projection) {
        projectionTargets.push(record.spec);
      }
    }
    return specs;
  }

  function syncTransfers(specs: readonly PlantEntitySpec[], next: FactoryWebGLRuntimeUpdate) {
    const play =
      next.animateTransitions && !next.reducedMotion && next.lens !== "risk";
    const live = new Set<string>();
    const changed: string[] = [];
    for (const spec of specs) {
      if (spec.entity !== "crate") {
        continue;
      }
      const registryKey = plantEntityRegistryKey("crate", spec.key);
      live.add(spec.key);
      if (!spec.transfer) {
        playedTransfers.delete(spec.key);
        continue;
      }
      if (playedTransfers.get(spec.key) === spec.transfer.signature) {
        continue;
      }
      playedTransfers.set(spec.key, spec.transfer.signature);
      if (!play) {
        continue;
      }
      const projection = spec.projection;
      if (!projection) {
        continue;
      }
      const offset = {
        x: spec.transfer.from.x - spec.position.x,
        z: spec.transfer.from.z - spec.position.z,
      };
      activeTransfers.set(registryKey, {
        elevation: projection.elevation,
        from: spec.transfer.from,
        id: projection.id,
        start: scheduler.state().elapsed,
        to: spec.position,
      });
      entities.get(registryKey)?.entity.setTransfer(offset);
      instanceScene.setTransfer(projection.id, offset);
      animatedWorldPositions.set(projection.id, {
        x: spec.transfer.from.x,
        y: projection.elevation,
        z: spec.transfer.from.z,
      });
      changed.push(projection.id);
    }
    for (const key of [...playedTransfers.keys()]) {
      if (!live.has(key)) {
        playedTransfers.delete(key);
      }
    }
    if (!play) {
      for (const [key, record] of entities) {
        if (activeTransfers.has(key)) {
          record.entity.setTransfer(undefined);
          instanceScene.setTransfer(record.spec.key, undefined);
        }
      }
      changed.push(...animatedWorldPositions.keys());
      activeTransfers.clear();
      animatedWorldPositions.clear();
    }
    if (changed.length > 0) {
      publishAnimatedProjections(changed);
    }
  }

  /**
   * Publishes an alignment measurement taken right now.
   *
   * The camera is published synchronously but the overlay commits on the next
   * animation frame, so a reading taken in the same tick as a camera change
   * can carry one frame of React latency. Consumers that need the settled
   * truth call `remeasure()` on the probe once the page is quiet; that runs
   * this same measurement against the committed DOM.
   */
  function measure() {
    if (!probe || disposed) {
      return;
    }
    probe.projections(
      measureProjectionTargets(
        projectionTargets,
        projectionState,
        animatedWorldPositions,
      ),
    );
  }

  function readVisualMeasurement(lens: FactoryLens): PlantVisualMeasurement {
    return {
      backingPixelCap: PLANT_MAX_BACKING_PIXELS,
      bands: PLANT_LUMINANCE_BANDS,
      contextOpacity: PLANT_RISK_CONTEXT_OPACITY,
      contrast: measurePlantContrast(palette),
      // Asserted rather than assumed: the Risk lens used to erase the hall
      // behind a fog wall, which is how "at risk" became "invisible".
      fog: world.fog !== null && world.fog !== undefined,
      gates: PLANT_CONTRAST_GATES,
      lens,
      markers: instanceScene.markers,
      palette,
      staticDrawCalls: statics.drawCalls,
      theme: palette.theme,
    };
  }

  function projectEntity(
    id: string,
    fallback: PlantProjectionWorldPoint,
  ): PlantProjectedPoint {
    return projectPlantWorldPoint(
      animatedWorldPositions.get(id) ?? fallback,
      projectionState.matrix,
      projectionState.canvas,
    );
  }

  /**
   * Publishes the camera that actually drew the frame.
   *
   * The overlay never re-derives a camera; it reads this. Emission is
   * dirty-checked on the full input signature so a plant that is only
   * animating never wakes React, while a resize, a layout change, a safe-area
   * change or a refit reaches it exactly once.
   */
  function syncProjection(): boolean {
    const next: Omit<PlantProjectionState, "revision"> = {
      canvas: plantScreenRect(0, 0, width, height),
      matrix: readViewProjection(camera),
      safeArea,
      source: "webgl",
    };
    const signature = plantProjectionSignature(next);
    if (signature === projectionSignature) {
      return false;
    }
    projectionSignature = signature;
    projectionRevision += 1;
    projectionState = { ...next, revision: projectionRevision };
    for (const listener of [...projectionListeners]) {
      listener(projectionState);
    }
    return true;
  }

  function applySize() {
    const parent = canvas.parentElement;
    const nextWidth = Math.max(1, parent?.clientWidth || canvas.clientWidth || 1);
    const nextHeight = Math.max(1, parent?.clientHeight || canvas.clientHeight || 1);
    width = nextWidth;
    height = nextHeight;
    safeArea = resolveSafeArea(latest?.safeArea, nextWidth, nextHeight);
    cameraFit = latest
      ? fitFactoryPlantCameraToSafeArea(
          latest.layout,
          { height: nextHeight, width: nextWidth },
          safeArea,
          { x: 1.8, y: 1.5 },
        )
      : undefined;
    const viewWidth = cameraFit?.viewWidth ?? 30;
    const viewHeight = cameraFit?.viewHeight ?? 20;
    camera.left = -viewWidth / 2;
    camera.right = viewWidth / 2;
    camera.top = viewHeight / 2;
    camera.bottom = -viewHeight / 2;
    if (cameraFit) {
      camera.near = cameraFit.near;
      camera.far = cameraFit.far;
      camera.position.set(
        cameraFit.position.x,
        cameraFit.position.y,
        cameraFit.position.z,
      );
      camera.lookAt(
        cameraFit.target.x,
        cameraFit.target.y,
        cameraFit.target.z,
      );
    }
    camera.updateProjectionMatrix();
    camera.updateMatrixWorld();
    renderer.setPixelRatio(
      plantPixelRatio(host.pixelRatio(), nextWidth, nextHeight),
    );
    renderer.setSize(nextWidth, nextHeight, false);
    syncProjection();
  }

  const resize = () => {
    if (disposed) {
      return;
    }
    applySize();
    if (started) {
      scheduler.requestRender();
    }
    measure();
  };

  const handleContextLost = (event: Event) => {
    event.preventDefault();
    contextLost = true;
    readyPending = false;
    scheduler.setPaused(true);
    probe?.contextLost();
    setState("fallback");
  };

  const handleContextRestored = () => {
    contextLost = false;
    probe?.contextRestored();
    // Three.js re-initializes its GL state on this same event. The runtime only
    // has to re-apply its own size and ask for a frame; "ready" is reported by
    // that frame, never by the event, so the fallback stays up until pixels
    // actually exist again.
    readyPending = true;
    setState("pending");
    applySize();
    scheduler.setPaused(false);
    syncMotion();
    scheduler.requestRender();
    measure();
  };

  canvas.addEventListener("webglcontextlost", handleContextLost);
  canvas.addEventListener("webglcontextrestored", handleContextRestored);

  const stopResize = host.observeResize(canvas.parentElement ?? canvas, resize);
  const stopVisibility = host.observeDocumentVisibility((visible) => {
    documentHidden = !visible;
    syncMotion();
  });
  const stopIntersection = host.observeIntersection(canvas, (visible) => {
    offscreen = !visible;
    syncMotion();
  });
  const stopMeasureRequests = probe?.registerMeasure(measure);

  return {
    dispose: () => {
      if (disposed) {
        return;
      }
      disposed = true;
      scheduler.dispose();
      stopResize?.();
      stopVisibility?.();
      stopIntersection?.();
      stopMeasureRequests?.();
      projectionListeners.clear();
      animationListeners.clear();
      canvas.removeEventListener("webglcontextlost", handleContextLost);
      canvas.removeEventListener("webglcontextrestored", handleContextRestored);
      for (const record of entities.values()) {
        record.entity.dispose();
      }
      entities.clear();
      animated.length = 0;
      activeTransfers.clear();
      animatedWorldPositions.clear();
      playedTransfers.clear();
      projectionTargets = [];
      instanceScene.dispose();
      statics.dispose();
      geometryCache.dispose();
      world.clear();
      world.fog = null;
      probe?.sceneDisposed();
      renderer.dispose();
      probe?.rendererDisposed(context);
    },
    inspect: () => ({
      activeTransfers: activeTransfers.size,
      animatedCount: animated.length,
      batches: instanceScene.drawCalls,
      ...(cameraFit ? { cameraFit } : {}),
      contextLost,
      disposed,
      documentHidden,
      entityKeys: [...entities.keys()],
      frames,
      lastReconcile,
      motion: scheduler.state().motion,
      offscreen,
      projectionRevision,
      reconciles,
      resources: {
        disposals: ledger.disposals,
        doubleDisposals: ledger.doubleDisposals,
        geometries: geometryCache.size,
      },
      safeArea,
      scheduled: scheduler.state().scheduled,
      state,
    }),
    pick: (point) => pickPlantEntity(raycaster, world, camera, point, projectionState),
    project: (point) =>
      projectPlantWorldPoint(point, projectionState.matrix, projectionState.canvas),
    projectEntity,
    projection: () => projectionState,
    resize,
    scene: world,
    subscribe: (listener) => {
      projectionListeners.add(listener);
      return () => {
        projectionListeners.delete(listener);
      };
    },
    subscribeAnimation: (listener) => {
      animationListeners.add(listener);
      return () => {
        animationListeners.delete(listener);
      };
    },
    update: (next) => {
      if (disposed) {
        return;
      }
      const themeChanged = latest?.theme !== next.theme;
      const layoutChanged = latest?.layout !== next.layout;
      const safeAreaChanged =
        plantProjectionSignature({
          canvas: plantScreenRect(0, 0, width, height),
          matrix: projectionState.matrix,
          safeArea: resolveSafeArea(next.safeArea, width, height),
          source: "webgl",
        }) !== projectionSignature;
      latest = next;
      if (themeChanged) {
        palette = readPalette();
        statics.applyPalette(palette);
      }
      statics.applyLayout(next.layout);
      instanceScene.apply(next.layout, next.lens === "risk", palette);
      const specs = reconcile(next);
      syncTransfers(specs, next);
      syncMotion();
      probe?.environment({
        freshness: next.freshness,
        lens: next.lens,
        reducedMotion: next.reducedMotion,
        theme: next.theme,
      });
      probe?.visual(readVisualMeasurement(next.lens));
      if (!started) {
        started = true;
        probe?.sceneBuilt();
        applySize();
        scheduler.renderNow();
        if (!contextLost) {
          setState("ready");
        }
      } else {
        if (layoutChanged || safeAreaChanged) {
          applySize();
        }
        scheduler.requestRender();
      }
      measure();
    },
  };
}

function easeOut(progress: number): number {
  return 1 - (1 - progress) ** 3;
}

function readRendererInfo(renderer: PlantRenderer): PlantRendererInfo {
  return {
    calls: renderer.info.render.calls,
    geometries: renderer.info.memory.geometries,
    programs: renderer.info.programs?.length ?? 0,
    textures: renderer.info.memory.textures,
    triangles: renderer.info.render.triangles,
  };
}

function measurePlantLayout(
  layout: FactoryPlantLayout,
  actualDrawCalls: number,
): PlantLayoutMeasurement {
  return {
    counts: {
      workflows: layout.aggregatePlan.workflows,
      bayCells: layout.bays.reduce(
        (total, bay) => total + bay.cells.length,
        0,
      ),
      stations: layout.aggregatePlan.stations,
      tracks: layout.aggregatePlan.tracks,
      trackSegments: layout.aggregatePlan.trackSegments,
      carriers: layout.aggregatePlan.carriers,
      workers: layout.aggregatePlan.workers,
      batches: layout.aggregatePlan.batches,
      instances: layout.aggregatePlan.instances,
    },
    bounds: {
      world: {
        minX: layout.worldBounds.min.x,
        minY: layout.worldBounds.min.y,
        minZ: layout.worldBounds.min.z,
        maxX: layout.worldBounds.max.x,
        maxY: layout.worldBounds.max.y,
        maxZ: layout.worldBounds.max.z,
        width: layout.worldBounds.size.x,
        height: layout.worldBounds.size.y,
        depth: layout.worldBounds.size.z,
      },
      projected: { ...layout.projectedBounds },
    },
    collisions: { ...layout.metrics.collisions },
    unresolvedTracks: layout.metrics.unresolvedTrackIds.length,
    boundsFinite: layout.metrics.boundsFinite,
    drawCalls: {
      instancedPlan: layout.aggregatePlan.drawCalls.instancedPlan,
      currentRendererUpperBound:
        layout.aggregatePlan.drawCalls.currentRendererUpperBound,
      actual: actualDrawCalls,
    },
    dom: { ...layout.aggregatePlan.dom },
  };
}

/**
 * Measures WebGL-projected anchors against their DOM hit targets.
 *
 * Both sides are resolved through the published {@link PlantProjectionState}
 * so the probe measures the same contract the overlay renders with; a drift
 * here is a real misalignment, never a second camera disagreeing with the
 * first.
 */
function measureProjectionTargets(
  targets: readonly PlantEntitySpec[],
  projection: PlantProjectionState,
  animatedWorldPositions: ReadonlyMap<string, PlantProjectionWorldPoint>,
): PlantProjectionEntry[] {
  const elements = new Map(
    Array.from(document.querySelectorAll<HTMLElement>("[data-plant-probe-id]")).map(
      (element) => [element.dataset.plantProbeId ?? "", element],
    ),
  );
  if (elements.size === 0) {
    return [];
  }
  const projector = createPlantProjector(projection);
  return targets.flatMap((target) => {
    const spec = target.projection;
    if (!spec) {
      return [];
    }
    const element = elements.get(spec.id);
    if (!element) {
      return [];
    }
    const expected = domAnchorPoint(element);
    if (!expected) {
      return [];
    }
    const actual = projector.project(
      animatedWorldPositions.get(spec.id) ?? {
        x: target.position.x,
        y: spec.elevation,
        z: target.position.z,
      },
    );
    const driftX = actual.x - expected.x;
    const driftY = actual.y - expected.y;
    return [
      {
        actual: { x: actual.x, y: actual.y },
        anchorId: spec.anchorId,
        drift: { distance: Math.hypot(driftX, driftY), x: driftX, y: driftY },
        expected,
        id: spec.id,
        kind: spec.kind,
        visible: actual.visible,
      },
    ];
  });
}

/**
 * Resolves the DOM anchor point of a probe target in canvas CSS pixels.
 *
 * Overlay items mark themselves with `data-plant-anchor-origin`: the element is
 * a zero-size box pinned to the projected point, so its rect *is* the anchor
 * and label packing cannot move it. Classic controls have no such box, so their
 * centre is used instead.
 */
function domAnchorPoint(element: HTMLElement): PlantScreenPoint | undefined {
  const canvas = element.closest<HTMLElement>("[data-plant-canvas]");
  const canvasBounds = canvas?.getBoundingClientRect();
  if (!canvas || !canvasBounds || canvasBounds.width <= 0 || canvasBounds.height <= 0) {
    return undefined;
  }
  const scaleX = canvasBounds.width / (canvas.offsetWidth || canvasBounds.width);
  const scaleY = canvasBounds.height / (canvas.offsetHeight || canvasBounds.height);
  if (scaleX <= 0 || scaleY <= 0) {
    return undefined;
  }
  const origin =
    element.querySelector<HTMLElement>("[data-plant-anchor-origin]") ?? element;
  const bounds = origin.getBoundingClientRect();
  return {
    x: (bounds.left + bounds.width / 2 - canvasBounds.left) / scaleX,
    y: (bounds.top + bounds.height / 2 - canvasBounds.top) / scaleY,
  };
}

/** Reads the view-projection matrix that actually drew the last frame. */
function readViewProjection(
  camera: THREE.OrthographicCamera,
): readonly number[] {
  camera.updateMatrixWorld();
  return new THREE.Matrix4()
    .multiplyMatrices(camera.projectionMatrix, camera.matrixWorldInverse)
    .toArray();
}

/** Clamps a requested safe area to the canvas; falls back to the whole canvas. */
function resolveSafeArea(
  requested: PlantScreenRect | undefined,
  width: number,
  height: number,
): PlantScreenRect {
  const canvas = plantScreenRect(0, 0, width, height);
  if (!requested) {
    return canvas;
  }
  const left = Math.max(canvas.left, Math.min(requested.left, canvas.right));
  const top = Math.max(canvas.top, Math.min(requested.top, canvas.bottom));
  const right = Math.min(canvas.right, Math.max(requested.right, left));
  const bottom = Math.min(canvas.bottom, Math.max(requested.bottom, top));
  const rect = plantScreenRect(left, top, right - left, bottom - top);
  return rect.width > 1 && rect.height > 1 ? rect : canvas;
}

/**
 * Maps a canvas point to the semantic entity drawn under it.
 *
 * Selection stays a semantic act: the raycast resolves geometry back to the
 * registry key stamped on the object graph, so picking a mesh and clicking the
 * DOM control produce the identical selection.
 */
function pickPlantEntity(
  raycaster: THREE.Raycaster,
  world: THREE.Scene,
  camera: THREE.OrthographicCamera,
  point: PlantScreenPoint,
  projection: PlantProjectionState,
): PlantPickResult | undefined {
  const ndc = plantScreenToNdc(point, projection.canvas);
  raycaster.setFromCamera(new THREE.Vector2(ndc.x, ndc.y), camera);
  const hits = raycaster.intersectObjects(world.children, true);
  const semanticHits: PlantPickResult[] = [];
  for (const hit of hits) {
    const instanceIds = hit.object.userData?.plantInstanceIds;
    if (
      Array.isArray(instanceIds) &&
      hit.instanceId !== undefined &&
      typeof instanceIds[hit.instanceId] === "string"
    ) {
      semanticHits.push({
        distance: hit.distance,
        entity:
          typeof hit.object.userData?.plantEntityKind === "string"
            ? hit.object.userData.plantEntityKind
            : "unknown",
        key: instanceIds[hit.instanceId],
      });
      continue;
    }
    let node: THREE.Object3D | null = hit.object;
    while (node) {
      const key = node.userData?.plantEntityKey;
      const kind = node.userData?.plantEntityKind;
      if (typeof key === "string" && key.length > 0) {
        semanticHits.push({
          distance: hit.distance,
          entity: typeof kind === "string" ? kind : "unknown",
          key,
        });
        break;
      }
      node = node.parent;
    }
  }
  semanticHits.sort(
    (left, right) =>
      left.distance - right.distance ||
      left.entity.localeCompare(right.entity) ||
      left.key.localeCompare(right.key),
  );
  return semanticHits[0];
}
