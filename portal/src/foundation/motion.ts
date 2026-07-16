import { useEffect, useState } from "react";

export const reducedMotionQuery = "(prefers-reduced-motion: reduce)";

export function usePrefersReducedMotion(): boolean {
  const [reducedMotion, setReducedMotion] = useState(
    () => typeof window.matchMedia === "function" && window.matchMedia(reducedMotionQuery).matches,
  );

  useEffect(() => {
    if (typeof window.matchMedia !== "function") {
      return;
    }
    const mediaQuery = window.matchMedia(reducedMotionQuery);
    const onChange = (event: MediaQueryListEvent) => setReducedMotion(event.matches);
    mediaQuery.addEventListener("change", onChange);
    return () => mediaQuery.removeEventListener("change", onChange);
  }, []);

  return reducedMotion;
}
