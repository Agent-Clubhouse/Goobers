import { useCallback, useEffect, useRef, useState, type SetStateAction } from "react";

const compactRecordLayoutQuery =
  "(max-width: 820px), (max-width: 900px) and (orientation: landscape)";

function compactRecordLayoutDefault(): boolean {
  return typeof window !== "undefined"
    ? (window.matchMedia?.(compactRecordLayoutQuery).matches ?? false)
    : false;
}

export function useCompactRecordDisclosure() {
  const [expanded, setExpanded] = useState(compactRecordLayoutDefault);
  const explicitlyChanged = useRef(false);

  useEffect(() => {
    if (typeof window === "undefined" || !window.matchMedia) {
      return;
    }

    const mediaQuery = window.matchMedia(compactRecordLayoutQuery);
    const expandForCompactLayout = (event: MediaQueryListEvent) => {
      if (event.matches && !explicitlyChanged.current) {
        setExpanded(true);
      }
    };

    mediaQuery.addEventListener("change", expandForCompactLayout);
    return () => mediaQuery.removeEventListener("change", expandForCompactLayout);
  }, []);

  const setExpandedExplicitly = useCallback((next: SetStateAction<boolean>) => {
    explicitlyChanged.current = true;
    setExpanded(next);
  }, []);

  return [expanded, setExpandedExplicitly] as const;
}
