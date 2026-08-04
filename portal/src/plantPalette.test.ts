import { describe, expect, it } from "vitest";

import {
  contrastRatio,
  desaturateHexColor,
  encodedLuminance,
  formatHexColor,
  mixHexColor,
  parseHexColor,
  PLANT_CONTRAST_GATES,
  PLANT_DARK_SCENE_PALETTE,
  PLANT_LIGHT_SCENE_PALETTE,
  PLANT_LUMINANCE_BANDS,
  measurePlantContrast,
  plantScenePalette,
  readPlantTheme,
  relativeLuminance,
  type PlantScenePalette,
} from "./plantPalette";

const PALETTES: readonly PlantScenePalette[] = [
  PLANT_LIGHT_SCENE_PALETTE,
  PLANT_DARK_SCENE_PALETTE,
];

describe("plant scene palette", () => {
  it("resolves a theme without reading a single UI token", () => {
    expect(plantScenePalette("dark")).toBe(PLANT_DARK_SCENE_PALETTE);
    expect(plantScenePalette("light")).toBe(PLANT_LIGHT_SCENE_PALETTE);
    // An unset or unrecognised theme is daylight, never an unlit room.
    expect(plantScenePalette(undefined)).toBe(PLANT_LIGHT_SCENE_PALETTE);
    expect(plantScenePalette("sepia")).toBe(PLANT_LIGHT_SCENE_PALETTE);
  });

  it("reads the document theme from the data attribute alone", () => {
    expect(readPlantTheme({ dataset: { theme: "dark" } as DOMStringMap })).toBe(
      "dark",
    );
    expect(readPlantTheme({ dataset: {} as DOMStringMap })).toBe("light");
    expect(readPlantTheme({})).toBe("light");
  });

  it("keys every surface with a parseable colour", () => {
    for (const palette of PALETTES) {
      for (const [key, value] of Object.entries(palette)) {
        if (typeof value !== "string" || key === "theme") {
          continue;
        }
        expect(() => parseHexColor(value), `${palette.theme}.${key}`).not.toThrow();
      }
    }
  });

  /**
   * The bug this palette replaced: the key light read a panel token, so the
   * dark theme lit its hall with a near-black lamp. Lights are lights.
   */
  it("keeps key, fill and rim as actual light colours in both themes", () => {
    for (const palette of PALETTES) {
      expect(
        relativeLuminance(palette.keyLight),
        `${palette.theme} key light`,
      ).toBeGreaterThan(0.6);
      expect(
        relativeLuminance(palette.fillSky),
        `${palette.theme} fill sky`,
      ).toBeGreaterThan(0.25);
      expect(
        relativeLuminance(palette.rimLight),
        `${palette.theme} rim light`,
      ).toBeGreaterThan(0.25);
      expect(palette.keyIntensity).toBeGreaterThan(1);
      expect(palette.fillIntensity).toBeGreaterThan(0.5);
      expect(palette.rimIntensity).toBeGreaterThan(0);
    }
  });

  it("meets the machine, marker, ring and text contrast gates in both themes", () => {
    for (const palette of PALETTES) {
      const report = measurePlantContrast(palette);
      const label = palette.theme;

      expect(report.machineVsFloor, `${label} machine vs floor`).toBeGreaterThanOrEqual(
        PLANT_CONTRAST_GATES.machineVsDeck,
      );
      expect(report.machineVsPad, `${label} machine vs pad`).toBeGreaterThanOrEqual(
        PLANT_CONTRAST_GATES.machineVsDeck,
      );
      expect(
        report.machineAltVsPad,
        `${label} alternate machine vs pad`,
      ).toBeGreaterThanOrEqual(PLANT_CONTRAST_GATES.machineVsDeck);

      for (const marker of [
        report.riskBlockedVsBody,
        report.riskHeldVsBody,
        report.riskImpededVsBody,
        report.riskUnknownVsBody,
      ]) {
        expect(marker, `${label} risk marker vs body`).toBeGreaterThanOrEqual(
          PLANT_CONTRAST_GATES.riskMarkerVsBody,
        );
      }

      expect(
        report.selectionRingVsPad,
        `${label} selection ring`,
      ).toBeGreaterThanOrEqual(PLANT_CONTRAST_GATES.ringVsDeck);
      expect(report.focusRingVsPad, `${label} focus ring`).toBeGreaterThanOrEqual(
        PLANT_CONTRAST_GATES.ringVsDeck,
      );

      for (const status of [
        report.statusBlockedVsPad,
        report.statusHeldVsPad,
        report.statusImpededVsPad,
        report.statusUnknownVsPad,
        report.statusRunningVsPad,
      ]) {
        expect(status, `${label} status vs pad`).toBeGreaterThanOrEqual(
          PLANT_CONTRAST_GATES.statusVsDeck,
        );
      }

      expect(report.padEdgeVsPad, `${label} kerb vs deck`).toBeGreaterThanOrEqual(3);
      expect(
        report.textVsBackground,
        `${label} text vs background`,
      ).toBeGreaterThanOrEqual(PLANT_CONTRAST_GATES.textVsBackground);
      expect(report.textVsFloor, `${label} text vs floor`).toBeGreaterThanOrEqual(
        PLANT_CONTRAST_GATES.textVsBackground,
      );
    }
  });

  /**
   * A dark theme that renders 98% black is an outage, not a night shift. The
   * two largest surfaces are asserted here; the rendered mean is asserted in
   * the browser harness against the same bands.
   */
  it("authors dark surfaces into a usable band rather than at the floor", () => {
    const dark = measurePlantContrast(PLANT_DARK_SCENE_PALETTE);
    expect(dark.backgroundLuminance).toBeGreaterThan(0.06);
    expect(dark.floorLuminance).toBeGreaterThan(PLANT_LUMINANCE_BANDS.dark.minMean);
    expect(dark.padLuminance).toBeGreaterThan(PLANT_LUMINANCE_BANDS.dark.minMean);
    expect(dark.padLuminance).toBeLessThan(PLANT_LUMINANCE_BANDS.dark.maxMean);

    const light = measurePlantContrast(PLANT_LIGHT_SCENE_PALETTE);
    expect(light.backgroundLuminance).toBeLessThan(PLANT_LUMINANCE_BANDS.light.maxMean);
    expect(light.padLuminance).toBeGreaterThan(PLANT_LUMINANCE_BANDS.light.minMean);
  });

  it("keeps the accent as trim only, and never as a deck or a body", () => {
    for (const palette of PALETTES) {
      expect(palette.machineTrim).toBe(palette.accent);
      for (const surface of [
        palette.pad,
        palette.padAlternate,
        palette.floor,
        palette.machineBody,
        palette.machineBodyAlt,
        palette.background,
      ]) {
        expect(surface, `${palette.theme} large surface`).not.toBe(palette.accent);
      }
    }
  });

  it("keeps the unknown marker neutral instead of borrowing hold amber", () => {
    for (const palette of PALETTES) {
      expect(palette.riskMarkerUnknown).not.toBe(palette.statusHeld);
      expect(palette.riskMarkerUnknown).not.toBe(palette.riskBeaconHeld);
      const { b, g, r } = parseHexColor(palette.riskMarkerUnknown);
      // Neutral: no channel dominates, so it cannot read as a warning colour.
      expect(Math.max(r, g, b) - Math.min(r, g, b)).toBeLessThanOrEqual(24);
    }
  });
});

describe("plant colour arithmetic", () => {
  it("parses, expands and formats hex colours", () => {
    expect(parseHexColor("#fff")).toEqual({ b: 255, g: 255, r: 255 });
    expect(parseHexColor("102030")).toEqual({ b: 48, g: 32, r: 16 });
    expect(formatHexColor({ b: 48.4, g: 32, r: 16 })).toBe("#102030");
    expect(formatHexColor({ b: -20, g: 900, r: 16 })).toBe("#10ff00");
    expect(() => parseHexColor("#nothex")).toThrow();
  });

  it("computes WCAG luminance and contrast", () => {
    expect(relativeLuminance("#ffffff")).toBeCloseTo(1, 5);
    expect(relativeLuminance("#000000")).toBeCloseTo(0, 5);
    expect(contrastRatio("#ffffff", "#000000")).toBeCloseTo(21, 5);
    expect(contrastRatio("#767676", "#ffffff")).toBeGreaterThan(4.5);
    expect(contrastRatio("#123456", "#123456")).toBeCloseTo(1, 5);
  });

  it("reports encoded luminance the way the harness samples pixels", () => {
    expect(encodedLuminance("#ffffff")).toBeCloseTo(1, 5);
    expect(encodedLuminance("#000000")).toBeCloseTo(0, 5);
    expect(encodedLuminance("#808080")).toBeCloseTo(128 / 255, 5);
  });

  it("mixes and desaturates without moving perceived brightness far", () => {
    expect(mixHexColor("#000000", "#ffffff", 0.5)).toBe("#808080");
    expect(mixHexColor("#000000", "#ffffff", -1)).toBe("#000000");
    expect(mixHexColor("#000000", "#ffffff", 2)).toBe("#ffffff");

    const source = "#9c1f2a";
    const muted = desaturateHexColor(source, 0.72);
    expect(muted).not.toBe(source);
    expect(encodedLuminance(muted)).toBeCloseTo(encodedLuminance(source), 2);
    expect(desaturateHexColor(source, 1)).toMatch(/^#([0-9a-f]{2})\1\1$/);
  });
});
