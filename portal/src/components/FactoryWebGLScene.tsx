import { useEffect, useRef, useState } from "react";

import {
  createFactoryWebGLRuntime,
  type FactoryWebGLRuntime,
  type FactoryWebGLRuntimeOptions,
} from "./plant/factoryWebGLRuntime";
import type { FactoryFloorModel, FactoryLens } from "../factoryModel";
import type { FactoryPlantLayout } from "../factoryPlantLayout";
import {
  observePlantForcedColors,
  plantForcedColorsActive,
} from "../plantForcedColors";
import { getPlantProbeSink } from "../plantProbeSink";
import type { PlantFreshness } from "../plantRisk";
import type {
  PlantProjectionController,
  PlantScreenRect,
} from "../plantProjection";

export type RendererState = "pending" | "ready" | "fallback";

export interface FactoryWebGLSceneProps {
  layout: FactoryPlantLayout;
  model: FactoryFloorModel;
  lens: FactoryLens;
  reducedMotion: boolean;
  animateTransitions: boolean;
  /** The page's own read state, threaded through to the runtime and probe. */
  freshness: PlantFreshness;
  /**
   * The unobscured part of the canvas in canvas pixels.
   *
   * Optional for direct embeddings. The normal FactoryViewport path leaves
   * this unset and moves the entire Plant with its outer navigation camera.
   */
  safeArea?: PlantScreenRect;
  /** Lifts renderer state so the plant can pick its coordinate source. */
  onRendererState?: (state: RendererState) => void;
  /** Publishes the live camera; undefined once the renderer is gone. */
  onController?: (controller: PlantProjectionController | undefined) => void;
  /** Injection point for tests; production always builds the real runtime. */
  createRuntime?: (options: FactoryWebGLRuntimeOptions) => FactoryWebGLRuntime | undefined;
}

function readThemeKey(): string {
  if (typeof document === "undefined") {
    return "light";
  }
  return document.documentElement.dataset.theme ?? "light";
}

/**
 * React adapter over the persistent Plant runtime.
 *
 * This component owns no Three.js state. The mount effect creates exactly one
 * runtime for the canvas and disposes it on unmount; every prop change flows
 * through a separate update effect so the renderer, scene, camera, listeners
 * and RAF scheduler survive model, lens, theme and motion churn.
 */
export function FactoryWebGLScene({
  animateTransitions,
  createRuntime,
  freshness,
  layout,
  lens,
  model,
  onController,
  onRendererState,
  reducedMotion,
  safeArea,
}: FactoryWebGLSceneProps) {
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const runtimeRef = useRef<FactoryWebGLRuntime | undefined>(undefined);
  const [state, setState] = useState<RendererState>("pending");
  const [themeKey, setThemeKey] = useState(readThemeKey);
  const [forcedColors, setForcedColors] = useState(() =>
    plantForcedColorsActive(),
  );

  const pendingUpdate = useRef({
    animateTransitions,
    freshness,
    layout,
    lens,
    model,
    reducedMotion,
    ...(safeArea ? { safeArea } : {}),
    theme: themeKey,
  });
  pendingUpdate.current = {
    animateTransitions,
    freshness,
    layout,
    lens,
    model,
    reducedMotion,
    ...(safeArea ? { safeArea } : {}),
    theme: themeKey,
  };
  const lastAppliedUpdate = useRef<typeof pendingUpdate.current | undefined>(
    undefined,
  );
  // A changing callback identity must not tear down the renderer.
  const onControllerRef = useRef(onController);
  onControllerRef.current = onController;

  useEffect(() => {
    onRendererState?.(state);
  }, [onRendererState, state]);

  useEffect(() => {
    const canvas = canvasRef.current;
    const probe = getPlantProbeSink();
    if (forcedColors) {
      // Forced colours replace author colour everywhere except inside a canvas.
      // Rendering the hall here would leave an operator with a status system
      // their OS has already told us they cannot read.
      probe?.rendererState("fallback");
      setState("fallback");
      onControllerRef.current?.(undefined);
      return;
    }
    if (!canvas) {
      probe?.rendererState("fallback");
      setState("fallback");
      onControllerRef.current?.(undefined);
      return;
    }
    const factory = createRuntime ?? createFactoryWebGLRuntime;
    const runtime = factory({
      canvas,
      onState: setState,
      ...(probe ? { probe } : {}),
    });
    if (!runtime) {
      // No renderer means no runtime to report state, so the adapter reports the
      // one fact the probe needs: this browser is on the image fallback.
      probe?.rendererState("fallback");
      setState("fallback");
      onControllerRef.current?.(undefined);
      return;
    }
    runtimeRef.current = runtime;
    const update = pendingUpdate.current;
    lastAppliedUpdate.current = update;
    runtime.update(update);
    onControllerRef.current?.(runtime);
    return () => {
      lastAppliedUpdate.current = undefined;
      runtimeRef.current = undefined;
      onControllerRef.current?.(undefined);
      runtime.dispose();
    };
    // Mount-only: prop changes are handled by the update effect below.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [createRuntime, forcedColors]);

  useEffect(() => {
    const runtime = runtimeRef.current;
    if (!runtime) {
      return;
    }
    const update = pendingUpdate.current;
    if (lastAppliedUpdate.current === update) {
      // The mount effect already applied this exact snapshot.
      return;
    }
    lastAppliedUpdate.current = update;
    runtime.update(update);
  }, [
    animateTransitions,
    freshness,
    layout,
    lens,
    model,
    reducedMotion,
    safeArea,
    themeKey,
  ]);

  useEffect(() => observePlantForcedColors(setForcedColors), []);

  useEffect(() => {
    if (typeof document === "undefined" || typeof MutationObserver === "undefined") {
      return;
    }
    const root = document.documentElement;
    const observer = new MutationObserver(() => {
      setThemeKey(root.dataset.theme ?? "light");
    });
    observer.observe(root, { attributeFilter: ["data-theme"], attributes: true });
    setThemeKey(root.dataset.theme ?? "light");
    return () => {
      observer.disconnect();
    };
  }, []);

  return (
    <div
      className="factory-plant-renderer"
      data-forced-colors={forcedColors ? "true" : "false"}
      data-webgl={state}
    >
      {/*
       * The authored backdrop is CSS, so the complete fallback is on screen in
       * the same frame a context is lost. The approved illustration is
       * progressive enhancement mounted only once we are actually on the
       * fallback, which keeps 540 KB off the successful WebGL path.
       */}
      <div aria-hidden="true" className="factory-plant-backdrop-authored" />
      {state === "fallback" && !forcedColors ? (
        <img
          alt=""
          className="factory-plant-backdrop"
          draggable="false"
          src="/factory-plant-base.png"
        />
      ) : null}
      {forcedColors ? null : (
        <canvas className="factory-plant-webgl" ref={canvasRef} />
      )}
    </div>
  );
}

export default FactoryWebGLScene;
