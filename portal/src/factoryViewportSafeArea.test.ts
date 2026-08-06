import { describe, expect, it } from "vitest";

import portalStyles from "./styles.css?raw";
import {
  FACTORY_INSPECTOR_GAP,
  FACTORY_INSPECTOR_INSET,
  FACTORY_INSPECTOR_NARROW_INSET,
  FACTORY_INSPECTOR_NARROW_MAX_WIDTH,
  FACTORY_INSPECTOR_SHORT_MAX_HEIGHT,
  FACTORY_INSPECTOR_SHEET_TOP_RATIO,
  FACTORY_INSPECTOR_WIDTH,
  FACTORY_VIEWPORT_MIN_USABLE_HEIGHT,
  factoryInspectorRect,
  factoryViewportRect,
  factoryViewportSafeArea,
  isNarrowFactoryViewport,
  plantCanvasSafeArea,
  toFactoryViewportRect,
  toPlantWorldRect,
} from "./factoryViewportSafeArea";
import { plantRectsOverlap, plantScreenRect } from "./plantProjection";

const WIDE = { height: 1000, width: 1440 };

describe("isNarrowFactoryViewport", () => {
  it("switches to the bottom sheet at the shared breakpoint", () => {
    expect(isNarrowFactoryViewport(FACTORY_INSPECTOR_NARROW_MAX_WIDTH)).toBe(
      true,
    );
    expect(
      isNarrowFactoryViewport(FACTORY_INSPECTOR_NARROW_MAX_WIDTH + 1),
    ).toBe(false);
  });
});

describe("factoryInspectorRect", () => {
  it("is undefined while the inspector is closed", () => {
    expect(
      factoryInspectorRect({ ...WIDE, inspectorOpen: false }),
    ).toBeUndefined();
  });

  it("occupies the right edge on a wide viewport", () => {
    const rect = factoryInspectorRect({ ...WIDE, inspectorOpen: true });
    expect(rect).toBeDefined();
    expect(rect?.width).toBe(FACTORY_INSPECTOR_WIDTH);
    expect(rect?.right).toBe(WIDE.width - FACTORY_INSPECTOR_INSET);
    expect(rect?.top).toBe(FACTORY_INSPECTOR_INSET);
  });

  it("becomes a bottom sheet on a narrow viewport", () => {
    const rect = factoryInspectorRect({
      height: 800,
      inspectorOpen: true,
      width: 640,
    });
    expect(rect).toBeDefined();
    expect(rect?.top).toBeCloseTo(800 * FACTORY_INSPECTOR_SHEET_TOP_RATIO, 6);
    expect(rect?.left).toBe(FACTORY_INSPECTOR_NARROW_INSET);
    expect(rect?.width).toBeGreaterThan(640 - FACTORY_INSPECTOR_NARROW_INSET * 3);
  });
});

describe("factoryViewportSafeArea", () => {
  it("returns the whole viewport when the inspector is closed", () => {
    expect(factoryViewportSafeArea({ ...WIDE, inspectorOpen: false })).toEqual(
      factoryViewportRect(WIDE),
    );
  });

  it("never intersects the open inspector", () => {
    for (const size of [
      { height: 1000, width: 1440 },
      { height: 800, width: 1280 },
      { height: 900, width: 1100 },
      { height: 800, width: 640 },
    ]) {
      const safe = factoryViewportSafeArea({ ...size, inspectorOpen: true });
      const inspector = factoryInspectorRect({ ...size, inspectorOpen: true });
      expect(inspector).toBeDefined();
      expect(plantRectsOverlap(safe, inspector!)).toBe(false);
      expect(safe.width).toBeGreaterThan(0);
      expect(safe.height).toBeGreaterThan(0);
    }
  });

  it("keeps a gap between the safe area and the inspector", () => {
    const safe = factoryViewportSafeArea({ ...WIDE, inspectorOpen: true });
    const inspector = factoryInspectorRect({ ...WIDE, inspectorOpen: true })!;
    expect(inspector.left - safe.right).toBeGreaterThanOrEqual(
      FACTORY_INSPECTOR_GAP,
    );
  });

  it("shrinks only horizontally on a wide viewport", () => {
    const open = factoryViewportSafeArea({ ...WIDE, inspectorOpen: true });
    const closed = factoryViewportSafeArea({ ...WIDE, inspectorOpen: false });
    expect(open.height).toBe(closed.height);
    expect(open.width).toBeLessThan(closed.width);
  });

  it("shrinks only vertically on a narrow viewport", () => {
    const size = { height: 800, width: 640 };
    const open = factoryViewportSafeArea({ ...size, inspectorOpen: true });
    const closed = factoryViewportSafeArea({ ...size, inspectorOpen: false });
    expect(open.width).toBe(closed.width);
    expect(open.height).toBeLessThan(closed.height);
  });

  it("restores the full viewport once the inspector closes", () => {
    const size = { height: 900, width: 1100 };
    factoryViewportSafeArea({ ...size, inspectorOpen: true });
    expect(factoryViewportSafeArea({ ...size, inspectorOpen: false })).toEqual(
      factoryViewportRect(size),
    );
  });

  it.each([
    { height: 320, width: 640 },
    { height: 200, width: 360 },
  ])(
    "keeps an explicit usable short-viewport area at $width x $height",
    ({ height, width }) => {
      const safe = factoryViewportSafeArea({
        height,
        inspectorOpen: true,
        width,
      });
      const inspector = factoryInspectorRect({
        height,
        inspectorOpen: true,
        width,
      })!;
      expect(safe.bottom).toBeLessThanOrEqual(
        inspector.top - FACTORY_INSPECTOR_GAP,
      );
      expect(plantRectsOverlap(safe, inspector)).toBe(false);
      expect(safe.height).toBeGreaterThanOrEqual(
        FACTORY_VIEWPORT_MIN_USABLE_HEIGHT,
      );
    },
  );

  it("degrades gracefully on a zero-sized viewport", () => {
    const safe = factoryViewportSafeArea({
      height: 0,
      inspectorOpen: true,
      width: 0,
    });
    expect(safe.width).toBe(0);
    expect(safe.height).toBe(0);
  });
});

describe("plant world and viewport conversion", () => {
  it("round-trips through the camera", () => {
    const camera = { x: 120, y: -40, zoom: 0.75 };
    const rect = plantScreenRect(10, 20, 300, 200);
    const world = toPlantWorldRect(rect, camera);
    const back = toFactoryViewportRect(world, camera);
    expect(back.left).toBeCloseTo(rect.left, 6);
    expect(back.top).toBeCloseTo(rect.top, 6);
    expect(back.width).toBeCloseTo(rect.width, 6);
    expect(back.height).toBeCloseTo(rect.height, 6);
  });

  it("treats a zero zoom as identity rather than dividing by it", () => {
    const world = toPlantWorldRect(plantScreenRect(0, 0, 10, 10), {
      x: 0,
      y: 0,
      zoom: 0,
    });
    expect(Number.isFinite(world.width)).toBe(true);
    expect(world.width).toBe(10);
  });
});

describe("plantCanvasSafeArea", () => {
  it("clips the viewport safe area to the canvas", () => {
    const safe = plantCanvasSafeArea({
      camera: { x: 0, y: 0, zoom: 1 },
      canvas: plantScreenRect(0, 0, 1450, 950),
      safeArea: plantScreenRect(0, 0, 5000, 5000),
    });
    expect(safe.left).toBe(0);
    expect(safe.top).toBe(0);
    expect(safe.width).toBeLessThanOrEqual(1450);
    expect(safe.height).toBeLessThanOrEqual(950);
  });

  it("moves with the camera", () => {
    const canvas = plantScreenRect(0, 0, 1450, 950);
    const safeArea = plantScreenRect(0, 0, 700, 600);
    const centred = plantCanvasSafeArea({
      camera: { x: 0, y: 0, zoom: 1 },
      canvas,
      safeArea,
    });
    // Panning the camera left moves the world under the safe rectangle right.
    const panned = plantCanvasSafeArea({
      camera: { x: -200, y: 0, zoom: 1 },
      canvas,
      safeArea,
    });
    expect(centred.left).toBe(0);
    expect(panned.left).toBe(200);
  });

  it("grows the covered world as the camera zooms out", () => {
    const canvas = plantScreenRect(0, 0, 1450, 950);
    const safeArea = plantScreenRect(0, 0, 700, 600);
    const near = plantCanvasSafeArea({
      camera: { x: 0, y: 0, zoom: 1 },
      canvas,
      safeArea,
    });
    const far = plantCanvasSafeArea({
      camera: { x: 0, y: 0, zoom: 0.5 },
      canvas,
      safeArea,
    });
    expect(far.width).toBeGreaterThan(near.width);
  });

  it("never returns an inverted rectangle when the camera is off-canvas", () => {
    const safe = plantCanvasSafeArea({
      camera: { x: 90_000, y: 90_000, zoom: 1 },
      canvas: plantScreenRect(0, 0, 1450, 950),
      safeArea: plantScreenRect(0, 0, 700, 600),
    });
    expect(safe.width).toBeGreaterThanOrEqual(0);
    expect(safe.height).toBeGreaterThanOrEqual(0);
  });
});

describe("stylesheet parity", () => {
  const css = portalStyles;

  /**
   * The inspector is positioned by CSS and reasoned about by the camera. If the
   * two ever disagree, Fit All quietly starts hiding things behind the drawer,
   * so the numbers are asserted equal rather than merely kept nearby.
   */
  it("declares the same inspector geometry the camera fits against", () => {
    expect(css).toContain(
      `--factory-inspector-width: ${FACTORY_INSPECTOR_WIDTH}px;`,
    );
    expect(css).toContain(
      `--factory-inspector-inset: ${FACTORY_INSPECTOR_INSET}px;`,
    );
    expect(css).toContain(
      `--factory-inspector-narrow-inset: ${FACTORY_INSPECTOR_NARROW_INSET}px;`,
    );
    expect(css).toContain(
      `--factory-inspector-sheet-top: ${FACTORY_INSPECTOR_SHEET_TOP_RATIO * 100}%;`,
    );
  });

  it("keeps the bottom-sheet breakpoint on the shared width", () => {
    expect(css).toContain(
      `@media (max-width: ${FACTORY_INSPECTOR_NARROW_MAX_WIDTH}px)`,
    );
  });

  it("keeps the compact-inspector height breakpoint in stylesheet parity", () => {
    expect(css).toContain(
      `@media (max-height: ${FACTORY_INSPECTOR_SHORT_MAX_HEIGHT}px)`,
    );
    expect(css).toContain(
      `minmax(${FACTORY_VIEWPORT_MIN_USABLE_HEIGHT}px, 1fr)`,
    );
  });
});
