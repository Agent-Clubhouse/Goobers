export function newestFirst<T>(
  items: readonly T[],
  sequence: (item: T) => number | undefined,
): T[] {
  return [...items].sort(
    (left, right) =>
      (sequence(right) ?? Number.NEGATIVE_INFINITY) -
      (sequence(left) ?? Number.NEGATIVE_INFINITY),
  );
}
