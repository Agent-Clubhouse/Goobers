import { describe, expect, it } from "vitest";
import { resolveComputedColor } from "./computedColor";

describe("resolveComputedColor", () => {
  it.each(["#25242b", "rgb(37, 36, 43)", "rgba(37, 36, 43, 1)"])(
    "normalizes the opaque color %s",
    (color) => {
      const element = document.createElement("div");
      element.style.backgroundColor = color;
      document.body.append(element);
      try {
        expect(resolveComputedColor(element, "background-color")).toBe("#25242b");
      } finally {
        element.remove();
      }
    },
  );

  it.each(["transparent", "rgba(37, 36, 43, 0.5)", ""])(
    "rejects a non-opaque background %s",
    (color) => {
      const element = document.createElement("div");
      element.style.backgroundColor = color;
      document.body.append(element);
      try {
        expect(() => resolveComputedColor(element, "background-color")).toThrow("opaque color");
      } finally {
        element.remove();
      }
    },
  );
});
