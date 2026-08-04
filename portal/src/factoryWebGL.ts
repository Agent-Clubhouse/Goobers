import type { FactoryLens } from "./factoryModel";

/**
 * Renderer motion policy.
 *
 * The hand-rolled fixed-camera projection that used to live here was retired in
 * favour of one live-camera contract: the WebGL runtime publishes the camera it
 * actually drew with (see `plantProjection.ts`), and the semantic overlay
 * projects through that. The 2D fallback keeps its own classic bitmap
 * coordinates, which are correct because the bitmap is what is on screen.
 */
export function webGLMotionEnabled(
  lens: FactoryLens,
  reducedMotion: boolean,
  animatedObjectCount: number,
) {
  return !reducedMotion && lens !== "risk" && animatedObjectCount > 0;
}