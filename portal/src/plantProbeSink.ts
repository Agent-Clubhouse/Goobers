import type { FactoryLens } from "./factoryModel";
import type {
  PLANT_CONTRAST_GATES,
  PLANT_LUMINANCE_BANDS,
  PlantContrastReport,
  PlantScenePalette,
  PlantTheme,
} from "./plantPalette";
import type { PlantFreshness, PlantRiskLevel } from "./plantRisk";
import type { DataFreshness, LiveFreshness } from "./liveData";

export type PlantRendererState = "pending" | "ready" | "fallback";

export interface PlantRendererInfo {
  calls: number;
  triangles: number;
  programs: number;
  geometries: number;
  textures: number;
}

export interface PlantCanvasMeasurement {
  cssWidth: number;
  cssHeight: number;
  renderedWidth: number;
  renderedHeight: number;
  backingWidth: number;
  backingHeight: number;
  devicePixelRatio: number;
}

/** What the Risk lens concluded, published exactly as the operator reads it. */
export interface PlantRiskMeasurement {
  level: PlantRiskLevel;
  confirmed: number;
  complete: boolean;
  allClear: boolean;
  headline: string;
  detail: string;
  unknownStations: number;
  unknownCarriers: number;
}

export interface PlantModelMeasurement {
  lens: FactoryLens;
  theme: string;
  reducedMotion: boolean;
  working: boolean;
  /** The page's own read state, threaded through rather than inferred. */
  freshness: PlantFreshness;
  readState: {
    query: PlantFreshness;
    data: DataFreshness;
    transport: LiveFreshness;
  };
  /** True when the OS forced-colors mode is active and WebGL is not mounted. */
  forcedColors: boolean;
  risk: PlantRiskMeasurement;
  counts: {
    gaggles: number;
    workflows: number;
    lanes: number;
    stations: number;
    carriers: number;
    renderedCarriers: number;
    workers: number;
    renderedWorkers: number;
    activeRuns: number;
    blockedRuns: number;
    unreadRuns: number;
    heldStages: number;
    blockedStages: number;
  };
}

/**
 * The visual contract the renderer is currently honouring.
 *
 * Published from the runtime so the browser harness asserts on the palette that
 * actually drove the frame rather than re-deriving one from CSS.
 */
export interface PlantVisualMeasurement {
  theme: PlantTheme;
  lens: FactoryLens;
  /** Must stay false: the Risk lens never erases the hall behind fog. */
  fog: boolean;
  palette: PlantScenePalette;
  contrast: PlantContrastReport;
  /**
   * The authored gates and bands, republished so the browser harness asserts
   * against the numbers this build actually holds rather than a copy that
   * quietly drifts.
   */
  gates: typeof PLANT_CONTRAST_GATES;
  bands: typeof PLANT_LUMINANCE_BANDS;
  /** Confirmed-hazard beacons and unread markers currently drawn. */
  markers: number;
  /** The legibility floor healthy context keeps in the Risk lens. */
  contextOpacity: number;
  staticDrawCalls: number;
  backingPixelCap: number;
}

export interface PlantViewportMeasurement {
  label: string;
  camera: {
    x: number;
    y: number;
    zoom: number;
    fitted: boolean;
  };
  viewport: {
    width: number;
    height: number;
    scrollWidth: number;
    scrollHeight: number;
    overflowX: string;
    overflowY: string;
  };
  world: {
    width: number;
    height: number;
    left: number;
    top: number;
    right: number;
    bottom: number;
  };
  document: {
    clientWidth: number;
    clientHeight: number;
    scrollWidth: number;
    scrollHeight: number;
    overflowX: boolean;
    overflowY: boolean;
  };
  inspectorOpen?: boolean;
  /** The unobscured viewport rectangle the camera fits into. */
  safeArea?: { x: number; y: number; width: number; height: number };
  /** What the open inspector covers, in the same pixels. */
  inspector?: { x: number; y: number; width: number; height: number };
}

export interface PlantViewportCameraPose {
  x: number;
  y: number;
  zoom: number;
}

export interface PlantViewportControl {
  setCamera: (pose: PlantViewportCameraPose) => void;
}

export interface PlantLayoutMeasurement {
  counts: {
    workflows: number;
    bayCells: number;
    stations: number;
    tracks: number;
    trackSegments: number;
    carriers: number;
    workers: number;
    batches: number;
    instances: number;
  };
  bounds: {
    world: {
      minX: number;
      minY: number;
      minZ: number;
      maxX: number;
      maxY: number;
      maxZ: number;
      width: number;
      height: number;
      depth: number;
    };
    projected: {
      minX: number;
      minY: number;
      maxX: number;
      maxY: number;
      width: number;
      height: number;
    };
  };
  collisions: {
    bayCells: number;
    machines: number;
    duplicateStationCoordinates: number;
  };
  unresolvedTracks: number;
  boundsFinite: boolean;
  drawCalls: {
    instancedPlan: number;
    currentRendererUpperBound: number;
    actual: number;
  };
  dom: {
    detailCandidates: number;
    detailLimit: number;
    baySummaries: number;
    overview: number;
    maxAtAnyLod: number;
  };
}

export type PlantProjectionKind = "station" | "carrier" | "worker";

export interface PlantEntityStats {
  created: number;
  replaced: number;
  updated: number;
  removed: number;
  live: number;
}

export interface PlantProjectionEntry {
  id: string;
  anchorId?: string;
  kind: PlantProjectionKind;
  expected: { x: number; y: number };
  actual: { x: number; y: number };
  drift: { x: number; y: number; distance: number };
  visible?: boolean;
}

/** One overlay anchor as it was projected and then laid out on screen. */
export interface PlantOverlayEntry {
  id: string;
  anchorId: string;
  kind: string;
  tier: string;
  /** Where both cameras place the anchor, in final FactoryViewport CSS pixels. */
  projected: { x: number; y: number };
  /** Where the rendered DOM anchor actually sits, in the same CSS pixels. */
  dom?: { x: number; y: number };
  drift?: { x: number; y: number; distance: number };
  hit: { x: number; y: number; width: number; height: number };
  label?: { x: number; y: number; width: number; height: number };
  clamped: boolean;
  clipped: boolean;
  collapsed: boolean;
  occluded: boolean;
  onScreen: boolean;
  selected: boolean;
  critical: boolean;
}

/**
 * The alignment truth for one overlay pass.
 *
 * Everything the harness asserts on is measured here rather than recomputed in
 * the browser: drift against the live camera, packing outcomes, and how much
 * of the plant the inspector is covering.
 */
export interface PlantOverlayMeasurement {
  source: "webgl" | "classic";
  revision: number;
  inspectorOpen: boolean;
  canvas: { x: number; y: number; width: number; height: number };
  safeArea: { x: number; y: number; width: number; height: number };
  inspector?: { x: number; y: number; width: number; height: number };
  entries: readonly PlantOverlayEntry[];
  drift: { max: number; mean: number; count: number };
  collisions: number;
  clipped: number;
  collapsed: number;
  chips: number;
  offscreen: number;
  occlusion: { total: number; critical: number; selected: number };
  hitTargets: { minWidth: number; minHeight: number; belowMinimum: number };
}

export interface PlantProbeSink {
  rendererState: (state: PlantRendererState) => void;
  rendererCreated: (input: {
    canvas: HTMLCanvasElement;
    context: WebGLRenderingContext | WebGL2RenderingContext;
  }) => void;
  rendererDisposed: (context: WebGLRenderingContext | WebGL2RenderingContext) => void;
  contextLost: () => void;
  contextRestored: () => void;
  sceneBuilt: () => void;
  sceneDisposed: () => void;
  /** One keyed reconciliation pass over the retained scene. */
  entities: (stats: PlantEntityStats) => void;
  motion: (enabled: boolean, animatedCount: number) => void;
  rafScheduled: () => void;
  frame: (input: {
    raf: boolean;
    info: PlantRendererInfo;
    canvas: PlantCanvasMeasurement;
  }) => void;
  environment: (input: {
    lens: FactoryLens;
    theme: string;
    reducedMotion: boolean;
    freshness: PlantFreshness;
  }) => void;
  model: (model: PlantModelMeasurement) => void;
  /** The palette, contrast and marker truth behind the frame just drawn. */
  visual: (measurement: PlantVisualMeasurement) => void;
  layout: (layout: PlantLayoutMeasurement) => void;
  viewport: (viewport: PlantViewportMeasurement) => void;
  /** Probe-only control used by the browser harness for exact camera poses. */
  viewportControl?: (control: PlantViewportControl | undefined) => void;
  projections: (entries: readonly PlantProjectionEntry[]) => void;
  overlay: (measurement: PlantOverlayMeasurement) => void;
  /**
   * Registers a measurement that can be re-run on demand.
   *
   * Producers publish measurements as they change; this lets a consumer that
   * has settled the page pull a fresh reading rather than inheriting one taken
   * mid-transition. Returns a disposer.
   */
  registerMeasure: (measure: () => void) => () => void;
}

let activeSink: PlantProbeSink | undefined;

export function getPlantProbeSink(): PlantProbeSink | undefined {
  return activeSink;
}

export function setPlantProbeSink(sink: PlantProbeSink | undefined): void {
  activeSink = sink;
}

export function plantProbeRequested(search: string): boolean {
  const query = search.startsWith("?") ? search.slice(1) : search;
  return new URLSearchParams(query).get("plant-probe") === "1";
}

export function measurePlantCanvas(
  canvas: HTMLCanvasElement,
): PlantCanvasMeasurement {
  const bounds = canvas.getBoundingClientRect();
  return {
    cssWidth: canvas.clientWidth,
    cssHeight: canvas.clientHeight,
    renderedWidth: bounds.width,
    renderedHeight: bounds.height,
    backingWidth: canvas.width,
    backingHeight: canvas.height,
    devicePixelRatio:
      canvas.clientWidth > 0 && canvas.clientHeight > 0
        ? Math.max(
            canvas.width / canvas.clientWidth,
            canvas.height / canvas.clientHeight,
          )
        : 0,
  };
}
