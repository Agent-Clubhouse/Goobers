// Shared budget/validation constants for the canon glTF export (MS1/#135),
// used by both demo/export-gltf.mjs (the CLI) and tests/canon-export.test.js
// so the two can't silently drift apart on what "passing" means.

// MASCOTS.md §2's bone-naming convention, exactly.
export const EXPECTED_JOINTS = [
  'root', 'hips', 'spine', 'head_aim', 'antenna_base', 'antenna_tip',
  'arm_L', 'arm_R', 'foot_L', 'foot_R',
];

// MS12/#161: eye expression is no longer a set of named morph-target shapes
// (open/squint/wide/flat/blink, gaze_x/gaze_y) — that convention approximated
// the live demo's actual mechanism (a shared arc mesh's Y-scale, a separate
// "wide" ring mesh swapped in by visibility, gaze as a group position offset)
// instead of reproducing it, and the approximation was visible. The rig now
// matches the demo's own mechanism directly (see canon-export.js's eye rig
// header), so there is exactly one morph target left: a continuous
// "eye_squash" shape whose weight *is* 1-sy, reproducing any Y-scale in one
// value. "wide" is validated separately as a mesh, not a morph target (see
// EXPECTED_EYE_MESHES below) — a real glTF/three.js node, not a shape.
export const EXPECTED_MORPH_TARGETS = ['eye_squash'];

// The eye node structure every canon export must have, alongside the morph
// target above: an arc mesh (eye_squash) and a wide mesh (no morph) per eye,
// both children of an anchor/group chain gaze is applied to as a position
// offset. mascot-core's Rig (src/mascot/core/rig.js) binds these by name.
export const EXPECTED_EYE_MESHES = [
  'eye_L_arc', 'eye_L_wide', 'eye_R_arc', 'eye_R_wide',
];

// §7: "Per-goober asset ≤ 250 KB (mesh + textures)" and "Mesh budget ≤ 15k
// triangles per goober."
export const MAX_TRIANGLES = 15000;
export const MAX_BYTES = 250 * 1024;

export function countTriangles(scene) {
  let tris = 0;
  scene.traverse((o) => {
    if (!o.isMesh) return;
    const geo = o.geometry;
    tris += (geo.index ? geo.index.count : geo.attributes.position.count) / 3;
  });
  return tris;
}

// Fails loudly (throws) on a missing bone/morph/mesh channel, per §7: "the
// runtime binds by name and fails loudly on a missing channel, so a bad
// export breaks in CI, not on the page." `meshes` is optional (a plain
// object keyed by name, as buildCanonRig returns) — omitted callers just
// skip the eye-mesh check, kept optional so existing joint/morph-only
// call sites (e.g. this file's own tests) don't need updating for a check
// unrelated to what they're asserting.
export function validateRig({ joints, morphTargetNames, meshes }) {
  const missingJoints = EXPECTED_JOINTS.filter((j) => !joints.includes(j));
  if (missingJoints.length) {
    throw new Error(`canon export: missing joint(s): ${missingJoints.join(', ')}`);
  }
  const missingMorphs = EXPECTED_MORPH_TARGETS.filter((m) => !morphTargetNames.includes(m));
  if (missingMorphs.length) {
    throw new Error(`canon export: missing morph target(s): ${missingMorphs.join(', ')}`);
  }
  if (meshes) {
    const missingMeshes = EXPECTED_EYE_MESHES.filter((m) => !meshes[m]);
    if (missingMeshes.length) {
      throw new Error(`canon export: missing eye mesh(es): ${missingMeshes.join(', ')}`);
    }
  }
}
