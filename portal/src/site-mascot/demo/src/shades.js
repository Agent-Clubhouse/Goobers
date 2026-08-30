// Color derivation only — split out of goober-model.js (MASCOTS.md §4:
// "color derivation must reproduce the L*73 match and the derived shades
// ... from a single base hue"). goober-model.js has module-level side
// effects (a THREE.DataTexture built at import time for the toon material
// gradient), which blocks bundlers from tree-shaking the rest of that file
// — MarchingCubes, canvas texture generation, the whole procedural model —
// out of any bundle that imports so much as one function from it. Production
// code (src/mascot/core/actor.js) only ever needs this one pure formula, so
// it lives here instead, with zero side-effecting imports of its own.
// goober-model.js re-exports it, so there's still exactly one definition.
import * as THREE from 'three';

export function deriveShades(baseHex) {
  const base = new THREE.Color(baseHex);
  const plate = base.clone().offsetHSL(0, -0.09, -0.12);
  const ball = base.clone().offsetHSL(0, 0.0, -0.18);
  const tint = base.clone().offsetHSL(0, -0.1, 0.26);
  return {
    base: `#${base.getHexString()}`,
    plate: `#${plate.getHexString()}`,
    ball: `#${ball.getHexString()}`,
    tint: `#${tint.getHexString()}`,
  };
}
