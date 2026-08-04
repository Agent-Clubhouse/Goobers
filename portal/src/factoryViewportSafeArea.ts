/**
 * The one inspector safe-area constant set.
 *
 * The Factory viewport camera, the WebGL camera fit, the semantic overlay, the
 * probe and the stylesheet all have to agree on exactly how much of the stage
 * the open inspector covers. They agree by sharing these numbers: the
 * stylesheet reads them as custom properties, everything else imports them.
 * A second copy of "350px" anywhere is how a selected machine ends up hidden
 * behind the drawer that was opened to describe it.
 */

import { insetPlantRect, plantScreenRect, type PlantScreenRect } from "./plantProjection";

/** Drawer width in CSS pixels; mirrored by `--factory-inspector-width`. */
export const FACTORY_INSPECTOR_WIDTH = 350;
/** Drawer inset from the viewport edge; mirrored by `--factory-inspector-inset`. */
export const FACTORY_INSPECTOR_INSET = 8;
/** Bottom-sheet inset; mirrored by `--factory-inspector-narrow-inset`. */
export const FACTORY_INSPECTOR_NARROW_INSET = 6;
/** Clear space kept between the drawer and the stage content. */
export const FACTORY_INSPECTOR_GAP = 12;
/** Below this viewport width the drawer becomes a bottom sheet. */
export const FACTORY_INSPECTOR_NARROW_MAX_WIDTH = 900;
/** Bottom-sheet top edge, as a fraction of viewport height. */
export const FACTORY_INSPECTOR_SHEET_TOP_RATIO = 0.3;
/** Below this height the inspector is a compact bottom summary. */
export const FACTORY_INSPECTOR_SHORT_MAX_HEIGHT = 600;
/** Maximum compact-summary height; mirrored by the short-height stylesheet. */
export const FACTORY_INSPECTOR_COMPACT_HEIGHT = 180;
/** Explicit browser gate: below this, Plant is not operationally usable. */
export const FACTORY_VIEWPORT_MIN_USABLE_HEIGHT = 84;
export const FACTORY_VIEWPORT_MIN_USABLE_WIDTH = 240;

export interface FactorySafeAreaInput {
  width: number;
  height: number;
  inspectorOpen: boolean;
  /** Bottom-sheet mode; defaults to the shared narrow-width rule. */
  narrow?: boolean;
  /** Short-screen mode; defaults to the viewport height for direct callers. */
  short?: boolean;
}

export function isNarrowFactoryViewport(width: number): boolean {
  return Number.isFinite(width) && width <= FACTORY_INSPECTOR_NARROW_MAX_WIDTH;
}

export function isShortFactoryViewport(height: number): boolean {
  return (
    Number.isFinite(height) &&
    height > 0 &&
    height <= FACTORY_INSPECTOR_SHORT_MAX_HEIGHT
  );
}

/** The full viewport rectangle, obscured or not. */
export function factoryViewportRect(input: {
  width: number;
  height: number;
}): PlantScreenRect {
  return plantScreenRect(0, 0, Math.max(0, input.width), Math.max(0, input.height));
}

/**
 * The rectangle the open inspector covers, in viewport pixels.
 *
 * Returns undefined when the inspector is closed, which is the same answer as
 * "nothing is covered" but says so without inventing a zero-area rectangle.
 */
export function factoryInspectorRect(
  input: FactorySafeAreaInput,
): PlantScreenRect | undefined {
  if (!input.inspectorOpen) {
    return undefined;
  }
  const viewport = factoryViewportRect(input);
  if (viewport.width <= 0 || viewport.height <= 0) {
    return undefined;
  }
  const narrow = input.narrow ?? isNarrowFactoryViewport(viewport.width);
  const short = input.short ?? isShortFactoryViewport(viewport.height);
  if (short) {
    const available = Math.max(
      0,
      viewport.height -
        FACTORY_VIEWPORT_MIN_USABLE_HEIGHT -
        FACTORY_INSPECTOR_GAP -
        FACTORY_INSPECTOR_NARROW_INSET,
    );
    const height = Math.min(FACTORY_INSPECTOR_COMPACT_HEIGHT, available);
    return plantScreenRect(
      FACTORY_INSPECTOR_NARROW_INSET,
      viewport.height - FACTORY_INSPECTOR_NARROW_INSET - height,
      Math.max(0, viewport.width - FACTORY_INSPECTOR_NARROW_INSET * 2),
      height,
    );
  }
  if (narrow) {
    const top = viewport.height * FACTORY_INSPECTOR_SHEET_TOP_RATIO;
    return plantScreenRect(
      FACTORY_INSPECTOR_NARROW_INSET,
      top,
      Math.max(0, viewport.width - FACTORY_INSPECTOR_NARROW_INSET * 2),
      Math.max(0, viewport.height - top - FACTORY_INSPECTOR_NARROW_INSET),
    );
  }
  const width = Math.min(
    FACTORY_INSPECTOR_WIDTH,
    Math.max(0, viewport.width - FACTORY_INSPECTOR_INSET * 2),
  );
  return plantScreenRect(
    Math.max(0, viewport.width - FACTORY_INSPECTOR_INSET - width),
    FACTORY_INSPECTOR_INSET,
    width,
    Math.max(0, viewport.height - FACTORY_INSPECTOR_INSET * 2),
  );
}

/**
 * The unobscured rectangle the camera must fit into and labels must stay in.
 *
 * The drawer is an edge overlay, so the safe area is a single rectangle rather
 * than a subtracted polygon: the stage keeps the full remaining side (or the
 * full remaining top, on a bottom sheet).
 */
export function factoryViewportSafeArea(
  input: FactorySafeAreaInput,
): PlantScreenRect {
  const viewport = factoryViewportRect(input);
  const inspector = factoryInspectorRect(input);
  if (!inspector) {
    return viewport;
  }
  const narrow = input.narrow ?? isNarrowFactoryViewport(viewport.width);
  const short = input.short ?? isShortFactoryViewport(viewport.height);
  if (narrow || short) {
    // The inspector is the physical truth. On a very short viewport the
    // remaining strip can be tiny, but claiming a larger minimum would put the
    // camera and its hit targets underneath the bottom sheet.
    const height = Math.max(0, inspector.top - FACTORY_INSPECTOR_GAP);
    return plantScreenRect(viewport.left, viewport.top, viewport.width, height);
  }
  const width = Math.max(0, inspector.left - FACTORY_INSPECTOR_GAP);
  return plantScreenRect(viewport.left, viewport.top, width, viewport.height);
}

/**
 * Converts a viewport-space rectangle into the plant world's own pixels.
 *
 * The plant renders inside the viewport camera's transformed world, so the
 * overlay, the packer and the WebGL camera all need the safe area expressed in
 * unscaled canvas pixels.
 */
export function toPlantWorldRect(
  rect: PlantScreenRect,
  camera: { x: number; y: number; zoom: number },
): PlantScreenRect {
  const zoom = Number.isFinite(camera.zoom) && camera.zoom > 0 ? camera.zoom : 1;
  return plantScreenRect(
    (rect.left - camera.x) / zoom,
    (rect.top - camera.y) / zoom,
    rect.width / zoom,
    rect.height / zoom,
  );
}

/** Converts a plant world rectangle back into viewport pixels. */
export function toFactoryViewportRect(
  rect: PlantScreenRect,
  camera: { x: number; y: number; zoom: number },
): PlantScreenRect {
  const zoom = Number.isFinite(camera.zoom) && camera.zoom > 0 ? camera.zoom : 1;
  return plantScreenRect(
    rect.left * zoom + camera.x,
    rect.top * zoom + camera.y,
    rect.width * zoom,
    rect.height * zoom,
  );
}

/** The safe area a canvas of `canvasSize` sees, given the viewport safe rect. */
export function plantCanvasSafeArea(input: {
  canvas: PlantScreenRect;
  safeArea: PlantScreenRect;
  camera: { x: number; y: number; zoom: number };
  /** Extra clear space kept inside the safe rectangle. */
  padding?: number;
}): PlantScreenRect {
  const projected = toPlantWorldRect(input.safeArea, input.camera);
  const left = Math.max(input.canvas.left, projected.left);
  const top = Math.max(input.canvas.top, projected.top);
  const right = Math.min(input.canvas.right, projected.right);
  const bottom = Math.min(input.canvas.bottom, projected.bottom);
  if (right <= left || bottom <= top) {
    return input.canvas;
  }
  const clipped = plantScreenRect(left, top, right - left, bottom - top);
  return input.padding ? insetPlantRect(clipped, input.padding) : clipped;
}
