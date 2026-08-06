/**
 * Forced-colors detection for the Plant.
 *
 * Windows High Contrast and its equivalents replace the author's colours with a
 * small system palette. A WebGL canvas is completely outside that substitution:
 * it keeps painting the authored scene, and every status the hall encodes in
 * colour silently stops meaning anything. The honest response is not to render
 * it at all, and to hand the operator the DOM view that the OS *can* recolour.
 *
 * Pure and DOM-only: no Three.js, no React, so the decision is testable without
 * mounting a renderer.
 */

export const PLANT_FORCED_COLORS_QUERY = "(forced-colors: active)";

interface MediaQueryHost {
  matchMedia?: (query: string) => MediaQueryList;
}

function query(view: MediaQueryHost | undefined): MediaQueryList | undefined {
  const host =
    view ?? (typeof window === "undefined" ? undefined : (window as MediaQueryHost));
  if (typeof host?.matchMedia !== "function") {
    return undefined;
  }
  try {
    return host.matchMedia(PLANT_FORCED_COLORS_QUERY);
  } catch {
    // Older engines throw on unknown media features rather than returning a
    // non-matching list. An unreadable answer is treated as "not forced".
    return undefined;
  }
}

export function plantForcedColorsActive(view?: MediaQueryHost): boolean {
  return query(view)?.matches ?? false;
}

/** Subscribes to forced-colors changes. Returns a disposer. */
export function observePlantForcedColors(
  onChange: (active: boolean) => void,
  view?: MediaQueryHost,
): () => void {
  const list = query(view);
  if (!list) {
    return () => undefined;
  }
  const listener = (event: MediaQueryListEvent) => onChange(event.matches);
  if (typeof list.addEventListener === "function") {
    list.addEventListener("change", listener);
    return () => list.removeEventListener("change", listener);
  }
  if (typeof list.addListener === "function") {
    list.addListener(listener);
    return () => list.removeListener?.(listener);
  }
  return () => undefined;
}
