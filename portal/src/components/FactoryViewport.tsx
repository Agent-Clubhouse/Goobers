import { useCallback, useEffect, useRef, useState } from "react";

const MIN_ZOOM = 0.2;
const MAX_ZOOM = 2;
const VIEWPORT_PADDING = 20;

interface Camera {
  x: number;
  y: number;
  zoom: number;
}

export function FactoryViewport({
  children,
  label,
  worldHeight,
  worldWidth,
}: {
  children: React.ReactNode;
  label: string;
  worldHeight: number;
  worldWidth: number;
}) {
  const viewportRef = useRef<HTMLDivElement>(null);
  const dragRef = useRef<{ pointerId: number; x: number; y: number } | undefined>(undefined);
  const [camera, setCamera] = useState<Camera>({ x: 0, y: 0, zoom: 1 });
  const [fitted, setFitted] = useState(true);

  const fit = useCallback(() => {
    const viewport = viewportRef.current;
    if (!viewport) {
      return;
    }
    const zoom = Math.min(
      1,
      Math.max(
        MIN_ZOOM,
        Math.min(
          (viewport.clientWidth - VIEWPORT_PADDING * 2) / worldWidth,
          (viewport.clientHeight - VIEWPORT_PADDING * 2) / worldHeight,
        ),
      ),
    );
    setCamera({
      x: Math.round((viewport.clientWidth - worldWidth * zoom) / 2),
      y: Math.round((viewport.clientHeight - worldHeight * zoom) / 2),
      zoom,
    });
    setFitted(true);
  }, [worldHeight, worldWidth]);

  useEffect(() => {
    const viewport = viewportRef.current;
    if (!viewport) {
      return;
    }
    const handleResize = () => {
      if (fitted) {
        fit();
      }
    };
    if (typeof ResizeObserver === "undefined") {
      window.addEventListener("resize", handleResize);
      if (fitted) {
        fit();
      }
      return () => window.removeEventListener("resize", handleResize);
    }
    const observer = new ResizeObserver(handleResize);
    observer.observe(viewport);
    if (fitted) {
      fit();
    }
    return () => observer.disconnect();
  }, [fit, fitted]);

  const changeZoom = (factor: number) => {
    const viewport = viewportRef.current;
    if (!viewport) {
      return;
    }
    setCamera((current) => {
      const zoom = Math.min(MAX_ZOOM, Math.max(MIN_ZOOM, current.zoom * factor));
      const centerX = viewport.clientWidth / 2;
      const centerY = viewport.clientHeight / 2;
      return {
        x: centerX - ((centerX - current.x) / current.zoom) * zoom,
        y: centerY - ((centerY - current.y) / current.zoom) * zoom,
        zoom,
      };
    });
    setFitted(false);
  };

  return (
    <div
      aria-label={`${label} viewport`}
      className="factory-viewport"
      data-camera={fitted ? "fit" : "manual"}
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
        style={{
          height: `${worldHeight}px`,
          transform: `translate3d(${camera.x}px, ${camera.y}px, 0) scale(${camera.zoom})`,
          width: `${worldWidth}px`,
        }}
      >
        {children}
      </div>
    </div>
  );
}
