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

function hue(color: string): number {
  const { b, g, r } = parseHexColor(color);
  const red = r / 255;
  const green = g / 255;
  const blue = b / 255;
  const max = Math.max(red, green, blue);
  const min = Math.min(red, green, blue);
  const range = max - min;
  if (range === 0) {
    return 0;
  }
  const sector =
    max === red
      ? ((green - blue) / range) % 6
      : max === green
        ? (blue - red) / range + 2
        : (red - green) / range + 4;
  return (sector * 60 + 360) % 360;
}

function hueDistance(first: string, second: string): number {
  const distance = Math.abs(hue(first) - hue(second));
  return Math.min(distance, 360 - distance);
}

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

  it("keeps the accent off decks and machine bodies", () => {
    for (const palette of PALETTES) {
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

  it("separates always-on machine and storage colour from alarm hues", () => {
    for (const palette of PALETTES) {
      const alarms = [
        palette.statusBlocked,
        palette.statusHeld,
        palette.statusRunning,
        palette.riskBeaconBlocked,
        palette.riskBeaconHeld,
        palette.riskBeaconImpeded,
      ];
      const environmentalContext = [
        palette.machineTrim,
        palette.machineAccentGate,
        palette.machineAccentDeterministic,
        palette.machineAccentAgentic,
        palette.machineAccentEvaluator,
        palette.machineAccentParallel,
        palette.storage,
        palette.utility,
        palette.pallet,
        palette.commons,
        palette.water,
        palette.roof,
        palette.window,
        palette.pipe,
      ];
      for (const context of environmentalContext) {
        for (const alarm of alarms) {
          expect(
            hueDistance(context, alarm),
            `${palette.theme} context ${context} vs alarm ${alarm}`,
          ).toBeGreaterThanOrEqual(35);
        }
      }
    }
  });

  it("gives each machine archetype a distinct, deck-legible accent", () => {
    for (const palette of PALETTES) {
      const accents = {
        gate: palette.machineAccentGate,
        deterministic: palette.machineAccentDeterministic,
        agentic: palette.machineAccentAgentic,
        evaluator: palette.machineAccentEvaluator,
        parallel: palette.machineAccentParallel,
      };
      const entries = Object.entries(accents);
      // Each accent must read against the deck it is drawn on.
      for (const [name, accent] of entries) {
        expect(
          contrastRatio(accent, palette.pad),
          `${palette.theme} ${name} accent vs pad`,
        ).toBeGreaterThanOrEqual(3);
        expect(
          contrastRatio(accent, palette.floor),
          `${palette.theme} ${name} accent vs floor`,
        ).toBeGreaterThanOrEqual(3);
      }
      // Adjacent silhouettes must not share a hue, or the colour channel adds
      // no identity.
      for (let i = 0; i < entries.length; i += 1) {
        for (let j = i + 1; j < entries.length; j += 1) {
          expect(
            hueDistance(entries[i][1], entries[j][1]),
            `${palette.theme} ${entries[i][0]} vs ${entries[j][0]}`,
          ).toBeGreaterThanOrEqual(30);
        }
      }
      // Grayscale value stays reserved for status: the accents are matched in
      // encoded luminance so a monochrome print still reads type by shape, not
      // by brightness.
      const luminances = entries.map(([, accent]) => encodedLuminance(accent));
      const spread = Math.max(...luminances) - Math.min(...luminances);
      expect(spread, `${palette.theme} accent luminance spread`).toBeLessThanOrEqual(
        0.14,
      );
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
