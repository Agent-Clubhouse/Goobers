// mascot-core: the canon stage look — one lighting rig and one lens, shared
// by every surface that renders a goober.
//
// WHY THIS EXISTS
// ---------------
// Geometry parity was only ever half of "why doesn't it look the same." The
// three surfaces that render goobers had each grown their own lighting and
// their own field of view, so identical models still read differently:
//
//   surface                      hemi (sky/ground/int)      key    fill   FOV
//   demo/src/main.js             #fffdf5 / #cfc4ae / 0.85   1.6    0.55   38
//   src/mascot/stage.js          #fffdf5 / #2a1566 / 0.50   1.9    0.85   42
//   react/GooberStage.jsx        #fffdf5 / #2a1566 / 0.60   2.0    none   35
//
// GooberStage had no fill light at all, which is why its goobers looked
// harder and flatter on the shadow side than the lander's regardless of what
// the mesh underneath was doing. No amount of mesh work would have closed
// that, and it was never tracked as a delta because the fidelity audits were
// all pointed at geometry.
//
// The coming-soon lander is the reference Mason signed off on ("the landing
// is right"), so its values are canon here. The demo's neutral #cfc4ae
// ground bounce is a studio look for the authoring tool; the lander's
// #2a1566 is the brand ink, which is what makes the character sit on the
// Paper canvas instead of floating in a showroom void (MASCOTS.md §0).
//
// Field of view is part of how the character reads — a goober at 35° and the
// same goober at 42° have visibly different proportions — so it is fixed
// here alongside the lights. Camera *position* is not: framing is a layout
// decision each surface makes for itself.
import * as THREE from 'three';

/** Vertical FOV, degrees. Fixed across surfaces — see this file's header. */
export const CANON_FOV = 42;

export const CANON_LIGHTS = {
  hemisphere: { sky: '#fffdf5', ground: '#2a1566', intensity: 0.5 },
  key: { color: '#fff3e2', intensity: 1.9, position: [2.5, 4.5, 3.5] },
  fill: { color: '#dfe6ff', intensity: 0.85, position: [-3, 2.5, -2] },
};

/**
 * Adds the canon three-light rig to a scene and returns the lights, so a
 * caller that owns scene teardown can dispose or remove them.
 *
 * Every goober-rendering surface should call this rather than hand-rolling
 * lights — that is the whole point of the module. If a surface genuinely
 * needs a different look, change it here and accept it everywhere, or the
 * table in this file's header starts growing rows again.
 */
export function applyCanonLighting(scene) {
  const { hemisphere, key, fill } = CANON_LIGHTS;
  const hemi = new THREE.HemisphereLight(hemisphere.sky, hemisphere.ground, hemisphere.intensity);
  const keyLight = new THREE.DirectionalLight(key.color, key.intensity);
  keyLight.position.set(...key.position);
  const fillLight = new THREE.DirectionalLight(fill.color, fill.intensity);
  fillLight.position.set(...fill.position);
  scene.add(hemi, keyLight, fillLight);
  return { hemi, key: keyLight, fill: fillLight };
}
