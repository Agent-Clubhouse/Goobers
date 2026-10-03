const compactRecordLayoutQuery =
  "(max-width: 820px), (max-width: 900px) and (orientation: landscape)";

export function compactRecordLayoutDefault(): boolean {
  return typeof window !== "undefined"
    ? (window.matchMedia?.(compactRecordLayoutQuery).matches ?? false)
    : false;
}
