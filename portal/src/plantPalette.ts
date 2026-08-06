/**
 * The authored scene palette for the WebGL Plant.
 *
 * The Plant is an operations instrument, not a themed panel. UI tokens describe
 * chrome — panels, ink, lines — and a hall needs different things: concrete,
 * painted decks, machine bodies, structure, fixtures, and *light*. Reading
 * `--panel-raised` for the key light is exactly how a dark theme ends up as a
 * black scene lit by a black lamp.
 *
 * Both palettes are therefore authored here, not derived from CSS. Every
 * surface entry is a surface, every light entry is a light with an authored
 * intensity, and the contrast relationships an operator depends on are asserted
 * in `plantPalette.test.ts` rather than assumed.
 *
 * Nothing in this file touches Three.js or the DOM, so the palette and its
 * contrast arithmetic are unit-testable on their own and stay out of the lazy
 * renderer chunk.
 */

export type PlantTheme = "light" | "dark";

/**
 * The scene vocabulary.
 *
 * Grouped by what the thing *is* in the hall, because that is the only way a
 * later change ("make gates read colder") stays a one-line edit instead of a
 * hunt through material constructors.
 */
export interface PlantScenePalette {
  theme: PlantTheme;

  /* Environment ---------------------------------------------------------- */
  /** Clear colour behind the hall. Never pure black or pure white. */
  background: string;
  /** Concrete outside the workflow bays. */
  floor: string;
  floorGrid: string;
  floorGridStrong: string;
  /** Painted circulation aisle between bays, plus its restrained markings. */
  aisle: string;
  aisleMarking: string;
  /** Hall enclosure. */
  wall: string;
  wallTrim: string;
  window: string;
  roof: string;
  pipe: string;
  utility: string;
  storage: string;
  pallet: string;
  commons: string;
  water: string;

  /* Workflow bays -------------------------------------------------------- */
  pad: string;
  padAlternate: string;
  /** Raised kerb that separates one bay deck from the next. */
  padEdge: string;
  /** Bay identification pylon / sign post. */
  signPost: string;

  /* Structure ------------------------------------------------------------ */
  structure: string;
  structureTrim: string;
  guardrail: string;
  /** Operator console block beside a machine. */
  console: string;

  /* Machines ------------------------------------------------------------- */
  machineBody: string;
  /** Second body value so adjacent silhouettes never merge into one mass. */
  machineBodyAlt: string;
  machineCap: string;
  /** Brand accent, used only as machine trim. */
  machineTrim: string;
  /**
   * Per-archetype machine identity accents.
   *
   * A bounded cool-band ramp (teal, blue, violet, magenta, chartreuse), each
   * held at least 35 degrees from every alarm hue and matched in luminance so
   * grayscale value keeps meaning *status*, not *type*. Applied only to a
   * machine's trim, never its body, so the machine/deck contrast gate and the
   * shape-first reading are untouched and the alarm channel cannot collapse.
   */
  machineAccentGate: string;
  machineAccentDeterministic: string;
  machineAccentAgentic: string;
  machineAccentEvaluator: string;
  machineAccentParallel: string;

  /* Fixtures and light --------------------------------------------------- */
  lightHousing: string;
  /** The emissive lens of an overhead fixture: a light source, not a surface. */
  lightEmissive: string;
  /** Actual light colours. These stay light in every theme. */
  keyLight: string;
  fillSky: string;
  fillGround: string;
  rimLight: string;
  keyIntensity: number;
  fillIntensity: number;
  rimIntensity: number;
  /** Baseline exposure so a theme cannot silently render to near-black. */
  exposure: number;

  /* Status vocabulary ---------------------------------------------------- */
  statusRunning: string;
  statusIdle: string;
  statusHeld: string;
  statusBlocked: string;
  statusImpeded: string;
  /** Neutral, deliberately not hold amber: unknown is not a confirmed hazard. */
  statusUnknown: string;
  /** Completeness modifier, also neutral. */
  statusStale: string;

  /* Risk markers --------------------------------------------------------- */
  /** Beacon body for a confirmed hazard; must out-contrast the machine body. */
  riskBeaconBlocked: string;
  riskBeaconHeld: string;
  riskBeaconImpeded: string;
  /** Marker for an unread or incomplete signal: neutral, hatched, wireframe. */
  riskMarkerUnknown: string;
  /** Floor ring under an entity carrying a confirmed hazard. */
  riskRing: string;

  /* People and material -------------------------------------------------- */
  worker: string;
  workerIdle: string;
  workerVisor: string;
  crate: string;
  crateBlocked: string;
  crateHeld: string;
  crateUnknown: string;

  /* Selection ------------------------------------------------------------ */
  selectionRing: string;
  focusRing: string;

  /* Text and trim -------------------------------------------------------- */
  /** In-scene and overlay key text, measured against `background`. */
  text: string;
  textMuted: string;
  /** Restrained safety marking: kerb stripes and hatching. */
  safety: string;
  accent: string;
}

/**
 * Daylight hall.
 *
 * Neutral concrete and painted steel first; the brand accent appears only on
 * machine trim. Machine bodies are deliberately far darker than the deck they
 * stand on so a silhouette survives a grayscale print.
 */
export const PLANT_LIGHT_SCENE_PALETTE: PlantScenePalette = {
  accent: "#a93156",
  aisle: "#dad7cd",
  aisleMarking: "#a8841c",
  background: "#e7e9ec",
  console: "#3f454e",
  crate: "#7a5628",
  crateBlocked: "#8f2029",
  crateHeld: "#7a4d04",
  crateUnknown: "#4c525b",
  exposure: 1,
  fillGround: "#dedbd4",
  fillIntensity: 2.05,
  fillSky: "#eef3fb",
  floor: "#e2e4e8",
  floorGrid: "#c9ccd1",
  floorGridStrong: "#a6abb3",
  focusRing: "#2f2296",
  guardrail: "#7f858e",
  keyIntensity: 1.7,
  keyLight: "#fff6e8",
  lightEmissive: "#fffdf4",
  lightHousing: "#b9bdc4",
  machineBody: "#3f454e",
  machineBodyAlt: "#303640",
  machineCap: "#515965",
  machineTrim: "#2f6f9f",
  machineAccentGate: "#3e6f81",
  machineAccentDeterministic: "#4b569d",
  machineAccentAgentic: "#774b9d",
  machineAccentEvaluator: "#9b4b8b",
  machineAccentParallel: "#566f32",
  pad: "#d6d9de",
  padAlternate: "#cdd1d7",
  padEdge: "#71777f",
  riskBeaconBlocked: "#ff9ea4",
  riskBeaconHeld: "#ffcf7a",
  riskBeaconImpeded: "#ffb183",
  riskMarkerUnknown: "#f0f2f5",
  riskRing: "#2a2e35",
  rimIntensity: 0.5,
  rimLight: "#e6ecf6",
  safety: "#a8841c",
  selectionRing: "#1f2937",
  signPost: "#6b717b",
  statusBlocked: "#9c1f2a",
  statusHeld: "#7f5203",
  statusIdle: "#666c75",
  statusImpeded: "#8b4a15",
  statusRunning: "#146341",
  statusStale: "#555b64",
  statusUnknown: "#474d56",
  structure: "#9ba0a8",
  structureTrim: "#7d838c",
  text: "#1d2026",
  textMuted: "#454a53",
  theme: "light",
  wall: "#dcdee2",
  wallTrim: "#b3b7be",
  window: "#78acd0",
  roof: "#496f8d",
  pipe: "#52616f",
  utility: "#6d7f92",
  storage: "#657688",
  pallet: "#78899a",
  commons: "#4b8296",
  water: "#67add2",
  worker: "#1f4a8c",
  workerIdle: "#6e747d",
  workerVisor: "#22262c",
};

/**
 * Night hall.
 *
 * Authored to sit in a usable mid-dark band rather than at the bottom of the
 * range: an operations view that renders as 98% black is not a dark theme, it
 * is an outage. Lights stay real lights, so the same albedo relationships
 * survive the theme change.
 */
export const PLANT_DARK_SCENE_PALETTE: PlantScenePalette = {
  accent: "#e17b9b",
  aisle: "#545c67",
  aisleMarking: "#c9a63f",
  background: "#333a44",
  console: "#d4dae2",
  crate: "#c79a5c",
  crateBlocked: "#7d1d26",
  crateHeld: "#6d4c09",
  crateUnknown: "#cad1da",
  exposure: 1,
  fillGround: "#6b7482",
  fillIntensity: 1.95,
  fillSky: "#93a8c8",
  floor: "#474f5a",
  floorGrid: "#5a626d",
  floorGridStrong: "#858e9b",
  focusRing: "#d6c8ff",
  guardrail: "#98a1ad",
  keyIntensity: 1.65,
  keyLight: "#fff3e2",
  lightEmissive: "#fff8e6",
  lightHousing: "#6c7580",
  machineBody: "#f0f3f7",
  machineBodyAlt: "#e7ebf0",
  machineCap: "#ffffff",
  machineTrim: "#78b7e3",
  machineAccentGate: "#5ab6dd",
  machineAccentDeterministic: "#9eaafa",
  machineAccentAgentic: "#ce9cfa",
  machineAccentEvaluator: "#fa91e5",
  machineAccentParallel: "#97bc5f",
  pad: "#525a65",
  padAlternate: "#4c545e",
  padEdge: "#aab4c1",
  riskBeaconBlocked: "#7a1c24",
  riskBeaconHeld: "#674707",
  riskBeaconImpeded: "#70370e",
  riskMarkerUnknown: "#2b3038",
  riskRing: "#eef2f7",
  rimIntensity: 0.5,
  rimLight: "#9fb4d4",
  safety: "#c9a63f",
  selectionRing: "#f2f5f9",
  signPost: "#8b95a1",
  statusBlocked: "#ff9ca2",
  statusHeld: "#f2c065",
  statusIdle: "#a6aeba",
  statusImpeded: "#f0a05e",
  statusRunning: "#63d3a3",
  statusStale: "#b3bbc7",
  statusUnknown: "#c2cad6",
  structure: "#79828e",
  structureTrim: "#929ba7",
  text: "#f2f4f8",
  textMuted: "#c3c9d2",
  theme: "dark",
  wall: "#3b424c",
  wallTrim: "#5f6773",
  window: "#47799e",
  roof: "#6f9fc2",
  pipe: "#a8b2bf",
  utility: "#9aabba",
  storage: "#8295a9",
  pallet: "#889aaa",
  commons: "#69a6b8",
  water: "#4b91bb",
  worker: "#3f6fbf",
  workerIdle: "#6a7280",
  workerVisor: "#e2e7ee",
};

export type PlantPaletteKey = keyof PlantScenePalette;

/** Resolves the authored palette for a theme name; unknown names read light. */
export function plantScenePalette(theme: string | undefined): PlantScenePalette {
  return theme === "dark" ? PLANT_DARK_SCENE_PALETTE : PLANT_LIGHT_SCENE_PALETTE;
}

/**
 * The identity accent for a machine, chosen by its archetype.
 *
 * `meshArchetype` is the scene batch key, e.g. `machine:agentic`. An
 * unrecognised or observed-unknown machine falls back to the neutral brand
 * trim rather than borrowing another archetype's colour, so an unread stage is
 * never painted as a confident type.
 */
export function plantMachineAccent(
  meshArchetype: string,
  palette: PlantScenePalette,
): string {
  switch (meshArchetype) {
    case "machine:gate":
      return palette.machineAccentGate;
    case "machine:deterministic":
      return palette.machineAccentDeterministic;
    case "machine:agentic":
      return palette.machineAccentAgentic;
    case "machine:evaluator":
      return palette.machineAccentEvaluator;
    case "machine:parallel":
      return palette.machineAccentParallel;
    default:
      return palette.machineTrim;
  }
}

/** Reads the document theme without reading any UI colour token. */
export function readPlantTheme(root?: { dataset?: DOMStringMap }): PlantTheme {
  const element =
    root ?? (typeof document === "undefined" ? undefined : document.documentElement);
  return element?.dataset?.theme === "dark" ? "dark" : "light";
}

/* --------------------------------------------------------------------------
 * Colour arithmetic
 *
 * WCAG relative luminance and contrast, plus the two mixes the scene needs.
 * Kept here so the palette, its tests, the runtime and the harness all agree
 * on one definition rather than three approximations.
 * ----------------------------------------------------------------------- */

export interface PlantRgb {
  r: number;
  g: number;
  b: number;
}

export function parseHexColor(value: string): PlantRgb {
  const hex = value.trim().replace(/^#/, "");
  const expanded =
    hex.length === 3
      ? hex
          .split("")
          .map((character) => character + character)
          .join("")
      : hex;
  if (!/^[0-9a-fA-F]{6}$/.test(expanded)) {
    throw new Error(`Not a hex colour: ${value}`);
  }
  return {
    b: Number.parseInt(expanded.slice(4, 6), 16),
    g: Number.parseInt(expanded.slice(2, 4), 16),
    r: Number.parseInt(expanded.slice(0, 2), 16),
  };
}

export function formatHexColor({ b, g, r }: PlantRgb): string {
  const channel = (value: number) =>
    Math.max(0, Math.min(255, Math.round(value)))
      .toString(16)
      .padStart(2, "0");
  return `#${channel(r)}${channel(g)}${channel(b)}`;
}

/** WCAG 2.x relative luminance in [0, 1]. */
export function relativeLuminance(color: string): number {
  const { b, g, r } = parseHexColor(color);
  const linear = (channel: number) => {
    const normalized = channel / 255;
    return normalized <= 0.04045
      ? normalized / 12.92
      : ((normalized + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * linear(r) + 0.7152 * linear(g) + 0.0722 * linear(b);
}

/** WCAG contrast ratio in [1, 21]. */
export function contrastRatio(a: string, b: string): number {
  const first = relativeLuminance(a);
  const second = relativeLuminance(b);
  const lighter = Math.max(first, second);
  const darker = Math.min(first, second);
  return (lighter + 0.05) / (darker + 0.05);
}

/** Perceived brightness of a gamma-encoded colour, matching harness sampling. */
export function encodedLuminance(color: string): number {
  const { b, g, r } = parseHexColor(color);
  return (r * 0.2126 + g * 0.7152 + b * 0.0722) / 255;
}

export function mixHexColor(from: string, to: string, amount: number): string {
  const start = parseHexColor(from);
  const end = parseHexColor(to);
  const ratio = Math.max(0, Math.min(1, amount));
  return formatHexColor({
    b: start.b + (end.b - start.b) * ratio,
    g: start.g + (end.g - start.g) * ratio,
    r: start.r + (end.r - start.r) * ratio,
  });
}

/**
 * Pulls saturation out of a colour while holding its luminance.
 *
 * Risk context is desaturated, never erased: the healthy plant has to stay
 * readable so the operator can see *where* the confirmed hazard is.
 */
export function desaturateHexColor(color: string, amount: number): string {
  const { b, g, r } = parseHexColor(color);
  const gray = r * 0.2126 + g * 0.7152 + b * 0.0722;
  const ratio = Math.max(0, Math.min(1, amount));
  return formatHexColor({
    b: b + (gray - b) * ratio,
    g: g + (gray - g) * ratio,
    r: r + (gray - r) * ratio,
  });
}

/* --------------------------------------------------------------------------
 * Contrast contract
 * ----------------------------------------------------------------------- */

/** The gates the Plant promises in both themes. */
export const PLANT_CONTRAST_GATES = {
  /** A machine silhouette against the deck it stands on. */
  machineVsDeck: 3,
  /** A risk marker against the machine body it marks. */
  riskMarkerVsBody: 3,
  /** Key text against the scene background. */
  textVsBackground: 4.5,
  /** A selection or focus ring against the deck it is drawn on. */
  ringVsDeck: 3,
  /** A status colour against the deck it is painted on. */
  statusVsDeck: 3,
} as const;

export interface PlantContrastReport {
  theme: PlantTheme;
  machineVsFloor: number;
  machineVsPad: number;
  machineAltVsPad: number;
  riskBlockedVsBody: number;
  riskHeldVsBody: number;
  riskImpededVsBody: number;
  riskUnknownVsBody: number;
  textVsBackground: number;
  textVsFloor: number;
  selectionRingVsPad: number;
  focusRingVsPad: number;
  statusBlockedVsPad: number;
  statusHeldVsPad: number;
  statusImpededVsPad: number;
  statusUnknownVsPad: number;
  statusRunningVsPad: number;
  padEdgeVsPad: number;
  /** Encoded luminance of the two largest surfaces, for the browser bands. */
  backgroundLuminance: number;
  floorLuminance: number;
  padLuminance: number;
}

/** Measures every relationship the Plant promises, for tests and the probe. */
export function measurePlantContrast(
  palette: PlantScenePalette,
): PlantContrastReport {
  return {
    backgroundLuminance: encodedLuminance(palette.background),
    floorLuminance: encodedLuminance(palette.floor),
    focusRingVsPad: contrastRatio(palette.focusRing, palette.pad),
    machineAltVsPad: contrastRatio(palette.machineBodyAlt, palette.padAlternate),
    machineVsFloor: contrastRatio(palette.machineBody, palette.floor),
    machineVsPad: contrastRatio(palette.machineBody, palette.pad),
    padEdgeVsPad: contrastRatio(palette.padEdge, palette.pad),
    padLuminance: encodedLuminance(palette.pad),
    riskBlockedVsBody: contrastRatio(palette.riskBeaconBlocked, palette.machineBody),
    riskHeldVsBody: contrastRatio(palette.riskBeaconHeld, palette.machineBody),
    riskImpededVsBody: contrastRatio(palette.riskBeaconImpeded, palette.machineBody),
    riskUnknownVsBody: contrastRatio(palette.riskMarkerUnknown, palette.machineBody),
    selectionRingVsPad: contrastRatio(palette.selectionRing, palette.pad),
    statusBlockedVsPad: contrastRatio(palette.statusBlocked, palette.pad),
    statusHeldVsPad: contrastRatio(palette.statusHeld, palette.pad),
    statusImpededVsPad: contrastRatio(palette.statusImpeded, palette.pad),
    statusRunningVsPad: contrastRatio(palette.statusRunning, palette.pad),
    statusUnknownVsPad: contrastRatio(palette.statusUnknown, palette.pad),
    textVsBackground: contrastRatio(palette.text, palette.background),
    textVsFloor: contrastRatio(palette.text, palette.floor),
    theme: palette.theme,
  };
}

/**
 * The mean encoded luminance band a rendered scene must land in.
 *
 * A dark plant that measures 0.09 with 99% of its pixels below 0.2 is not a
 * night shift, it is an unlit room; a light plant that measures 0.98 has been
 * erased. Both ends are asserted in the browser harness.
 *
 * `maxDarkPixelRatio` counts pixels below 0.2 encoded luminance and
 * `maxNearBlackRatio` counts pixels below 0.06 — the second is the erasure
 * test, because a legitimately dim surface still carries an image and a black
 * one does not.
 */
export const PLANT_LUMINANCE_BANDS = {
  dark: {
    maxDarkPixelRatio: 0.45,
    maxMean: 0.55,
    maxNearBlackRatio: 0.06,
    minMean: 0.16,
  },
  light: {
    maxDarkPixelRatio: 0.2,
    maxMean: 0.93,
    maxNearBlackRatio: 0.06,
    minMean: 0.45,
  },
} as const;
