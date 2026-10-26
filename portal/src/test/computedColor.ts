// Contrast assertions need a color longhand, not the serialized background
// shorthand (which also includes image, position, repeat and attachment).
export function resolveComputedColor(
  element: Element,
  property: "background-color" | "color",
): string {
  const style = window.getComputedStyle(element);
  const computed = style.getPropertyValue(property).trim();
  const customProperty = computed.match(/^var\((--[\w-]+)\)$/)?.[1];
  // jsdom can leave custom properties unresolved. Read the token from the
  // element first, then the root where the portal defines its theme tokens.
  const resolved = customProperty
    ? style.getPropertyValue(customProperty).trim() ||
      window.getComputedStyle(document.documentElement).getPropertyValue(customProperty).trim()
    : computed;

  if (/^#[\da-f]{6}$/i.test(resolved)) {
    return resolved.toLowerCase();
  }
  const rgb = resolved.match(
    /^rgba?\(\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)(?:\s*,\s*([\d.]+))?\s*\)$/i,
  );
  if (rgb) {
    const channels = rgb.slice(1, 4).map(Number);
    const alpha = rgb[4] === undefined ? 1 : Number(rgb[4]);
    if (alpha === 1 && channels.every((channel) => channel <= 255)) {
      return `#${channels.map((channel) => channel.toString(16).padStart(2, "0")).join("")}`;
    }
  }
  // Never substitute an expected token for a transparent or invalid result:
  // that would hide a missing background and invalidate the contrast check.
  throw new Error(`Expected ${property} to resolve to an opaque color, received "${resolved}".`);
}
