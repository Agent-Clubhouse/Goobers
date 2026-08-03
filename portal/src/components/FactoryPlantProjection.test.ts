import { describe, expect, it } from "vitest";
import { projectedPointStyle } from "../factoryWebGL";

describe("Factory Plant WebGL projection", () => {
  it("projects semantic hit targets through the isometric camera", () => {
    expect(projectedPointStyle({ x: 725, y: 475 }, 0)).toEqual({
      left: "50%",
      top: "50%",
      "--factory-webgl-left": "50%",
      "--factory-webgl-top": "50%",
    });

    const projected = projectedPointStyle({ x: 1261.5, y: 475 }, 0.72);
    expect(projected.left).toBe("87%");
    expect(projected.top).toBe("50%");
    expect(
      Number.parseFloat(String(projected["--factory-webgl-left"])),
    ).toBeCloseTo(75, 0);
    expect(
      Number.parseFloat(String(projected["--factory-webgl-top"])),
    ).toBeCloseTo(65, 0);
  });
});
