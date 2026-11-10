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

  it("resolves a theme token from the actual color longhand", () => {
    const styles = document.createElement("style");
    styles.textContent = `
      :root { --computed-color-test: #25242b; }
      :root[data-computed-color-test="dark"] { --computed-color-test: #0d0d11; }
      .computed-color-test { background-color: var(--computed-color-test); }
    `;
    const element = document.createElement("div");
    element.className = "computed-color-test";
    document.head.append(styles);
    document.body.append(element);
    try {
      expect(resolveComputedColor(element, "background-color")).toBe("#25242b");
      document.documentElement.dataset.computedColorTest = "dark";
      expect(resolveComputedColor(element, "background-color")).toBe("#0d0d11");
    } finally {
      delete document.documentElement.dataset.computedColorTest;
      element.remove();
      styles.remove();
    }
  });

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
