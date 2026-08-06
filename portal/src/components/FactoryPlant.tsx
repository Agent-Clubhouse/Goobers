import {
  Component,
  lazy,
  Suspense,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type ComponentType,
  type ErrorInfo,
  type ReactNode,
} from "react";
import { FactoryFloor } from "./FactoryFloor";
import type {
  FactoryCarrier,
  FactoryFloorModel,
  FactoryLane,
  FactoryLens,
  FactoryStation,
  FactoryWorker,
  FactoryWorkerPlacement,
} from "../factoryModel";
import { carrierIsWorking } from "../factoryModel";
import {
  carrierLabel,
  laneLabel,
  machineStatusText,
  shortKind,
  stationLabel,
  workerLabel,
} from "../factoryLabels";
import {
  buildClassicPlant,
  CLASSIC_PLANT_HEIGHT,
  CLASSIC_PLANT_WIDTH,
  type ClassicPoint,
} from "../factoryClassicPlant";
import type { FactorySelection } from "../factorySelection";
import { isSelected } from "../factorySelection";
import {
  buildFactoryPlantLayout,
  type FactoryPlantAllocation,
  type PlantAggregateSummary,
} from "../factoryPlantLayout";
import {
  buildPlantOverlayItems,
  findPlantOverlayAnchorId,
  type PlantOverlayItem,
} from "../factoryPlantOverlay";
import { getPlantProbeSink } from "../plantProbeSink";
import { plantForcedColorsActive } from "../plantForcedColors";
import {
  PLANT_READ_CURRENT,
  assessCarrierRisk,
  assessStationRisk,
  plantRiskLevelLabel,
  plantRiskMarkerShape,
  summarizePlantRisk,
  type PlantReadState,
} from "../plantRisk";
import {
  centeredPlantScreenRect,
  plantScreenRect,
  type PlantProjectionController,
  type PlantScreenPoint,
} from "../plantProjection";
import { FactoryPlantOverlay } from "./FactoryPlantOverlay";
import type {
  FactoryWebGLSceneProps,
  RendererState,
} from "./FactoryWebGLScene";
import { useFactoryViewportCamera } from "./FactoryViewport";

type FactoryWebGLSceneComponent = ComponentType<FactoryWebGLSceneProps>;
type FactoryWebGLSceneModule = {
  FactoryWebGLScene: FactoryWebGLSceneComponent;
};
type FactoryWebGLSceneLoader = () => Promise<FactoryWebGLSceneModule>;

declare global {
  interface Window {
    /** Browser-harness seam; ignored unless the Plant probe is requested. */
    __plantRendererImport?: FactoryWebGLSceneLoader;
  }
}

const loadFactoryWebGLScene: FactoryWebGLSceneLoader = () => {
  if (
    typeof window !== "undefined" &&
    new URLSearchParams(window.location.search).get("plant-probe") === "1" &&
    window.__plantRendererImport
  ) {
    return window.__plantRendererImport();
  }
  return import("./FactoryWebGLScene");
};

function lazyFactoryWebGLScene(loader: FactoryWebGLSceneLoader) {
  return lazy(async () => {
    const module = await loader();
    return { default: module.FactoryWebGLScene };
  });
}

/**
 * The boss's-window plant view.
 *
 * WebGL draws the operating hall when the browser supports it; the approved
 * factory illustration stays mounted underneath as the automatic fallback.
 *
 * There is exactly one coordinate system at a time. When the renderer is ready
 * every positioned semantic is placed by the live camera through the screen
 * space overlay; when it is pending, fallen back, or unavailable the classic
 * bitmap and its classic coordinates are used instead. The two are never mixed,
 * because a hit target on one camera and a drawing on another is precisely the
 * ~734 px drift this replaced.
 */
export function FactoryPlant({
  animateTransitions,
  createRuntime,
  loadRenderer = loadFactoryWebGLScene,
  readState = PLANT_READ_CURRENT,
  model,
  lens,
  onSelect,
  reducedMotion,
  selection,
}: {
  animateTransitions: boolean;
  /** Injection point for tests; production always builds the real runtime. */
  createRuntime?: FactoryWebGLSceneProps["createRuntime"];
  /** Injection point for rejected-import and retry tests. */
  loadRenderer?: FactoryWebGLSceneLoader;
  /** Complete page/data/transport read truth. Never inferred from the scene. */
  readState?: PlantReadState;
  model: FactoryFloorModel;
  lens: FactoryLens;
  onSelect: (selection: FactorySelection) => void;
  reducedMotion: boolean;
  selection: FactorySelection;
}) {
  const scene = useMemo(() => buildClassicPlant(model), [model]);
  const allocationRef = useRef<FactoryPlantAllocation | undefined>(undefined);
  const layout = useMemo(() => {
    const next = buildFactoryPlantLayout(model, {
      previous: allocationRef.current,
    });
    allocationRef.current = next.allocation;
    return next;
  }, [model]);
  const working = model.carriers.some(carrierIsWorking);
  const risk = useMemo(
    () => summarizePlantRisk({ model, readState }),
    [model, readState],
  );
  const probe = getPlantProbeSink();
  const camera = useFactoryViewportCamera();
  const [rendererState, setRendererState] = useState<RendererState>("pending");
  const [rendererAttempt, setRendererAttempt] = useState(0);
  const [rendererImportFailed, setRendererImportFailed] = useState(false);
  const LazyFactoryWebGLScene = useMemo(
    () => lazyFactoryWebGLScene(loadRenderer),
    [loadRenderer, rendererAttempt],
  );
  const [controller, setController] = useState<PlantProjectionController | undefined>(
    undefined,
  );
  const [focusId, setFocusId] = useState<string | undefined>(undefined);
  const baysById = new Map(layout.bays.map((bay) => [bay.id, bay]));
  const stationsById = new Map(model.stations.map((station) => [station.id, station]));
  const workersById = new Map(model.workers.map((worker) => [worker.id, worker]));
  const workersByStation = new Map<string, FactoryWorker[]>();
  for (const worker of model.workers) {
    for (const stationId of worker.activeStationIds) {
      const workers = workersByStation.get(stationId) ?? [];
      workers.push(worker);
      workersByStation.set(stationId, workers);
    }
  }

  const overlayReady = rendererState === "ready" && controller !== undefined;
  const overlayItems = useMemo(
    () =>
      buildPlantOverlayItems({
        animateTransitions,
        ...(focusId === undefined ? {} : { focusId }),
        layout,
        lens,
        model,
        selection,
      }),
    [animateTransitions, focusId, layout, lens, model, selection],
  );
  const renderedOverlayItems = useMemo(() => {
    const compactOverview =
      camera.safeRect.height > 0 && camera.safeRect.height < 160;
    if (!compactOverview && overlayItems.length <= 80) {
      return overlayItems;
    }
    if (compactOverview) {
      return overlayItems.filter(
        (item) =>
          item.kind === "bay" || item.selected || item.critical,
      ).sort(compareOverlayPriority);
    }
    const bayAnchors = new Set(layout.lod.levels.bay.anchorIds);
    const detailAnchors = new Set(layout.lod.levels.detail.anchorIds);
    return overlayItems.filter(
      (item) =>
        (camera.zoom >= 0.75
          ? detailAnchors.has(item.anchorId)
          : bayAnchors.has(item.anchorId)) ||
        (item.critical && item.kind !== "bay") ||
        item.selected,
    ).sort(compareOverlayPriority);
  }, [
    camera.safeRect.height,
    camera.zoom,
    layout.lod.levels.bay.anchorIds,
    layout.lod.levels.detail.anchorIds,
    overlayItems,
  ]);
  const overlayViewport = useMemo(
    () => ({
      safeRect: camera.safeRect,
      x: camera.x,
      y: camera.y,
      zoom: camera.zoom,
    }),
    [camera.safeRect, camera.x, camera.y, camera.zoom],
  );
  const focusAnchors = useMemo(() => {
    const anchors = new Map<string, string>();
    for (const anchor of layout.overlayAnchors) {
      const key =
        anchor.kind === "overflow" && anchor.overflow
          ? plantFocusKey(anchor.kind, anchor.entityId, anchor.overflow.kind)
          : plantFocusKey(anchor.kind, anchor.entityId);
      anchors.set(key, anchor.id);
    }
    return anchors;
  }, [layout]);
  const sceneRef = useRef<HTMLDivElement>(null);
  const pendingFocusRef = useRef<string | undefined>(undefined);
  const overlayReadyRef = useRef(overlayReady);
  overlayReadyRef.current = overlayReady;

  const capturePlantFocus = useCallback(() => {
    const active = document.activeElement;
    const sceneElement = sceneRef.current;
    if (!(active instanceof HTMLElement) || !sceneElement?.contains(active)) {
      return;
    }
    const control = active.closest<HTMLElement>("[data-plant-focus-id]");
    if (control?.dataset.plantFocusId) {
      pendingFocusRef.current = control.dataset.plantFocusId;
    }
  }, []);

  useLayoutEffect(() => {
    const focusIdToRestore = pendingFocusRef.current;
    const sceneElement = sceneRef.current;
    if (!focusIdToRestore || !sceneElement) {
      return;
    }
    const target = Array.from(
      sceneElement.querySelectorAll<HTMLElement>("[data-plant-focus-id]"),
    ).find((element) => element.dataset.plantFocusId === focusIdToRestore);
    if (!target) {
      pendingFocusRef.current = undefined;
      return;
    }
    target.focus({ preventScroll: true });
    pendingFocusRef.current = undefined;
  }, [overlayReady]);

  // Selection and its critical operating context must stay visible. The
  // overlay anchors are the world truth, so the camera keeps their envelope
  // inside the unobscured rect when selection changes.
  const protectedOverlaySignature = overlayItems
    .filter((item) => item.selected || (item.critical && item.kind !== "bay"))
    .map(
      (item) =>
        `${item.id}:${item.world.x}:${item.world.y}:${item.world.z}:${item.hit.width}:${item.hit.height}`,
    )
    .join("|");

  useEffect(() => {
    if (!overlayReady || !controller) {
      return;
    }
    const selected = overlayItems.find(
      (item) => item.id === findPlantOverlayAnchorId(overlayItems, selection),
    );
    if (!selected) {
      return;
    }
    const zoom = camera.zoom > 0 ? camera.zoom : 1;
    const protectedRects = overlayItems
      .filter(
        (item) =>
          item.selected || (item.critical && item.kind !== "bay"),
      )
      .map((item) =>
        centeredPlantScreenRect(
          controller.projectEntity(item.entityId, item.world),
          Math.max(item.hit.width, 48) / zoom,
          Math.max(item.hit.height, 48) / zoom,
        ),
      );
    const left = Math.min(...protectedRects.map((rect) => rect.left));
    const top = Math.min(...protectedRects.map((rect) => rect.top));
    const right = Math.max(...protectedRects.map((rect) => rect.right));
    const bottom = Math.max(...protectedRects.map((rect) => rect.bottom));
    const envelope = plantScreenRect(left, top, right - left, bottom - top);
    camera.ensureVisible(envelope);
    // Re-run when zoom changes because fixed-size hit targets consume a
    // different share of the safe area. Deliberately ignore x/y pan so the
    // camera does not fight deliberate navigation.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [
    camera.safeRect.height,
    camera.safeRect.width,
    camera.zoom,
    controller,
    overlayReady,
    protectedOverlaySignature,
    selection,
  ]);

  const handleRendererState = useCallback(
    (state: RendererState) => {
      if ((state === "ready") !== overlayReadyRef.current) {
        capturePlantFocus();
      }
      setRendererState(state);
    },
    [capturePlantFocus],
  );
  const handleController = useCallback(
    (next: PlantProjectionController | undefined) => {
      if (overlayReadyRef.current && next === undefined) {
        capturePlantFocus();
      }
      setController(() => next);
    },
    [capturePlantFocus],
  );

  const handleRendererImportError = useCallback(() => {
    capturePlantFocus();
    setController(undefined);
    setRendererState("fallback");
    setRendererImportFailed(true);
    probe?.rendererState("fallback");
  }, [capturePlantFocus, probe]);

  const retryRendererImport = useCallback(() => {
    capturePlantFocus();
    setRendererImportFailed(false);
    setRendererState("pending");
    setRendererAttempt((attempt) => attempt + 1);
  }, [capturePlantFocus]);

  const pressRef = useRef<
    { x: number; y: number; moved: boolean } | undefined
  >(undefined);
  /**
   * Renderer picking, mapped back to semantic selection.
   *
   * Clicking the body of a machine selects the same station the DOM control
   * does, because the raycast resolves geometry to the registry key the overlay
   * item already carries. A pan gesture is not a click, so a moved pointer is
   * ignored.
   */
  const handleSceneClickCapture = useCallback(
    (event: React.MouseEvent<HTMLDivElement>) => {
      // A keyboard-generated click has no pointer depth. In that case the
      // focused semantic button remains authoritative for accessibility.
      if (event.detail === 0) {
        return;
      }
      if (
        (event.target as HTMLElement).closest(".factory-plant-overlay-chip")
      ) {
        // Chips are displaced aggregate controls, not geometry hit targets.
        // Their mouse and keyboard actions must remain identical.
        pressRef.current = undefined;
        return;
      }
      const press = pressRef.current;
      pressRef.current = undefined;
      if (
        !controller ||
        !overlayReady ||
        !press ||
        press.moved ||
        Math.hypot(event.clientX - press.x, event.clientY - press.y) > 4
      ) {
        if (press?.moved) {
          event.preventDefault();
          event.stopPropagation();
        }
        return;
      }
      const bounds = event.currentTarget.getBoundingClientRect();
      if (bounds.width <= 0 || bounds.height <= 0) {
        return;
      }
      const selection = resolvePlantPointerSelection(controller, overlayItems, {
        x: ((event.clientX - bounds.left) / bounds.width) * CLASSIC_PLANT_WIDTH,
        y: ((event.clientY - bounds.top) / bounds.height) * CLASSIC_PLANT_HEIGHT,
      });
      if (!selection) {
        // A packed label or expanded hit target may intentionally extend beyond
        // geometry. With no rendered semantic under the pointer, let the DOM
        // button perform its normal action.
        return;
      }
      event.preventDefault();
      event.stopPropagation();
      onSelect(selection);
    },
    [controller, onSelect, overlayItems, overlayReady],
  );

  useEffect(() => {
    probe?.model({
      lens,
      theme: document.documentElement.getAttribute("data-theme") ?? "light",
      reducedMotion,
      working,
      freshness: readState.query,
      readState,
      forcedColors: plantForcedColorsActive(),
      risk: {
        allClear: risk.allClear,
        complete: risk.complete,
        confirmed: risk.confirmed,
        detail: risk.detail,
        headline: risk.headline,
        level: risk.level,
        unknownCarriers: risk.carriers.unknown,
        unknownStations: risk.stations.unknown,
      },
      counts: {
        gaggles: model.counts.gaggles,
        workflows: model.counts.workflows,
        lanes: model.lanes.length,
        stations: model.stations.length,
        carriers: model.carriers.length,
        renderedCarriers: scene.carriers.length,
        workers: model.workers.length,
        renderedWorkers: scene.workers.length,
        activeRuns: model.counts.activeRuns,
        blockedRuns: model.counts.blockedRuns,
        unreadRuns: model.counts.unreadRuns,
        heldStages: model.counts.heldStages,
        blockedStages: model.counts.blockedStages,
      },
    });
  }, [lens, model, probe, readState, reducedMotion, risk, scene, working]);

  return (
    <div
      aria-label="Factory plant"
      className="factory-plant factory-plant-classic"
      data-lens={lens}
      data-motion={reducedMotion ? "reduced" : "full"}
      data-projection={overlayReady ? "webgl" : "classic"}
      data-responsive-layout="fit"
      data-working={working ? "true" : "false"}
      role="group"
      style={{ height: `${CLASSIC_PLANT_HEIGHT}px`, width: `${CLASSIC_PLANT_WIDTH}px` }}
    >
      <div
        className="factory-plant-scene"
        data-plant-canvas=""
        onBlurCapture={(event) => {
          const next = event.relatedTarget;
          if (!(next instanceof HTMLElement) || !event.currentTarget.contains(next)) {
            setFocusId(undefined);
          }
        }}
        onClickCapture={handleSceneClickCapture}
        onFocusCapture={(event) => {
          const control = (event.target as HTMLElement).closest<HTMLElement>(
            "[data-plant-focus-id]",
          );
          setFocusId(control?.dataset.plantFocusId);
        }}
        onPointerCancel={() => {
          pressRef.current = undefined;
        }}
        onPointerDown={(event) => {
          if (event.button === 0) {
            pressRef.current = {
              moved: false,
              x: event.clientX,
              y: event.clientY,
            };
          }
        }}
        onPointerMove={(event) => {
          const press = pressRef.current;
          if (
            press &&
            Math.hypot(event.clientX - press.x, event.clientY - press.y) > 4
          ) {
            press.moved = true;
          }
        }}
        ref={sceneRef}
      >
        <PlantRendererErrorBoundary
          key={rendererAttempt}
          onError={handleRendererImportError}
          fallback={<FactoryPlantFallback state="fallback" />}
        >
          <Suspense fallback={<FactoryPlantFallback state="pending" />}>
            <LazyFactoryWebGLScene
              animateTransitions={animateTransitions}
              {...(createRuntime ? { createRuntime } : {})}
              freshness={readState.query}
              layout={layout}
              lens={lens}
              model={model}
              onController={handleController}
              onRendererState={handleRendererState}
              reducedMotion={reducedMotion}
            />
          </Suspense>
        </PlantRendererErrorBoundary>

        {rendererImportFailed ? (
          <div className="factory-plant-renderer-status" role="status">
            <span>Enhanced renderer unavailable. Exact plant controls remain active.</span>
            <button onClick={retryRendererImport} type="button">
              Retry 3D renderer
            </button>
          </div>
        ) : null}

        {overlayReady && controller ? (
          <FactoryPlantOverlay
            animateTransitions={animateTransitions}
            controller={controller}
            {...(focusId === undefined ? {} : { focusId })}
            {...(camera.inspectorRect
              ? { inspectorRect: camera.inspectorRect }
              : {})}
            inspectorOpen={camera.inspectorRect !== undefined}
            items={renderedOverlayItems}
            maxControls={layout.lod.thresholds.maxDetailDomItems}
            onFocus={setFocusId}
            onSelect={onSelect}
            scale={camera.zoom > 0 ? 1 / camera.zoom : 1}
            touch={camera.narrow}
            viewport={overlayViewport}
          />
        ) : (
          <div className="factory-plant-exact-fallback" data-exact-topology="true">
            <div
              className="factory-plant-exact-fallback-world"
              style={{
                transform: `scale(${Math.min(
                  1,
                  CLASSIC_PLANT_WIDTH / model.width,
                  CLASSIC_PLANT_HEIGHT / model.height,
                )})`,
              }}
            >
              <FactoryFloor
                ariaLabel="Factory plant exact fallback topology"
                animateTransitions={animateTransitions}
                focusIdFor={(kind, entityId, overflow) =>
                  focusAnchors.get(plantFocusKey(kind, entityId, overflow))
                }
                lens={lens}
                model={model}
                onSelect={onSelect}
                reducedMotion={reducedMotion}
                selection={selection}
              />
            </div>
          </div>
        )}

        <div aria-hidden="true" className="factory-plant-hud factory-plant-hud-top">
          <span>
            {model.scope.gaggle ?? "All gaggles"} · {model.counts.activeRuns}
            {model.runsTruncated ? "+" : ""} work orders
          </span>
          <b data-risk={risk.level}>
            {lens === "risk"
              ? risk.headline
              : model.counts.blockedStages > 0
                  ? "ATTENTION REQUIRED"
                  : model.counts.heldStages > 0
                    ? "HUMAN HOLD"
                    : working
                      ? "FACTORY WORKING"
                    : model.counts.unreadRuns > 0
                      ? "SIGNALS INCOMPLETE"
                      : "PLANT READY"}
          </b>
        </div>
        {/*
         * The scene vocabulary, published where the hall is. Shape is the
         * primary channel and status the secondary one, so this legend names
         * both and uses exactly the words the inspector uses.
         */}
        <div aria-hidden="true" className="factory-plant-hud factory-plant-hud-bottom">
          <span><i data-shape="agentic" /> Agentic</span>
          <span><i data-shape="gate" /> Gate</span>
          <span><i data-shape="evaluator" /> Evaluator</span>
          <span><i data-shape="parallel" /> Parallel</span>
          <span><i data-shape="deterministic" /> Deterministic</span>
          <span><i data-tone="running" /> Running</span>
          <span><i data-tone="held" /> {plantRiskLevelLabel("held")}</span>
          <span><i data-tone="blocked" /> {plantRiskLevelLabel("blocked")}</span>
          <span><i data-tone="unknown" /> {plantRiskLevelLabel("unknown")}</span>
          <strong>{model.counts.goobers} goobers posted</strong>
        </div>
      </div>
    </div>
  );
}

function compareOverlayPriority(
  left: PlantOverlayItem,
  right: PlantOverlayItem,
): number {
  const rank = (item: PlantOverlayItem) =>
    item.selected ? 0 : item.focused ? 1 : item.critical ? 2 : item.kind === "bay" ? 3 : 4;
  return rank(left) - rank(right) || left.id.localeCompare(right.id);
}

/**
 * Resolves a pointer through the geometry that was actually drawn.
 *
 * DOM order is intentionally absent: overlapping semantic rectangles are an
 * accessibility layer, not a depth buffer. Keyboard clicks continue to invoke
 * their focused button directly; pointer clicks use this raycast result.
 */
export function resolvePlantPointerSelection(
  controller: PlantProjectionController,
  items: readonly PlantOverlayItem[],
  point: PlantScreenPoint,
): FactorySelection | undefined {
  const hit = controller.pick(point);
  if (!hit) {
    return undefined;
  }
  return items.find((item) => item.entityId === hit.key)?.selection;
}

function plantFocusKey(
  kind: string,
  entityId: string,
  overflow?: string,
): string {
  return overflow
    ? `${kind}\u0000${overflow}\u0000${entityId}`
    : `${kind}\u0000${entityId}`;
}

/**
 * The classic bitmap semantics.
 *
 * Kept intact for the 2D fallback: these controls are positioned in the fixed
 * 1450x950 illustration's own coordinates, which is correct only when that
 * illustration is what the user is looking at.
 */
function ClassicPlantControls({
  animateTransitions,
  baysById,
  focusAnchors,
  lens,
  model,
  onSelect,
  probeEnabled,
  scene,
  selection,
  stationsById,
  workersById,
  workersByStation,
}: {
  animateTransitions: boolean;
  baysById: Map<string, { summary: PlantAggregateSummary }>;
  focusAnchors: ReadonlyMap<string, string>;
  lens: FactoryLens;
  model: FactoryFloorModel;
  onSelect: (selection: FactorySelection) => void;
  probeEnabled: boolean;
  scene: ReturnType<typeof buildClassicPlant>;
  selection: FactorySelection;
  stationsById: Map<string, FactoryStation>;
  workersById: Map<string, FactoryWorker>;
  workersByStation: Map<string, FactoryWorker[]>;
}) {
  return (
    <>
      {scene.lanes.map(({ lane, sign }) => (
        <LaneSign
          baySummary={baysById.get(lane.id)?.summary}
          focusId={focusAnchors.get(plantFocusKey("bay", lane.id))}
          key={lane.id}
          lane={lane}
          onSelect={onSelect}
          partial={model.runsTruncated}
          point={sign}
          selected={isSelected(selection, { kind: "lane", id: lane.id })}
        />
      ))}

      {scene.stations.map(({ station, machine }) => (
        <StationCard
          focusId={focusAnchors.get(plantFocusKey("station", station.id))}
          key={station.id}
          machine={machine}
          lens={lens}
          onSelect={onSelect}
          probeId={probeEnabled ? station.id : undefined}
          selected={isSelected(selection, { kind: "station", id: station.id })}
          station={station}
          workers={workersByStation.get(station.id) ?? []}
        />
      ))}

      {scene.carriers.map(({ carrier, point }) => (
        <Carrier
          animateTransitions={animateTransitions}
          carrier={carrier}
          focusId={focusAnchors.get(plantFocusKey("carrier", carrier.runId))}
          key={carrier.runId}
          lens={lens}
          onSelect={onSelect}
          point={point}
          probeId={probeEnabled ? carrier.runId : undefined}
          selected={isSelected(selection, { kind: "run", id: carrier.runId })}
        />
      ))}

      {scene.workers.map(({ placement, point, workerId }) => {
        const worker = workersById.get(workerId);
        return worker ? (
          <Worker
            focusId={focusAnchors.get(
              plantFocusKey("worker", placement.id),
            )}
            key={placement.id}
            onSelect={onSelect}
            placement={placement}
            point={point}
            probeId={probeEnabled ? placement.id : undefined}
            selected={isSelected(selection, { kind: "worker", id: worker.id })}
            working={
              placement.stationId
                ? stationsById.get(placement.stationId)?.status === "running"
                : false
            }
            worker={worker}
          />
        ) : null;
      })}

      {model.lanes.flatMap((lane) => {
        const buttons = [];
        if (lane.yard.overflowRunCount > 0) {
          const inbound = scene.lanes.find((item) => item.lane.id === lane.id)?.inbound;
          if (inbound) {
            buttons.push(
              <button
                aria-label={`${lane.yard.overflowRunCount} additional runs waiting at inbound for ${lane.displayName}. Select the workflow line.`}
                className="factory-overflow factory-plant-overflow"
                data-plant-focus-id={focusAnchors.get(
                  plantFocusKey("overflow", lane.id, "queued"),
                )}
                key={`${lane.id}-inbound-overflow`}
                onClick={() => onSelect({ kind: "lane", id: lane.id })}
                style={pointStyle({ x: inbound.x - 20, y: inbound.y + 58 })}
                type="button"
              >
                +{lane.yard.overflowRunCount} queued
              </button>,
            );
          }
        }
        for (const station of lane.stations) {
          const placement = scene.stations.find((item) => item.station.id === station.id);
          if (!placement) {
            continue;
          }
          if (station.overflowRunCount > 0) {
            buttons.push(
              <button
                aria-label={`${station.overflowRunCount} additional runs at stage ${station.stageId}. Select the stage to inspect all runs.`}
                className="factory-overflow factory-plant-overflow"
                data-plant-focus-id={focusAnchors.get(
                  plantFocusKey("overflow", station.id, "runs"),
                )}
                key={`${station.id}-run-overflow`}
                onClick={() => onSelect({ kind: "station", id: station.id })}
                style={pointStyle({
                  x: placement.machine.x - 18,
                  y: placement.machine.y + 62,
                })}
                type="button"
              >
                +{station.overflowRunCount} more
              </button>,
            );
          }
          if (station.workerOverflowCount > 0) {
            buttons.push(
              <button
                aria-label={`${station.workerOverflowCount} additional goobers at stage ${station.stageId}. Select the stage to inspect staffing.`}
                className="factory-overflow factory-plant-overflow factory-plant-overflow-staff"
                data-plant-focus-id={focusAnchors.get(
                  plantFocusKey("overflow", station.id, "staff"),
                )}
                key={`${station.id}-staff-overflow`}
                onClick={() => onSelect({ kind: "station", id: station.id })}
                style={pointStyle({
                  x: placement.machine.x + 65,
                  y: placement.machine.y + 40,
                })}
                type="button"
              >
                +{station.workerOverflowCount} staff
              </button>,
            );
          }
        }
        return buttons;
      })}

      {model.commons.overflowWorkerCount > 0 && (
        <button
          aria-label={`${model.commons.overflowWorkerCount} additional ready goobers. Select the floor summary.`}
          className="factory-overflow factory-plant-overflow"
          data-plant-focus-id={focusAnchors.get(
            plantFocusKey("overflow", "commons", "ready"),
          )}
          onClick={() => onSelect({ kind: "overview" })}
          style={pointStyle({ x: 835, y: 760 })}
          type="button"
        >
          +{model.commons.overflowWorkerCount} ready
        </button>
      )}
    </>
  );
}

function LaneSign({
  baySummary,
  focusId,
  lane,
  onSelect,
  partial,
  point,
  selected,
}: {
  baySummary?: PlantAggregateSummary;
  focusId?: string;
  lane: FactoryLane;
  onSelect: (selection: FactorySelection) => void;
  partial: boolean;
  point: ClassicPoint;
  selected: boolean;
}) {
  return (
    <button
      aria-label={laneLabel(lane, partial)}
      aria-pressed={selected}
      className="factory-plant-sign"
      data-blocked={lane.blockedRuns > 0 ? "true" : "false"}
      data-plant-focus-id={focusId}
      data-state={baySummary?.status ?? "idle"}
      onClick={() => onSelect({ kind: "lane", id: lane.id })}
      style={pointStyle(point)}
      type="button"
    >
      <span className="factory-plant-sign-title">
        {lane.gaggle} · {lane.workflow}
      </span>
      <span className="factory-plant-sign-gaggle">
        {lane.gaggleDisplayName} · {lane.displayName}
      </span>
      <span className="factory-plant-sign-readout">
        {lane.activeRuns}{partial ? "+" : ""} active
      </span>
      {baySummary && (
        <span className="factory-plant-sign-summary">
          {baySummary.stages} stages · WIP {baySummary.wip} · {baySummary.status}
        </span>
      )}
      {lane.source === "observed" && (
        <span className="factory-plant-sign-badge">
          {lane.stations.length === 0
            ? `${lane.stageCount} stages unread`
            : "observed topology · order unknown"}
        </span>
      )}
    </button>
  );
}

function StationCard({
  focusId,
  lens,
  machine,
  onSelect,
  probeId,
  selected,
  station,
  workers,
}: {
  focusId?: string;
  lens: FactoryLens;
  machine: ClassicPoint;
  onSelect: (selection: FactorySelection) => void;
  probeId?: string;
  selected: boolean;
  station: FactoryStation;
  workers: readonly FactoryWorker[];
}) {
  const risk = assessStationRisk(station);
  const showRisk =
    lens === "risk" && (risk.level !== "healthy" || risk.incomplete);
  const fill =
    station.saturation === undefined
      ? undefined
      : Math.min(100, Math.round(station.saturation * 100));
  return (
    <>
      <button
        aria-label={stationLabel(station, workers)}
        aria-pressed={selected}
        className="factory-plant-machine"
        data-alarm={station.alarm ?? "off"}
        data-kind={station.kind}
        data-plant-focus-id={focusId}
        data-plant-probe-id={probeId}
        data-plant-probe-kind={probeId ? "station" : undefined}
        data-source={station.source}
        data-start={station.isStart ? "true" : "false"}
        data-status={station.status}
        onClick={() => onSelect({ kind: "station", id: station.id })}
        style={pointStyle(machine)}
        type="button"
      >
        <span aria-hidden="true" className="factory-plant-machine-core">
          <i />
        </span>
        <span className="factory-plant-machine-tooltip">
          <span className="factory-plant-placard-head">
            <span className="factory-plant-placard-kind">{shortKind(station)}</span>
            <span className="factory-plant-placard-status">
              {station.alarm ? "ALARM" : machineStatusText(station)}
            </span>
          </span>
          <span className="factory-plant-placard-name">{station.stageId}</span>
          <span className="factory-plant-placard-foot">
            <span className="factory-plant-placard-wip">WIP {station.wip}</span>
            <span
              aria-hidden="true"
              className="factory-plant-placard-gauge"
              data-known={fill === undefined ? "false" : "true"}
            >
              <span style={{ width: `${fill ?? 0}%` }} />
            </span>
            <span>{station.limit === undefined ? "capacity ?" : `${station.wip} / ${station.limit}`}</span>
          </span>
        </span>
        {station.alarm && (
          <span aria-hidden="true" className="factory-plant-machine-alarm">
            {station.alarm === "blocked" ? "BLOCKED" : "HOLD"}
          </span>
        )}
        {showRisk ? (
          <span
            className="factory-plant-risk-label"
            data-risk={risk.level}
          >
            <i
              aria-hidden="true"
              className="factory-plant-risk-marker"
              data-shape={plantRiskMarkerShape(risk.level)}
            />
            {plantRiskLevelLabel(risk.level)}
          </span>
        ) : null}
        {station.alarm === "blocked" && <span className="sr-only">Alarm: stage blocked</span>}
        {station.alarm === "hold" && <span className="sr-only">Alarm: human gate hold</span>}
      </button>
    </>
  );
}

function Carrier({
  animateTransitions,
  carrier,
  focusId,
  lens,
  onSelect,
  point,
  probeId,
  selected,
}: {
  animateTransitions: boolean;
  carrier: FactoryCarrier;
  focusId?: string;
  lens: FactoryLens;
  onSelect: (selection: FactorySelection) => void;
  point: ClassicPoint;
  probeId?: string;
  selected: boolean;
}) {
  const moved = animateTransitions && carrier.transition?.kind === "stage-change";
  const risk = assessCarrierRisk(carrier);
  const showRisk =
    lens === "risk" && (risk.level !== "healthy" || risk.incomplete);
  return (
    <button
      aria-label={carrierLabel(carrier)}
      aria-pressed={selected}
      className={moved ? "factory-plant-crate is-transitioning" : "factory-plant-crate"}
      data-moved={moved ? "true" : "false"}
      data-plant-focus-id={focusId}
      data-plant-probe-id={probeId}
      data-plant-probe-kind={probeId ? "carrier" : undefined}
      data-state={carrier.state}
      onClick={() => onSelect({ kind: "run", id: carrier.runId })}
      style={pointStyle(point)}
      type="button"
    >
      <span aria-hidden="true" className="plant-crate-top" />
      <span aria-hidden="true" className="plant-crate-front" />
      <span aria-hidden="true" className="plant-crate-right" />
      <span aria-hidden="true" className="plant-crate-halo" />
      {showRisk ? (
        <span className="factory-plant-risk-label" data-risk={risk.level}>
          <i
            aria-hidden="true"
            className="factory-plant-risk-marker"
            data-shape={plantRiskMarkerShape(risk.level)}
          />
          {plantRiskLevelLabel(risk.level)}
        </span>
      ) : null}
      <span className="sr-only">{carrier.runId}</span>
    </button>
  );
}

function Worker({
  focusId,
  onSelect,
  placement,
  point,
  probeId,
  selected,
  working,
  worker,
}: {
  focusId?: string;
  onSelect: (selection: FactorySelection) => void;
  placement: FactoryWorkerPlacement;
  point: ClassicPoint;
  probeId?: string;
  selected: boolean;
  working: boolean;
  worker: FactoryWorker;
}) {
  return (
    <button
      aria-label={workerLabel(worker, placement)}
      aria-pressed={selected}
      className="factory-plant-staff"
      data-active={placement.active ? "true" : "false"}
      data-plant-focus-id={focusId}
      data-plant-probe-id={probeId}
      data-plant-probe-kind={probeId ? "worker" : undefined}
      data-working={working ? "true" : "false"}
      onClick={() => onSelect({ kind: "worker", id: worker.id })}
      style={pointStyle(point)}
      type="button"
    >
      <span aria-hidden="true" className="factory-plant-staff-head" />
      <span aria-hidden="true" className="factory-plant-staff-body" />
      <span aria-hidden="true" className="factory-plant-staff-tag">
        {worker.displayName.slice(0, 10)}
      </span>
    </button>
  );
}

function pointStyle(point: ClassicPoint): CSSProperties {
  return {
    left: `${(point.x / CLASSIC_PLANT_WIDTH) * 100}%`,
    top: `${(point.y / CLASSIC_PLANT_HEIGHT) * 100}%`,
  };
}

function FactoryPlantFallback({ state }: { state: "pending" | "fallback" }) {
  return (
    <div
      aria-hidden="true"
      className="factory-plant-renderer"
      data-webgl={state}
    >
      {/* Authored, not downloaded: the renderer chunk must not pull 540 KB. */}
      <div className="factory-plant-backdrop-authored" />
      {state === "fallback" ? (
        <img
          alt=""
          className="factory-plant-backdrop"
          draggable="false"
          src="/factory-plant-base.png"
        />
      ) : null}
    </div>
  );
}

class PlantRendererErrorBoundary extends Component<
  {
    children: ReactNode;
    fallback: ReactNode;
    onError: (error: Error) => void;
  },
  { failed: boolean }
> {
  state = { failed: false };

  static getDerivedStateFromError() {
    return { failed: true };
  }

  componentDidCatch(error: Error, _info: ErrorInfo) {
    this.props.onError(error);
  }

  render() {
    return this.state.failed ? this.props.fallback : this.props.children;
  }
}
