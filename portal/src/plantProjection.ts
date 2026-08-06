/**
 * The one screen-space projection contract for the Factory Plant.
 *
 * Everything downstream of the renderer — the semantic overlay, the label
 * packer, the probe, and the harness — works in exactly two coordinate
 * systems: confirmed world units, and CSS pixels inside the plant canvas.
 * The runtime supplies the live camera matrix and the canvas rectangle; this
 * module turns them into points. It deliberately imports neither Three.js nor
 * the DOM so the contract is provable without a GPU and stays out of the
 * renderer's lazy chunk.
 *
 * There is no second, hand-rolled camera. A projection that does not come from
 * the camera that drew the frame is a guess, and the drift this module exists
 * to remove was exactly that guess.
 */

/** A point in canvas CSS pixels. */
export interface PlantScreenPoint {
  x: number;
  y: number;
}

/** A confirmed world-space point, in the layout's units. */
export interface PlantProjectionWorldPoint {
  x: number;
  y: number;
  z: number;
}

/** An axis-aligned rectangle in canvas CSS pixels. */
export interface PlantScreenRect {
  left: number;
  top: number;
  right: number;
  bottom: number;
  width: number;
  height: number;
}

export type PlantProjectionSource = "webgl" | "classic";

/** A 16 element column-major view-projection matrix, as Three.js stores it. */
export type PlantViewProjectionMatrix = readonly number[];

export interface PlantProjectedPoint extends PlantScreenPoint {
  /** Normalised device depth; outside [-1, 1] means clipped by the camera. */
  depth: number;
  /** True when the point is inside the camera frustum and the canvas rect. */
  visible: boolean;
}

export interface PlantProjectionState {
  source: PlantProjectionSource;
  /**
   * Monotonic revision.
   *
   * Consumers compare revisions instead of deep-comparing matrices, so a
   * camera that did not move costs no React work.
   */
  revision: number;
  /** The canvas box in its own layout pixels, before any viewport transform. */
  canvas: PlantScreenRect;
  /** The unobscured rectangle inside the canvas, in the same pixels. */
  safeArea: PlantScreenRect;
  matrix: PlantViewProjectionMatrix;
}

export interface PlantPickResult {
  /** Entity family: machine, crate, worker, conveyor. */
  entity: string;
  /** Semantic identity: station id, run id, or placement id. */
  key: string;
  distance: number;
}

export interface PlantAnimatedProjection {
  /** Semantic identity used by the overlay and the renderer registry. */
  id: string;
  point: PlantProjectedPoint;
}

/**
 * The narrow surface the overlay needs from the renderer.
 *
 * The runtime implements it; tests implement it in four lines. Nothing in the
 * overlay reaches for a renderer, a canvas, or a camera directly.
 */
export interface PlantProjectionController {
  projection: () => PlantProjectionState;
  project: (point: PlantProjectionWorldPoint) => PlantProjectedPoint;
  /**
   * Projects an entity's current rendered position.
   *
   * Most entities use `point` unchanged. A confirmed carrier transfer may
   * temporarily override it so the DOM anchor and the crate share one clock.
   */
  projectEntity: (
    id: string,
    point: PlantProjectionWorldPoint,
  ) => PlantProjectedPoint;
  pick: (point: PlantScreenPoint) => PlantPickResult | undefined;
  subscribe: (listener: (state: PlantProjectionState) => void) => () => void;
  /**
   * Publishes only entities whose rendered position moved this frame.
   *
   * This is deliberately separate from camera subscriptions: carrier motion
   * updates a handful of DOM nodes imperatively instead of re-rendering the
   * complete semantic overlay through React.
   */
  subscribeAnimation: (
    listener: (entries: readonly PlantAnimatedProjection[]) => void,
  ) => () => void;
}

/** Maximum tolerated distance between a projected anchor and its DOM control. */
export const PLANT_PROJECTION_TOLERANCE_PX = 6;

export const PLANT_IDENTITY_MATRIX: PlantViewProjectionMatrix = Object.freeze([
  1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1,
]);

export function plantScreenRect(
  left: number,
  top: number,
  width: number,
  height: number,
): PlantScreenRect {
  const safeWidth = Math.max(0, finite(width));
  const safeHeight = Math.max(0, finite(height));
  const safeLeft = finite(left);
  const safeTop = finite(top);
  return {
    left: safeLeft,
    top: safeTop,
    right: safeLeft + safeWidth,
    bottom: safeTop + safeHeight,
    width: safeWidth,
    height: safeHeight,
  };
}

export function plantScreenRectFromEdges(
  left: number,
  top: number,
  right: number,
  bottom: number,
): PlantScreenRect {
  return plantScreenRect(left, top, right - left, bottom - top);
}

export function centeredPlantScreenRect(
  point: PlantScreenPoint,
  width: number,
  height: number,
): PlantScreenRect {
  return plantScreenRect(point.x - width / 2, point.y - height / 2, width, height);
}

export function plantRectCenter(rect: PlantScreenRect): PlantScreenPoint {
  return { x: rect.left + rect.width / 2, y: rect.top + rect.height / 2 };
}

/** Applies the outer FactoryViewport camera to a canvas-local point. */
export function plantPointToViewport(
  point: PlantScreenPoint,
  camera: { x: number; y: number; zoom: number },
): PlantScreenPoint {
  const zoom = Number.isFinite(camera.zoom) && camera.zoom > 0 ? camera.zoom : 1;
  return {
    x: point.x * zoom + finite(camera.x),
    y: point.y * zoom + finite(camera.y),
  };
}

/** Removes the outer FactoryViewport camera from a viewport-space point. */
export function plantPointFromViewport(
  point: PlantScreenPoint,
  camera: { x: number; y: number; zoom: number },
): PlantScreenPoint {
  const zoom = Number.isFinite(camera.zoom) && camera.zoom > 0 ? camera.zoom : 1;
  return {
    x: (point.x - finite(camera.x)) / zoom,
    y: (point.y - finite(camera.y)) / zoom,
  };
}

/** Applies the outer FactoryViewport camera to a canvas-local rectangle. */
export function plantRectToViewport(
  rect: PlantScreenRect,
  camera: { x: number; y: number; zoom: number },
): PlantScreenRect {
  const point = plantPointToViewport(
    { x: rect.left, y: rect.top },
    camera,
  );
  const zoom = Number.isFinite(camera.zoom) && camera.zoom > 0 ? camera.zoom : 1;
  return plantScreenRect(point.x, point.y, rect.width * zoom, rect.height * zoom);
}

export function plantRectsOverlap(
  left: PlantScreenRect,
  right: PlantScreenRect,
  epsilon = 0.01,
): boolean {
  return (
    left.left < right.right - epsilon &&
    left.right > right.left + epsilon &&
    left.top < right.bottom - epsilon &&
    left.bottom > right.top + epsilon
  );
}

export function plantRectContains(
  outer: PlantScreenRect,
  inner: PlantScreenRect,
  epsilon = 0.01,
): boolean {
  return (
    inner.left >= outer.left - epsilon &&
    inner.top >= outer.top - epsilon &&
    inner.right <= outer.right + epsilon &&
    inner.bottom <= outer.bottom + epsilon
  );
}

export function plantRectContainsPoint(
  rect: PlantScreenRect,
  point: PlantScreenPoint,
  epsilon = 0.01,
): boolean {
  return (
    point.x >= rect.left - epsilon &&
    point.x <= rect.right + epsilon &&
    point.y >= rect.top - epsilon &&
    point.y <= rect.bottom + epsilon
  );
}

export function intersectPlantRects(
  left: PlantScreenRect,
  right: PlantScreenRect,
): PlantScreenRect | undefined {
  const nextLeft = Math.max(left.left, right.left);
  const nextTop = Math.max(left.top, right.top);
  const nextRight = Math.min(left.right, right.right);
  const nextBottom = Math.min(left.bottom, right.bottom);
  if (nextRight <= nextLeft || nextBottom <= nextTop) {
    return undefined;
  }
  return plantScreenRectFromEdges(nextLeft, nextTop, nextRight, nextBottom);
}

export function plantRectOverlapArea(
  left: PlantScreenRect,
  right: PlantScreenRect,
): number {
  const overlap = intersectPlantRects(left, right);
  return overlap ? overlap.width * overlap.height : 0;
}

export function insetPlantRect(
  rect: PlantScreenRect,
  inset: number | { left?: number; top?: number; right?: number; bottom?: number },
): PlantScreenRect {
  const insets =
    typeof inset === "number"
      ? { bottom: inset, left: inset, right: inset, top: inset }
      : {
          bottom: inset.bottom ?? 0,
          left: inset.left ?? 0,
          right: inset.right ?? 0,
          top: inset.top ?? 0,
        };
  const left = rect.left + insets.left;
  const top = rect.top + insets.top;
  return plantScreenRect(
    left,
    top,
    Math.max(0, rect.width - insets.left - insets.right),
    Math.max(0, rect.height - insets.top - insets.bottom),
  );
}

export interface PlantClampResult {
  rect: PlantScreenRect;
  /** The rectangle had to move to stay inside the bounds. */
  clamped: boolean;
  /** The rectangle does not fit inside the bounds at all. */
  clipped: boolean;
}

/**
 * Slides a rectangle back inside its bounds without resizing it.
 *
 * A label that is wider or taller than the visible safe rectangle cannot be
 * shown truthfully at all, so it is reported clipped rather than shrunk into
 * something the operator would have to guess at.
 */
export function clampPlantRect(
  rect: PlantScreenRect,
  bounds: PlantScreenRect,
): PlantClampResult {
  const clipped = rect.width > bounds.width + 0.01 || rect.height > bounds.height + 0.01;
  let left = rect.left;
  let top = rect.top;
  if (left < bounds.left) {
    left = bounds.left;
  } else if (left + rect.width > bounds.right) {
    left = bounds.right - rect.width;
  }
  if (top < bounds.top) {
    top = bounds.top;
  } else if (top + rect.height > bounds.bottom) {
    top = bounds.bottom - rect.height;
  }
  if (clipped) {
    left = Math.max(bounds.left, Math.min(left, bounds.right - Math.min(rect.width, bounds.width)));
    top = Math.max(bounds.top, Math.min(top, bounds.bottom - Math.min(rect.height, bounds.height)));
  }
  const moved = Math.abs(left - rect.left) > 0.01 || Math.abs(top - rect.top) > 0.01;
  return {
    clamped: moved,
    clipped,
    rect: plantScreenRect(left, top, rect.width, rect.height),
  };
}

/**
 * Projects one world point through the camera that actually drew the frame.
 *
 * The matrix is `projection * viewInverse` in Three.js column-major order, so
 * this is the same arithmetic `Vector3.project` performs, without needing the
 * renderer in scope.
 */
export function projectPlantWorldPoint(
  point: PlantProjectionWorldPoint,
  matrix: PlantViewProjectionMatrix,
  canvas: PlantScreenRect,
): PlantProjectedPoint {
  const x = finite(point.x);
  const y = finite(point.y);
  const z = finite(point.z);
  const clipX = matrix[0] * x + matrix[4] * y + matrix[8] * z + matrix[12];
  const clipY = matrix[1] * x + matrix[5] * y + matrix[9] * z + matrix[13];
  const clipZ = matrix[2] * x + matrix[6] * y + matrix[10] * z + matrix[14];
  const clipW = matrix[3] * x + matrix[7] * y + matrix[11] * z + matrix[15];
  const w = Math.abs(clipW) < 1e-9 ? 1 : clipW;
  const ndcX = clipX / w;
  const ndcY = clipY / w;
  const ndcZ = clipZ / w;
  const screenX = canvas.left + ((ndcX + 1) / 2) * canvas.width;
  const screenY = canvas.top + ((1 - ndcY) / 2) * canvas.height;
  return {
    depth: ndcZ,
    visible:
      Number.isFinite(screenX) &&
      Number.isFinite(screenY) &&
      ndcZ >= -1.000001 &&
      ndcZ <= 1.000001 &&
      screenX >= canvas.left &&
      screenX <= canvas.right &&
      screenY >= canvas.top &&
      screenY <= canvas.bottom,
    x: screenX,
    y: screenY,
  };
}

/** Inverse of {@link projectPlantWorldPoint} for the x/y screen axes only. */
export function plantScreenToNdc(
  point: PlantScreenPoint,
  canvas: PlantScreenRect,
): PlantScreenPoint {
  const width = canvas.width || 1;
  const height = canvas.height || 1;
  return {
    x: ((point.x - canvas.left) / width) * 2 - 1,
    y: -(((point.y - canvas.top) / height) * 2 - 1),
  };
}

export interface PlantProjector {
  readonly source: PlantProjectionSource;
  readonly revision: number;
  readonly canvas: PlantScreenRect;
  readonly safeArea: PlantScreenRect;
  project: (point: PlantProjectionWorldPoint) => PlantProjectedPoint;
}

export function createPlantProjector(state: PlantProjectionState): PlantProjector {
  return {
    canvas: state.canvas,
    project: (point) => projectPlantWorldPoint(point, state.matrix, state.canvas),
    revision: state.revision,
    safeArea: state.safeArea,
    source: state.source,
  };
}

/**
 * Identity of a projection input set.
 *
 * The runtime emits a new revision only when this string changes, which is how
 * resize, zoom, layout and camera changes reach React exactly once each and an
 * operating animation reaches it not at all.
 */
export function plantProjectionSignature(
  state: Omit<PlantProjectionState, "revision">,
): string {
  return [
    state.source,
    rectSignature(state.canvas),
    rectSignature(state.safeArea),
    Array.from(state.matrix, (value) => round(value)).join(","),
  ].join("|");
}

function rectSignature(rect: PlantScreenRect): string {
  return `${round(rect.left)},${round(rect.top)},${round(rect.width)},${round(rect.height)}`;
}

function round(value: number): number {
  return Number.isFinite(value) ? Math.round(value * 1e4) / 1e4 : 0;
}

function finite(value: number): number {
  return Number.isFinite(value) ? value : 0;
}
