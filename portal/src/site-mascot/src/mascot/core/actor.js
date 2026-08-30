// mascot-core: behavior library (MS3/#137, part of #28). MASCOTS.md §6's
// physics-first vocabulary — idle, walk, hop, fall/land/stagger, gaze,
// carry/interact stubs for MS4's Director, and the full emote composite set
// — driving a Rig (rig.js) loaded from MS1's canon glTF instead of
// goober-model.js's procedural mesh.
//
// The physics/state math below is ported from demo/src/goober.js (the
// tuned feel is the point — not redesigned), with one structural change:
// goober.js writes directly to named mesh/group references it owns
// (parts.footL.position, parts.eyeL.arc.visible, m.squash.scale, ...);
// GooberActor writes to the Rig's *named bones and eye rig* instead
// (rig.bones.foot_L.position, rig.setEyeState(...), rig.bones.hips.scale,
// ...), per §7's "runtime binds by name" contract. Field names (x, z, y,
// grounded, sleeping, anchored, walkTarget, active) are kept identical to
// Goober's on purpose — demo/src/spatial.js's applySeparation() adapter
// reads exactly those fields and calls .bump(nx, nz), so it works against
// GooberActor unchanged, no adapter duplication needed.
//
// Rig-mapping notes (structural differences from the procedural rig):
// - squash/stretch scale -> hips.scale (§2: "hips — squash & stretch
//   center"); lean (droop/working/thinking/dance/bump) -> spine.rotation
//   (§2: "spine — lean, sway"); gaze yaw -> head_aim.rotation.y (§2:
//   "head_aim — gaze, antenna base"). The old rig conflated all three into
//   one "squash" group; the named-bone rig's split maps onto exactly the
//   spec's own bone semantics.
// - Eye state (MS12/#161) drives Rig.setEyeState/setGaze/setEyeDim, which
//   mirror goober.js's actual mesh-swap + Y-scale + position-offset
//   mechanism exactly (see rig.js's doc comments) — not an approximation
//   of it in morph-target shapes. Gaze amplitude constants below are
//   copied verbatim from goober.js's gaze section, not re-derived.
// - The antenna_base/antenna_tip bone pair is driven as one rigid unit
//   (only antenna_base's rotation is set; antenna_tip stays at its rest
//   transform, so it inherits antenna_base's world rotation exactly,
//   adding no extra motion). This isn't a shortcut standing in for
//   something the reference does — goober.js's own antenna is a *single*
//   THREE.Group (goober-model.js's `antenna`, stalk+ball siblings, no
//   independent secondary segment at all): there is no independent
//   base/tip jiggle in the demo to match. §2's two-bone naming is there
//   for a future rig need, not a current animation gap — driving both
//   bones independently here would *add* a perceptible difference from
//   the lander, not remove one.
import { Spring, clamp, lerp, angleDelta } from '../../../demo/src/springs.js';
import { SEPARATION_DIST } from '../../../demo/src/spatial.js';
import { deriveShades } from '../../../demo/src/shades.js';

const G = 14; // GU/s² — matches demo/src/goober.js's cartoon-heavy gravity

export class GooberActor {
  constructor(rig, preset, home, { random = Math.random } = {}) {
    this.rig = rig;
    this.name = preset.name;
    this.color = preset.color;
    this.glyph = preset.glyph;
    this.temperament = preset.temperament;
    this.p = preset.params;
    this.home = { ...home };
    this.active = true;
    this.random = random;

    rig.applyCasting({ color: preset.color, plateColor: deriveShades(preset.color).plate, glyph: preset.glyph });

    this.x = home.x; this.z = home.z;
    this.y = 0; this.vy = 0; this.hvx = 0; this.hvz = 0;
    this.grounded = true;
    this.hangT = 0;
    this.anchored = false;
    this.crowded = false;

    const k = this.p.stiffness, d = this.p.damping;
    this.squashS = new Spring(1, 170 * k, 11 * d);
    this.headingS = new Spring(0, 42 * k, 8.5 * d);
    this.droopS = new Spring(0, 40, 8);
    this.antX = new Spring(0, 55 * k, 4.2 * d);
    this.antZ = new Spring(0, 55 * k, 4.2 * d);
    this.wideS = new Spring(1, 220, 12);

    this.eyeState = 'open';
    this.eyeRevert = 0;
    this.tempWide = false;
    this.blinkIn = this._nextBlink();
    this.blinkT = -1;
    this.gazeYaw = 0; this.gazeEyeX = 0; this.gazeEyeY = 0;

    this.walkTarget = null; this.walkResolve = null;
    this.walkBackward = false;
    this.hopResolve = null;
    this.phase = this.random() * Math.PI * 2;
    this.moving = false;
    this.waveT = -1; this.waveResolve = null;
    this.workingT = 0;
    this.thinkT = -1; this.thinkE = 0;
    this.cheerT = -1;
    this.danceT = -1;
    this.bumpT = -1; this.bumpCd = 0;
    this.bumpLx = 0; this.bumpLz = 0;
    this.sleeping = false;
    this.idleIn = 2 + this.random() * 5;
    this.driftSeed = this.random() * 100;
    this.t = 0;

    this.prevX = this.x; this.prevZ = this.z;
    this.antPrevVx = 0; this.antPrevVz = 0;
  }

  _nextBlink() {
    const mean = 60 / Math.max(this.p.blinkRate, 1);
    return mean * (0.6 + this.random() * 0.8);
  }

  // ------------------------------------------------------------ primitives

  walkTo(x, z) {
    this.resolveWalk();
    this.walkBackward = false;
    this.walkTarget = { x, z };
    return new Promise((res) => { this.walkResolve = res; });
  }
  walkBackwardTo(x, z) {
    this.resolveWalk();
    this.walkBackward = true;
    this.walkTarget = { x, z };
    return new Promise((res) => { this.walkResolve = res; });
  }
  resolveWalk() {
    if (this.walkResolve) { this.walkResolve(); this.walkResolve = null; }
    this.walkBackward = false;
  }

  hop(h, toward = false) {
    if (!this.grounded || this.sleeping) return Promise.resolve();
    this.squashS.kick(-1.6 * this.p.reactivity);
    this.vy = Math.sqrt(2 * G * h) / Math.sqrt(this.p.mass);
    if (toward && this.walkTarget) {
      const dx = this.walkTarget.x - this.x, dz = this.walkTarget.z - this.z;
      const dist = Math.hypot(dx, dz) || 1;
      const sp = Math.min(0.9 * this.p.speed, dist / 0.35);
      this.hvx = (dx / dist) * sp; this.hvz = (dz / dist) * sp;
    }
    this.grounded = false;
    this.hangT = 0;
    return new Promise((res) => { this.hopResolve = res; });
  }

  wave() {
    this.waveT = 0;
    return new Promise((res) => { this.waveResolve = res; });
  }

  setEyes(state, holdS = 0) {
    if (state === 'wide' && this.eyeState !== 'wide') this.wideS.snap(0.4);
    this.eyeState = state;
    this.eyeRevert = holdS;
  }

  stagger() {
    this.headingS.kick((this.random() < 0.5 ? -1 : 1) * 7 * this.p.reactivity);
    this.squashS.kick(-1.2);
    this.antX.kick(4); this.antZ.kick(3);
    this.setEyes('squint', 0.5);
  }

  async emote(name, sleep) {
    const wait = sleep || ((ms) => new Promise((r) => setTimeout(r, ms)));
    const r = this.p.reactivity;
    switch (name) {
      case 'surprise':
        this.setEyes('wide', 0.9);
        this.antX.kick(5 * r);
        await this.hop(0.16 + 0.1 * r);
        break;
      case 'working':
        this.setEyes('squint', 2.4);
        this.workingT = 2.4;
        await wait(2400);
        break;
      case 'blocked':
        this.setEyes('flat', 2.4);
        this.droopS.target = 0.5 * r;
        await wait(2200);
        this.droopS.target = 0;
        break;
      case 'celebrate':
        this.setEyes('wide', 1.8);
        await this.hop(0.2 + 0.08 * r);
        await wait(60);
        await this.hop(0.26 + 0.1 * r);
        break;
      case 'stagger':
        this.stagger();
        await wait(600);
        break;
      case 'thinking':
        this.thinkT = 0;
        await wait(2600);
        break;
      case 'cheer':
        this.setEyes('wide', 1.7);
        this.cheerT = 0;
        this.antX.kick(4 * r);
        await this.hop(0.26 + 0.12 * r);
        await wait(800);
        break;
      case 'dance':
        this.walkTarget = null;
        this.danceT = 0;
        await wait(1660);
        this.setEyes('wide', 0.7);
        this.squashS.kick(-0.8);
        await wait(300);
        break;
      case 'bump': {
        const a = this.random() * Math.PI * 2;
        this.bumpCd = 0;
        this.bump(Math.cos(a), Math.sin(a));
        await wait(900);
        break;
      }
    }
  }

  bump(nx, nz) {
    if (this.sleeping) {
      this.bumpCd = 2;
      this.wake();
      return;
    }
    if (this.bumpCd > 0 || !this.grounded) return;
    this.bumpCd = 3;
    const r = this.p.reactivity;
    const h = this.headingS.value;
    this.bumpLx = nx * Math.cos(h) - nz * Math.sin(h);
    this.bumpLz = nx * Math.sin(h) + nz * Math.cos(h);
    this.bumpT = 0;
    this.squashS.kick(-1.5 * r);
    this.headingS.kick(Math.sign(this.bumpLx || 1) * 2.4 * r);
    this.antX.kick(5 * r);
    this.antZ.kick((-4 * this.bumpLx || 3) * r);
    this.setEyes('wide', 0.8);
  }

  sleepMode(on) {
    this.sleeping = on;
    if (on) {
      this.walkTarget = null; this.resolveWalk();
      this.setEyes('flat', 0);
      this.droopS.target = 0.55;
    } else {
      this.droopS.target = 0;
      this.setEyes('open', 0);
    }
  }
  wake() {
    if (this.sleeping) { this.sleepMode(false); this.setEyes('wide', 0.8); }
  }

  resetTasks() {
    this.walkTarget = null;
    this.resolveWalk();
    if (this.hopResolve) { this.hopResolve(); this.hopResolve = null; }
    if (this.waveResolve) { this.waveResolve(); this.waveResolve = null; }
    this.waveT = -1;
    this.workingT = 0;
    this.thinkT = -1; this.thinkE = 0;
    this.cheerT = -1;
    this.danceT = -1;
    this.droopS.target = 0;
    if (this.eyeState !== 'open') this.setEyes('open', 0);
  }

  setActive(on) {
    this.active = on;
    this.rig.root.visible = on;
    if (!on) this.resetTasks();
  }


  // ---------------------------------------------------------------- update

  update(dt, ctx) {
    const p = this.p;
    this.t += dt;
    const floorY = ctx.floorY;

    // --- locomotion
    this.moving = false;
    if (this.walkTarget && !this.sleeping) {
      const dx = this.walkTarget.x - this.x;
      const dz = this.walkTarget.z - this.z;
      const dist = Math.hypot(dx, dz);
      const arrived = dist < 0.06 || (this.crowded && dist < SEPARATION_DIST + 0.06);
      if (arrived) {
        this.walkTarget = null;
        this.resolveWalk();
      } else {
        const desired = Math.atan2(dx, dz);
        const facing = this.walkBackward ? desired + Math.PI : desired;
        this.headingS.target = this.headingS.value + angleDelta(this.headingS.value, facing);
        if (this.grounded) {
          if (p.gait === 'hop') {
            this.hop(0.11, true);
          } else {
            const sp = Math.min(0.9 * p.speed, dist / 0.25 + 0.15);
            this.x += (dx / dist) * sp * dt;
            this.z += (dz / dist) * sp * dt;
            this.phase += dt * p.cadence * Math.PI;
            this.moving = true;
          }
        }
      }
    }

    // --- vertical physics & the floor contract
    if (this.grounded) {
      if (this.y - floorY > 0.045) {
        this.grounded = false;
        this.hangT = 0.13;
        this.tempWide = true;
        this.wideS.snap(0.4);
      } else {
        this.y = floorY;
      }
    } else if (this.hangT > 0) {
      this.hangT -= dt;
    } else {
      this.vy -= G * p.mass * dt;
      this.y += this.vy * dt;
      this.x += this.hvx * dt;
      this.z += this.hvz * dt;
      if (this.y <= floorY && this.vy <= 0) {
        const impact = Math.abs(this.vy);
        this.y = floorY; this.vy = 0; this.hvx = 0; this.hvz = 0;
        this.grounded = true;
        this.tempWide = false;
        this.squashS.kick(-impact * 0.55 * p.mass);
        if (this.hopResolve) { this.hopResolve(); this.hopResolve = null; }
        if (impact > 3.4) this.stagger();
      }
    }

    // --- idle life signs
    if (this.grounded && !this.walkTarget && !this.sleeping && this.waveT < 0) {
      this.idleIn -= dt;
      if (this.idleIn <= 0) {
        this.idleIn = 4 + this.random() * 5;
        this.headingS.kick((this.random() - 0.5) * 1.6 * p.reactivity);
        this.squashS.kick(-0.25 * p.reactivity);
      }
    }

    // --- thinking envelope
    if (this.thinkT >= 0) {
      this.thinkT += dt;
      const T = 2.6;
      const rIn = Math.min(this.thinkT / 0.35, 1);
      const rOut = clamp((T - this.thinkT) / 0.45, 0, 1);
      this.thinkE = Math.min(rIn, rOut);
      if (this.thinkT >= T) { this.thinkT = -1; this.thinkE = 0; }
    }

    // --- dance choreography
    let danceE = 0, danceSway = 0, danceBounce = 0;
    if (this.danceT >= 0) {
      this.danceT += dt;
      const T = 1.92;
      const rIn = Math.min(this.danceT / 0.2, 1);
      const rOut = clamp((T - this.danceT) / 0.3, 0, 1);
      danceE = Math.min(rIn, rOut) * Math.min(p.reactivity, 1.3);
      const w = (2 * Math.PI) / 0.96;
      danceSway = Math.sin(w * this.danceT);
      danceBounce = Math.abs(Math.sin(w * 2 * this.danceT));
      this.phase += dt * (Math.PI / 0.24);
      if (this.danceT >= T) this.danceT = -1;
    }
    const dancing = danceE > 0;

    // --- bump recoil envelope
    if (this.bumpCd > 0) this.bumpCd -= dt;
    let bumpE = 0;
    if (this.bumpT >= 0) {
      this.bumpT += dt;
      const T = 0.7;
      const rIn = Math.min(this.bumpT / 0.1, 1);
      const rOut = clamp((T - this.bumpT) / 0.45, 0, 1);
      bumpE = Math.min(rIn, rOut);
      if (this.bumpT >= T) { this.bumpT = -1; bumpE = 0; }
    }

    // --- blink
    if (!this.sleeping && this.eyeState === 'open') {
      this.blinkIn -= dt;
      if (this.blinkIn <= 0) { this.blinkT = 0; this.blinkIn = this._nextBlink(); }
    }
    let blinkEnv = 0; // 0 = not blinking, 1 = fully closed
    if (this.blinkT >= 0) {
      this.blinkT += dt;
      const bt = this.blinkT / 0.22;
      blinkEnv = bt >= 1 ? 0 : (bt < 0.45 ? (bt / 0.45) * 0.94 : 0.94 - ((bt - 0.45) / 0.55) * 0.94);
      if (bt >= 1) this.blinkT = -1;
    }

    // --- eye state hold/revert
    if (this.eyeRevert > 0) {
      this.eyeRevert -= dt;
      if (this.eyeRevert <= 0) { this.eyeState = 'open'; this.eyeRevert = 0; }
    }

    // --- gaze
    // eyeX/eyeY are GU-scale position offsets applied directly to the eye
    // rig (Rig.setGaze), matching goober.js's gaze section exactly — the
    // 0.05/0.035/0.018/0.012 amplitudes below are copied from there
    // verbatim, not re-derived, since Rig.setGaze is now a direct position
    // write rather than a clamp-and-scale helper (MS12/#161).
    let relYaw = 0, eyeX = 0, eyeY = 0;
    if (ctx.gazePoint && !this.sleeping) {
      const wy = Math.atan2(ctx.gazePoint.x - this.x, ctx.gazePoint.z - this.z);
      const rel = angleDelta(this.headingS.value, wy);
      relYaw = clamp(rel, -0.45, 0.45) * 0.7;
      eyeX = clamp(rel, -1, 1) * 0.05;
      eyeY = clamp((ctx.gazePoint.y - 0.7) / 2.5, -1, 1) * 0.035;
    } else {
      eyeX = Math.sin(this.t * 0.35 + this.driftSeed) * 0.018;
      eyeY = Math.sin(this.t * 0.23 + this.driftSeed * 2) * 0.012;
    }
    if (this.thinkE > 0) {
      relYaw = 0.18 * this.thinkE;
      eyeX = 0.05 * this.thinkE;
      eyeY = 0.05 * this.thinkE;
    }
    const gz = clamp(dt * 6 * p.gazeSpeed, 0, 1);
    this.gazeYaw = lerp(this.gazeYaw, relYaw, gz);
    this.gazeEyeX = lerp(this.gazeEyeX, eyeX, gz);
    this.gazeEyeY = lerp(this.gazeEyeY, eyeY, gz);

    // --- springs
    this.squashS.update(dt);
    this.headingS.update(dt);
    this.droopS.update(dt);
    this.wideS.target = 1; this.wideS.update(dt);

    const vx = (this.x - this.prevX) / Math.max(dt, 1e-4);
    const vz = (this.z - this.prevZ) / Math.max(dt, 1e-4);
    this.antX.kick(-(vz - this.antPrevVz) * 0.9);
    this.antZ.kick((vx - this.antPrevVx) * 0.9);
    this.antPrevVx = vx; this.antPrevVz = vz;
    this.antX.update(dt); this.antZ.update(dt);
    this.prevX = this.x; this.prevZ = this.z;

    this._applyToRig(dt, { dancing, danceSway, danceBounce, danceE, bumpE, blinkEnv, floorY });
  }

  // ---------------------------------------------------- apply to the rig

  _applyToRig(dt, motion) {
    const { dancing, danceSway, danceBounce, danceE, bumpE, blinkEnv, floorY } = motion;
    const rig = this.rig, bones = rig.bones;

    rig.root.position.set(this.x, this.y, this.z);
    rig.root.rotation.y = this.headingS.value;

    // squash & stretch, volume-preserving — hips is "the squash & stretch
    // center" (§2), the direct rig-tree equivalent of the old squash group.
    let s = clamp(this.squashS.value, 0.8, 1.18);
    if (!this.grounded) s = clamp(s * (1 + Math.abs(this.vy) * 0.03), 0.8, 1.16);
    const sxz = 1 / Math.sqrt(s);
    bones.hips.scale.set(sxz, s, sxz);

    // lean -> spine ("lean, sway" per §2)
    let leanX = this.droopS.value * 0.4;
    let leanZ = 0;
    if (this.workingT > 0) { this.workingT -= dt; leanZ = Math.sin(this.t * 9) * 0.035; }
    if (this.thinkE > 0) leanZ += (0.06 + Math.sin(this.t * 2.1) * 0.025) * this.thinkE;
    if (dancing) leanZ += danceSway * 0.08 * danceE;
    if (bumpE > 0) {
      const amp = 0.32 * clamp(this.p.reactivity, 0.5, 1.5) * bumpE;
      leanX += this.bumpLz * amp;
      leanZ -= this.bumpLx * amp;
    }
    if (this.sleeping) leanZ = Math.sin(this.t * 1.1) * 0.05;
    bones.spine.rotation.x = leanX;
    bones.spine.rotation.z = leanZ;

    // gaze yaw -> head_aim ("gaze, antenna base" per §2)
    bones.head_aim.rotation.y = this.gazeYaw + (dancing ? danceSway * 0.32 * danceE : 0);

    // walk bob rides on hips alongside the squash scale
    let bob = this.moving ? Math.abs(Math.sin(this.phase)) * 0.035 : 0;
    if (dancing) bob += danceBounce * 0.055 * danceE;
    bones.hips.position.y = bob;

    const lift = this.moving || dancing ? 0.05 : 0;
    bones.foot_L.position.y = 0.115 + Math.max(0, Math.sin(this.phase)) * lift;
    bones.foot_R.position.y = 0.115 + Math.max(0, -Math.sin(this.phase)) * lift;
    const stride = this.moving ? 0.06 : 0;
    bones.foot_L.position.z = 0.02 - Math.cos(this.phase) * stride;
    bones.foot_R.position.z = 0.02 + Math.cos(this.phase) * stride;

    // arms
    const swing = this.moving ? Math.sin(this.phase) * 0.45 : 0;
    bones.arm_L.rotation.x = swing;
    bones.arm_R.rotation.x = -swing;
    let armLz = -0.28 - this.droopS.value * 0.25;
    let armRz = 0.28 + this.droopS.value * 0.25;
    if (dancing) {
      armLz = -0.28 - Math.max(0, danceSway) * 1.7 * danceE;
      armRz = 0.28 + Math.max(0, -danceSway) * 1.7 * danceE;
      bones.arm_L.rotation.x = 0;
      bones.arm_R.rotation.x = 0;
    }
    if (this.cheerT >= 0) {
      this.cheerT += dt;
      const c = this.cheerT, up = 2.5;
      if (c < 0.18) {
        const k = c / 0.18;
        armLz = lerp(-0.28, -up, k); armRz = lerp(0.28, up, k);
      } else if (c < 1.15) {
        const wob = Math.sin((c - 0.18) * 14) * 0.1;
        armLz = -up + wob; armRz = up - wob;
      } else if (c < 1.45) {
        const k = (c - 1.15) / 0.3;
        armLz = lerp(-up, -0.28, k); armRz = lerp(up, 0.28, k);
      } else {
        this.cheerT = -1;
      }
      bones.arm_L.rotation.x = 0;
      bones.arm_R.rotation.x = 0;
    }
    if (this.waveT >= 0) {
      this.waveT += dt;
      const w = this.waveT;
      if (w < 0.25) armRz = lerp(0.28, 2.45, w / 0.25);
      else if (w < 0.85) armRz = 2.45 + Math.sin((w - 0.25) * 18) * 0.3;
      else if (w < 1.1) armRz = lerp(2.45, 0.28, (w - 0.85) / 0.25);
      else {
        this.waveT = -1;
        if (this.waveResolve) { this.waveResolve(); this.waveResolve = null; }
      }
      bones.arm_R.rotation.x = 0;
    }
    bones.arm_L.rotation.z = armLz;
    bones.arm_R.rotation.z = armRz;

    // antenna — both bones driven rigidly together (see file header)
    const antRotX = clamp(this.antX.value, -0.7, 0.7) - this.droopS.value * 0.5;
    const antRotZ = clamp(this.antZ.value, -0.7, 0.7);
    bones.antenna_base.rotation.x = antRotX;
    bones.antenna_base.rotation.z = antRotZ;

    // eyes — matches goober.js's mesh-swap + Y-scale mechanism exactly
    // (rig.js's setEyeState): tempWide overrides eyeState while airborne
    // ("wide overrides everything", same rule as before), blinkEnv
    // continuously thins the arc for every other state, and wideS is the
    // spring-driven bounce-in scale for the ring mesh.
    const state = this.tempWide ? 'wide' : this.eyeState;
    rig.setEyeState(state, blinkEnv, clamp(this.wideS.value, 0.3, 1.25));
    rig.setGaze(this.gazeEyeX, this.gazeEyeY);
    rig.setEyeDim(this.sleeping ? 0.6 : 1);

    // Contact shadow — goober.js:582-588 verbatim. It rides the floor rather
    // than the body, and grows/fades with altitude so a goober reads as
    // airborne even when nothing else in frame gives away its height.
    // Optional: the glTF canon had no shadow (contact shadows were pushed to
    // the stage in MS4/MS6), so a rig without one is still valid here.
    if (rig.shadow) {
      const h = Math.max(this.y - floorY, 0);
      rig.shadow.position.set(this.x, floorY + 0.012, this.z);
      const sc = 0.95 + h * 0.55;
      rig.shadow.scale.set(sc, sc, 1);
      rig.shadow.material.opacity = clamp(0.95 / (1 + h * 2.2), 0.15, 0.95);
    }
  }
}
