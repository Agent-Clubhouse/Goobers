// mascot-core: the canon Rig interface, backed by the reference model
// builder instead of a glTF reconstruction of it.
//
// WHY THIS REPLACES rig.js's glTF PATH
// ------------------------------------
// Until now the site rendered goobers two different ways: the coming-soon
// lander built the character procedurally at runtime from demo/src (the
// reference), and /home loaded a baked glTF export of it (the canon asset).
// Every visible difference between the two came from the export being a
// reconstruction — MS10/#155 (resolution), MS12/#161 (eye mesh-swap,
// antenna jiggle, arm/antenna/screen/plate segment counts at *every* tier).
// Each round found more, because a second implementation of a character is
// a permanent source of drift, not a one-time porting cost.
//
// This module ends that by deleting the second implementation. It builds
// the model with demo/src/goober-model.js's buildGooberModel() — the exact
// call the lander makes — and presents it through the same surface
// GooberActor already drives (root / bones / setEyeState / setGaze /
// setEyeDim / applyCasting). actor.js, director.js, floor.js, blink-gaze.js
// and GooberStage.jsx are unchanged: they were never coupled to glTF, only
// to this interface.
//
// THE BONE ALIASING, AND WHY IT IS EXACT, NOT APPROXIMATE
// -------------------------------------------------------
// MASCOTS.md §2's rig names hips / spine / head_aim as three joints. The
// reference model expresses all three as one group (`squash`, origin at the
// feet) and writes a *disjoint* set of properties to it per role:
//
//   hips      -> .scale (squash & stretch), .position.y (walk bob)
//   spine     -> .rotation.x, .rotation.z (lean, sway)
//   head_aim  -> .rotation.y (gaze yaw)
//
// Aliasing all three names to that one group therefore reproduces
// goober.js:490-499 property-for-property, with no write ever clobbering
// another. This is not a simplification of the rig — it is what the
// reference rig physically is. The glTF path's separate head_aim joint was
// the divergence: it turned only the head subtree on gaze where the
// reference turns the whole body (goober.js:496 vs actor.js:481), a delta
// no one had caught because it lived in the skeleton's shape, not in a
// value. Aliasing makes it unrepresentable.
//
// The canon names are also stamped onto the nodes (`.name`), so anything
// that binds by traversal — tests, a future exporter, a debug inspector —
// sees the same §2 vocabulary it saw on the glTF.
import {
  EXPECTED_JOINTS,
  EXPECTED_EYE_MESHES,
} from '../../../demo/src/canon-export-budget.js';
import { buildGooberModel } from '../../../demo/src/goober-model.js';

// Mirrors demo/src/goober.js's own EYE_SCALE table. Kept as a copy rather
// than imported because goober.js holds it module-private; tests/ assert
// the two stay equal (tests/mascot-procedural-rig.test.js) so a change to
// the reference can't silently desync this.
const EYE_SCALE = { open: 1.0, squint: 0.38, flat: 0.09, wide: 1.0 };

/**
 * One goober instance, built from the reference model and exposed through
 * the canon Rig surface. Construction is synchronous and network-free —
 * unlike loadRig(), there is no asset to fetch and no partial-rig failure
 * mode, so callers get a usable rig or an exception, never a pending one.
 */
export class ProceduralRig {
  constructor(preset) {
    if (!preset || !preset.color) {
      throw new Error('ProceduralRig: preset with a color is required');
    }
    // Casting is applied at build time (color -> derived shades, glyph ->
    // chest-plate texture), exactly as the reference does in Goober's
    // constructor. applyCasting() below is consequently a no-op; see there.
    const model = buildGooberModel({ color: preset.color, glyph: preset.glyph });
    const p = model.parts;

    this.model = model;
    this.root = model.root;
    /** Contact-shadow quad. The glTF canon deliberately omitted this and made
     *  it the stage's problem (MS4/MS6); the reference owns it per-instance,
     *  so it comes back here. A stage that draws its own ground shadow can
     *  simply not add it to the scene. */
    this.shadow = model.shadow;

    const squash = model.squash;
    this.bones = {
      root: model.root,
      // three §2 names, one reference group — see this file's header for
      // why the property sets are disjoint and this is exact.
      hips: squash,
      spine: squash,
      head_aim: squash,
      antenna_base: p.antenna,
      antenna_tip: p.ball,
      arm_L: p.armL,
      arm_R: p.armR,
      foot_L: p.footL,
      foot_R: p.footR,
    };

    this.meshes = {
      body: p.body,
      // the arm groups are the rotation pivots; the capsule mesh inside is
      // what carries the skin material
      arm_L_mesh: p.armL.children[0],
      arm_R_mesh: p.armR.children[0],
      antenna_ball: p.ball,
      chest_plate: p.plateGrp.children[0],
      eye_L_arc: p.eyeL.arc,
      eye_L_wide: p.eyeL.wide,
      eye_R_arc: p.eyeR.arc,
      eye_R_wide: p.eyeR.wide,
    };

    this.eyes = [p.eyeL, p.eyeR];
    this.eyesAnchor = p.eyesAnchor;

    // Fail loudly on a missing channel, per §7 — the same contract the glTF
    // path had. Here it guards against the reference model's part names
    // changing out from under the alias table above, which is the only way
    // this can now break.
    for (const joint of EXPECTED_JOINTS) {
      if (!this.bones[joint]) {
        throw new Error(`ProceduralRig: reference model is missing joint "${joint}"`);
      }
    }
    for (const mesh of EXPECTED_EYE_MESHES) {
      if (!this.meshes[mesh]) {
        throw new Error(`ProceduralRig: reference model is missing eye mesh "${mesh}"`);
      }
    }

    // Stamp the §2 vocabulary onto the nodes so name-based traversal still
    // works. hips/spine/head_aim share a node, so it takes the primary name.
    for (const [name, node] of Object.entries(this.bones)) {
      if (!node.name) node.name = name;
    }
    for (const [name, node] of Object.entries(this.meshes)) {
      if (node && !node.name) node.name = name;
    }
    squash.name = 'hips';
  }

  /**
   * Sets both eyes to a named expression state. This is goober.js:561-573
   * verbatim: 'wide' hides the arc and shows the separate ring mesh at the
   * caller's spring-driven `wideScale`; every other state Y-scales the
   * shared arc mesh directly.
   *
   * `blinkEnv` follows GooberActor's convention (0 = open, ~0.94 = closed),
   * which is the complement of the reference's own multiplier — hence the
   * `1 - blinkEnv`. The two produce identical geometry; only the variable's
   * polarity differs, and actor.js is the caller, so its convention wins.
   *
   * No morph targets are involved. The glTF path had to route this through
   * an `eye_squash` morph because glTF cannot portably express "swap between
   * two independent meshes" (#161); with the meshes themselves in hand, the
   * swap is just `.visible`, and the scale is just `.scale`.
   */
  setEyeState(state, blinkEnv = 0, wideScale = 1) {
    if (!(state in EYE_SCALE)) {
      throw new Error(`ProceduralRig.setEyeState: unknown state "${state}"`);
    }
    const wide = state === 'wide';
    const sy = Math.max(EYE_SCALE[state] * (wide ? 1 : 1 - blinkEnv), 0.05);
    for (const eye of this.eyes) {
      eye.arc.visible = !wide;
      eye.wide.visible = wide;
      if (wide) eye.wide.scale.set(wideScale, wideScale, 1);
      else eye.arc.scale.set(1, sy, 1);
    }
  }

  /** Gaze offset in GU, applied as a position on the anchor (x) and each eye
   *  group (y) — goober.js:578-580. Callers pass already-scaled values. */
  setGaze(x, y) {
    this.eyesAnchor.position.x = x;
    for (const eye of this.eyes) eye.grp.position.y = y;
  }

  /** Sleep dimming (0.6) or full opacity (1). The reference writes only
   *  eyeL.arc's material — which dims all four eye meshes, because
   *  goober-model.js:386 shares one THREE.MeshBasicMaterial across them.
   *  (rig.js read that single write as a one-eye oversight it should
   *  "correct" to both eyes; there was never an asymmetry to correct. Same
   *  result either way, but this matches the reference's actual mechanism.) */
  setEyeDim(opacity) {
    this.eyes[0].arc.material.opacity = opacity;
  }

  /**
   * No-op, kept for interface compatibility with the glTF Rig.
   *
   * The glTF canon shipped color-neutral and texture-free, so casting had to
   * be applied after load. buildGooberModel() takes color and glyph up front
   * and derives its own shades, so by the time a ProceduralRig exists it is
   * already cast — re-applying here would clone materials for nothing and
   * break goober-model.js's deliberate material sharing (the eye material in
   * particular; see setEyeDim).
   */
  applyCasting() {}

  /** Releases this instance's GPU resources. The body geometry is shared and
   *  cached across every goober (goober-model.js's BODY_GEO_CACHE), so it is
   *  deliberately not disposed here — same rule src/mascot/stage.js follows. */
  dispose() {
    this.root.traverse((obj) => {
      if (obj.isSkinnedMesh) obj.skeleton.dispose();
      if (obj.geometry && !obj.geometry.userData.shared) obj.geometry.dispose();
      if (obj.material) for (const m of [].concat(obj.material)) m.dispose();
    });
    if (this.shadow) {
      this.shadow.geometry.dispose();
      this.shadow.material.dispose();
    }
  }
}

/** Builds a rig for one cast preset. Synchronous — there is no asset to fetch.
 *  Returned as a resolved promise so Director.cast() and any other existing
 *  `await loadRig(...)` call site keeps working unchanged. */
export function createRig(preset) {
  return new ProceduralRig(preset);
}
