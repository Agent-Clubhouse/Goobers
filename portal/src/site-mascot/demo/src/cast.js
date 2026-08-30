// The cast — MASCOTS.md §4. Same body, same face; personality is parameters.
// All colors sit at L* 73 per BRAND.md so the gaggle reads as one species.

export const CAST = [
  {
    name: 'Pip',
    color: '#C9A0FA',
    glyph: '>_',
    temperament: 'steady, attentive',
    params: {
      mass: 1.0, stiffness: 1.0, damping: 1.0, reactivity: 1.0,
      gazeSpeed: 1.0, blinkRate: 8, gait: 'stride', cadence: 2.2, speed: 1.0,
    },
  },
  {
    name: 'Blu',
    color: '#91B0FE',
    glyph: '◔', // ◔ watcher
    temperament: 'unhurried, unbothered',
    params: {
      mass: 1.05, stiffness: 0.7, damping: 1.25, reactivity: 0.6,
      gazeSpeed: 0.6, blinkRate: 4, gait: 'stride', cadence: 1.7, speed: 0.8,
    },
  },
  {
    name: 'Kelp',
    color: '#18C4D4',
    glyph: '◎', // ◎ hunter
    temperament: 'quick, darting',
    params: {
      mass: 0.85, stiffness: 1.4, damping: 0.9, reactivity: 1.2,
      gazeSpeed: 1.5, blinkRate: 10, gait: 'stride', cadence: 3.2, speed: 1.25,
    },
  },
  {
    name: 'Moss',
    color: '#63C682',
    glyph: '✓', // ✓ reviewer
    temperament: 'deliberate, grounded',
    params: {
      mass: 1.3, stiffness: 0.9, damping: 1.2, reactivity: 0.8,
      gazeSpeed: 0.5, blinkRate: 5, gait: 'stride', cadence: 1.6, speed: 0.75,
    },
  },
  {
    name: 'Wick',
    color: '#E2A757',
    glyph: '⌗', // ⌗ curator
    temperament: 'eager, bouncy',
    params: {
      mass: 0.9, stiffness: 1.2, damping: 0.8, reactivity: 1.3,
      gazeSpeed: 1.1, blinkRate: 12, gait: 'hop', cadence: 2.6, speed: 1.0,
    },
  },
  {
    name: 'Coco',
    color: '#FE93A0',
    glyph: '>_',
    temperament: 'dramatic, expressive',
    params: {
      mass: 0.95, stiffness: 1.0, damping: 0.65, reactivity: 1.6,
      gazeSpeed: 1.2, blinkRate: 9, gait: 'stride', cadence: 2.4, speed: 1.0,
    },
  },
];

// Derived views onto CAST — the single source of truth for name/color/glyph
// pairings (previously hand-duplicated in tests/mascot-canon.test.js and
// Goober.astro's prop defaults; MASCOTS.md §4 stays the human-readable spec
// table, cross-checked against CAST by the canon test).
export const NAMES = CAST.map((c) => c.name);
export const DEFAULT_GLYPH = CAST[0].glyph;
export const DEFAULT_COLOR = CAST[0].color;

// glyph -> every canonical color cast in that role (e.g. `>_` has two: Pip's
// violet and Coco's coral).
export const CANON = CAST.reduce((acc, c) => {
  (acc[c.glyph] ??= []).push(c.color);
  return acc;
}, {});
