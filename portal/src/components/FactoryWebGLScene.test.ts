import { describe, expect, it } from "vitest";
import { webGLMotionEnabled } from "../factoryWebGL";

describe("Factory WebGL motion", () => {
  it("runs only for visible confirmed activity", () => {
    expect(webGLMotionEnabled("world", false, 1)).toBe(true);
    expect(webGLMotionEnabled("flow", false, 3)).toBe(true);
    expect(webGLMotionEnabled("risk", false, 1)).toBe(false);
    expect(webGLMotionEnabled("world", true, 1)).toBe(false);
    expect(webGLMotionEnabled("world", false, 0)).toBe(false);
  });
});
