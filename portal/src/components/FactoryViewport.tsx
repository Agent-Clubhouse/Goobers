import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import {
  FACTORY_INSPECTOR_NARROW_MAX_WIDTH,
  FACTORY_INSPECTOR_SHORT_MAX_HEIGHT,
  factoryInspectorRect,
  factoryViewportRect,
  factoryViewportSafeArea,
} from "../factoryViewportSafeArea";
import { getPlantProbeSink } from "../plantProbeSink";
import {
  plantRectContains,
  plantScreenRect,
  type PlantScreenRect,
} from "../plantProjection";

const MIN_ZOOM = 0.2;
/**
 * Fit All must show the whole world inside the unobscured rectangle, so it is
 * allowed below the interactive floor: a short safe area (a narrow bottom
 * sheet, say) would otherwise clamp the zoom and push the world off-screen.
 */
const MIN_FIT_ZOOM = 0.02;
const MAX_ZOOM = 2;
const VIEWPORT_PADDING = 20;

function fitPadding(height: number): number {
  if (height <= 160) {
    return 4;
  }
  if (height <= 300) {
    return 10;
  }
  return VIEWPORT_PADDING;
}

interface Camera {
  x: number;
  y: number;
  zoom: number;
}

/**
 * The live camera the plant is drawn through, published to its children.
 *
 * This is the sole navigation camera: it rigidly transforms the classic image,
 * WebGL canvas, and semantic overlay together. The WebGL camera only composes
 * the 3D hall inside that canvas. Selection uses `ensureVisible` so opening the
 * inspector cannot hide the thing it was opened to describe.
 */
export interface FactoryViewportCameraContext {
  x: number;
  y: number;
  zoom: number;
  fitted: boolean;
  /** The whole viewport box, in viewport pixels. */
  viewportRect: PlantScreenRect;
  /** The unobscured part of the viewport, in viewport pixels. */
  safeRect: PlantScreenRect;
  /** What the inspector covers, in viewport pixels; absent when closed. */
  inspectorRect?: PlantScreenRect;
  narrow: boolean;
  /** Pans the minimum distance that brings a world rectangle into the safe rect. */
  ensureVisible: (rect: PlantScreenRect) => void;
}

const FALLBACK_CONTEXT: FactoryViewportCameraContext = {
  ensureVisible: () => {},
  fitted: true,
  narrow: false,
  safeRect: plantScreenRect(0, 0, 0, 0),
  viewportRect: plantScreenRect(0, 0, 0, 0),
  x: 0,
  y: 0,
  zoom: 1,
};

const CameraContext = createContext<FactoryViewportCameraContext>(FALLBACK_CONTEXT);

export function useFactoryViewportCamera(): FactoryViewportCameraContext {
  return useContext(CameraContext);
}

export function FactoryViewport({
  children,
  inspectorOpen = false,
  label,
  worldHeight,
  worldWidth,
}: {
  children: React.ReactNode;
  /** Drives the safe area; the camera fits the world into what is left. */
  inspectorOpen?: boolean;
  label: string;
  worldHeight: number;
  worldWidth: number;
}) {
  const viewportRef = useRef<HTMLDivElement>(null);
  const worldRef = useRef<HTMLDivElement>(null);
  const dragRef = useRef<{ pointerId: number; x: number; y: number } | undefined>(undefined);
  const [camera, setCamera] = useState<Camera>({ x: 0, y: 0, zoom: 1 });
  const [fitted, setFitted] = useState(true);
  const [size, setSize] = useState({ height: 0, width: 0 });
  const [windowMode, setWindowMode] = useState(() => ({
    narrow:
      typeof window !== "undefined" &&
      window.innerWidth <= FACTORY_INSPECTOR_NARROW_MAX_WIDTH,
    short:
      typeof window !== "undefined" &&
      window.innerHeight <= FACTORY_INSPECTOR_SHORT_MAX_HEIGHT,
  }));
  const probe = getPlantProbeSink();

  const safeRect = useMemo(
    () =>
      factoryViewportSafeArea({
        height: size.height,
        inspectorOpen,
        narrow: windowMode.narrow,
        short: windowMode.short,
        width: size.width,
      }),
    [inspectorOpen, size.height, size.width, windowMode],
  );
  const inspectorRect = useMemo(
    () =>
      factoryInspectorRect({
        height: size.height,
        inspectorOpen,
        narrow: windowMode.narrow,
        short: windowMode.short,
        width: size.width,
      }),
    [inspectorOpen, size.height, size.width, windowMode],
  );

  useEffect(() => {
    if (!probe?.viewportControl) {
      return;
    }
    probe.viewportControl({
      setCamera: (pose) => {
        setCamera({
          x: Number.isFinite(pose.x) ? pose.x : 0,
          y: Number.isFinite(pose.y) ? pose.y : 0,
          zoom: Math.min(
            MAX_ZOOM,
            Math.max(
              MIN_FIT_ZOOM,
              Number.isFinite(pose.zoom) ? pose.zoom : 1,
            ),
          ),
        });
        setFitted(false);
      },
    });
    return () => {
      probe.viewportControl?.(undefined);
    };
  }, [probe]);

  /**
   * Fit all: the whole world inside the *unobscured* rectangle.
   *
   * Fitting to the raw viewport and then covering 350 px of it with a drawer is
   * how "Fit all" stopped meaning all.
   */
  const fit = useCallback(() => {
    const viewport = viewportRef.current;
    if (!viewport) {
      return;
    }
    const rect = factoryViewportSafeArea({
      height: viewport.clientHeight,
      inspectorOpen,
      narrow: windowMode.narrow,
      short: windowMode.short,
      width: viewport.clientWidth,
    });
    const padding = fitPadding(rect.height);
    const usableWidth = Math.max(1, rect.width - padding * 2);
    const usableHeight = Math.max(1, rect.height - padding * 2);
    const zoom = Math.min(
      1,
      Math.max(
        MIN_FIT_ZOOM,
        Math.min(usableWidth / worldWidth, usableHeight / worldHeight),
      ),
    );
    setCamera({
      x: Math.round(rect.left + (rect.width - worldWidth * zoom) / 2),
      y: Math.round(rect.top + (rect.height - worldHeight * zoom) / 2),
      zoom,
    });
    setFitted(true);
  }, [inspectorOpen, windowMode, worldHeight, worldWidth]);

  useEffect(() => {
    const viewport = viewportRef.current;
    if (!viewport) {
      return;
    }
    const handleResize = () => {
      setWindowMode((current) => {
        const next = {
          narrow: window.innerWidth <= FACTORY_INSPECTOR_NARROW_MAX_WIDTH,
          short: window.innerHeight <= FACTORY_INSPECTOR_SHORT_MAX_HEIGHT,
        };
        return current.narrow === next.narrow && current.short === next.short
          ? current
          : next;
      });
      setSize((current) =>
        current.width === viewport.clientWidth &&
        current.height === viewport.clientHeight
          ? current
          : { height: viewport.clientHeight, width: viewport.clientWidth },
      );
      if (fitted) {
        fit();
      }
    };
    if (typeof ResizeObserver === "undefined") {
      window.addEventListener("resize", handleResize);
      handleResize();
      return () => window.removeEventListener("resize", handleResize);
    }
    const observer = new ResizeObserver(handleResize);
    observer.observe(viewport);
    window.addEventListener("resize", handleResize);
    handleResize();
    return () => {
      observer.disconnect();
      window.removeEventListener("resize", handleResize);
    };
  }, [fit, fitted]);

  // Opening or closing the inspector changes the safe area, so a fitted camera
  // must refit; a manually posed camera is left exactly where the user put it.
  useEffect(() => {
    if (fitted) {
      fit();
    }
    // Refit is driven by the inspector, not by every camera nudge.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [inspectorOpen]);

  const ensureVisible = useCallback(
    (rect: PlantScreenRect) => {
      const viewport = viewportRef.current;
      if (!viewport || rect.width <= 0 || rect.height <= 0) {
        return;
      }
      setCamera((current) => {
        const safe = factoryViewportSafeArea({
          height: viewport.clientHeight,
          inspectorOpen,
          narrow: windowMode.narrow,
          short: windowMode.short,
          width: viewport.clientWidth,
        });
        const target = plantScreenRect(
          rect.left * current.zoom + current.x,
          rect.top * current.zoom + current.y,
          rect.width * current.zoom,
          rect.height * current.zoom,
        );
        const paddingX = Math.min(
          VIEWPORT_PADDING,
          Math.max(0, (safe.width - target.width) / 2),
        );
        const paddingY = Math.min(
          VIEWPORT_PADDING,
          Math.max(0, (safe.height - target.height) / 2),
        );
        const padded = plantScreenRect(
          safe.left + paddingX,
          safe.top + paddingY,
          Math.max(0, safe.width - paddingX * 2),
          Math.max(0, safe.height - paddingY * 2),
        );
        if (plantRectContains(padded, target)) {
          return current;
        }
        let dx = 0;
        let dy = 0;
        if (target.width > padded.width) {
          dx = padded.left - target.left;
        } else if (target.left < padded.left) {
          dx = padded.left - target.left;
        } else if (target.right > padded.right) {
          dx = padded.right - target.right;
        }
        if (target.height > padded.height) {
          dy = padded.top - target.top;
        } else if (target.top < padded.top) {
          dy = padded.top - target.top;
        } else if (target.bottom > padded.bottom) {
          dy = padded.bottom - target.bottom;
        }
        if (dx === 0 && dy === 0) {
          return current;
        }
        return { ...current, x: current.x + dx, y: current.y + dy };
      });
    },
    [inspectorOpen, windowMode],
  );

  useLayoutEffect(() => {
    const viewport = viewportRef.current;
    const world = worldRef.current;
    if (!probe || !viewport || !world) {
      return;
    }
    const viewportBounds = viewport.getBoundingClientRect();
    const worldBounds = world.getBoundingClientRect();
    const styles = getComputedStyle(viewport);
    const documentElement = document.documentElement;
    probe.viewport({
      label,
      camera: { ...camera, fitted },
      viewport: {
        width: viewport.clientWidth,
        height: viewport.clientHeight,
        scrollWidth: viewport.scrollWidth,
        scrollHeight: viewport.scrollHeight,
        overflowX: styles.overflowX,
        overflowY: styles.overflowY,
      },
      world: {
        width: worldBounds.width,
        height: worldBounds.height,
        left: worldBounds.left - viewportBounds.left,
        top: worldBounds.top - viewportBounds.top,
        right: worldBounds.right - viewportBounds.left,
        bottom: worldBounds.bottom - viewportBounds.top,
      },
      document: {
        clientWidth: documentElement.clientWidth,
        clientHeight: documentElement.clientHeight,
        scrollWidth: documentElement.scrollWidth,
        scrollHeight: documentElement.scrollHeight,
        overflowX: documentElement.scrollWidth > documentElement.clientWidth + 1,
        overflowY: documentElement.scrollHeight > documentElement.clientHeight + 1,
      },
      inspectorOpen,
      safeArea: {
        x: safeRect.left,
        y: safeRect.top,
        width: safeRect.width,
        height: safeRect.height,
      },
      ...(inspectorRect
        ? {
            inspector: {
              x: inspectorRect.left,
              y: inspectorRect.top,
              width: inspectorRect.width,
              height: inspectorRect.height,
            },
          }
        : {}),
    });
  }, [
    camera,
    fitted,
    inspectorOpen,
    inspectorRect,
    label,
    probe,
    safeRect,
    worldHeight,
    worldWidth,
  ]);

  const changeZoom = (factor: number) => {
    const viewport = viewportRef.current;
    if (!viewport) {
      return;
    }
    const rect = factoryViewportSafeArea({
      height: viewport.clientHeight,
      inspectorOpen,
      narrow: windowMode.narrow,
      short: windowMode.short,
      width: viewport.clientWidth,
    });
    setCamera((current) => {
      const floor = Math.min(MIN_ZOOM, current.zoom);
      const zoom = Math.min(MAX_ZOOM, Math.max(floor, current.zoom * factor));
      const centerX = rect.left + rect.width / 2;
      const centerY = rect.top + rect.height / 2;
      return {
        x: centerX - ((centerX - current.x) / current.zoom) * zoom,
        y: centerY - ((centerY - current.y) / current.zoom) * zoom,
        zoom,
      };
    });
    setFitted(false);
  };

  const cameraContext = useMemo<FactoryViewportCameraContext>(
    () => ({
      ensureVisible,
      fitted,
      ...(inspectorRect ? { inspectorRect } : {}),
      narrow: windowMode.narrow,
      safeRect,
      viewportRect: factoryViewportRect(size),
      x: camera.x,
      y: camera.y,
      zoom: camera.zoom,
    }),
    [
      camera.x,
      camera.y,
      camera.zoom,
      ensureVisible,
      fitted,
      inspectorRect,
      safeRect,
      size,
      windowMode.narrow,
    ],
  );

  return (
    <div
      aria-label={`${label} viewport`}
      className="factory-viewport"
      data-camera={fitted ? "fit" : "manual"}
      data-camera-x={camera.x}
      data-camera-y={camera.y}
      data-camera-zoom={camera.zoom}
      data-inspector={inspectorOpen ? "open" : "closed"}
      onPointerDown={(event) => {
        if (event.button !== 0 || (event.target as HTMLElement).closest("button")) {
          return;
        }
        dragRef.current = { pointerId: event.pointerId, x: event.clientX, y: event.clientY };
        event.currentTarget.setPointerCapture(event.pointerId);
        event.currentTarget.dataset.dragging = "true";
      }}
      onPointerMove={(event) => {
        const drag = dragRef.current;
        if (!drag || drag.pointerId !== event.pointerId) {
          return;
        }
        const dx = event.clientX - drag.x;
        const dy = event.clientY - drag.y;
        dragRef.current = { pointerId: event.pointerId, x: event.clientX, y: event.clientY };
        setCamera((current) => ({ ...current, x: current.x + dx, y: current.y + dy }));
        setFitted(false);
      }}
      onPointerUp={(event) => {
        if (dragRef.current?.pointerId === event.pointerId) {
          dragRef.current = undefined;
          event.currentTarget.releasePointerCapture(event.pointerId);
          delete event.currentTarget.dataset.dragging;
        }
      }}
      ref={viewportRef}
    >
      <div className="factory-viewport-controls" role="group" aria-label={`${label} camera`}>
        <button aria-label="Zoom out" onClick={() => changeZoom(0.8)} type="button">−</button>
        <output aria-label="Zoom level">{Math.round(camera.zoom * 100)}%</output>
        <button aria-label="Zoom in" onClick={() => changeZoom(1.25)} type="button">+</button>
        <button className="factory-viewport-fit" onClick={fit} type="button">Fit all</button>
      </div>
      <div
        className="factory-viewport-world"
        ref={worldRef}
        style={{
          height: `${worldHeight}px`,
          transform: `translate3d(${camera.x}px, ${camera.y}px, 0) scale(${camera.zoom})`,
          width: `${worldWidth}px`,
        }}
      >
        <CameraContext.Provider value={cameraContext}>{children}</CameraContext.Provider>
      </div>
    </div>
  );
}
