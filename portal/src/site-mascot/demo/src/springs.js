// Numeric damped spring — the backbone of all goober motion (MASCOTS.md §6).
// Semi-implicit Euler; stable for the stiffness range we use at 60fps.

export class Spring {
  constructor(value = 0, stiffness = 120, damping = 14) {
    this.value = value;
    this.target = value;
    this.vel = 0;
    this.stiffness = stiffness;
    this.damping = damping;
  }
  kick(v) {
    this.vel += v;
    return this;
  }
  snap(v) {
    this.value = this.target = v;
    this.vel = 0;
    return this;
  }
  update(dt) {
    const f = (this.target - this.value) * this.stiffness - this.vel * this.damping;
    this.vel += f * dt;
    this.value += this.vel * dt;
    return this.value;
  }
}

export const clamp = (v, a, b) => Math.min(b, Math.max(a, v));
export const lerp = (a, b, t) => a + (b - a) * t;

// Smallest signed angle from a to b.
export const angleDelta = (a, b) => {
  let d = (b - a) % (Math.PI * 2);
  if (d > Math.PI) d -= Math.PI * 2;
  if (d < -Math.PI) d += Math.PI * 2;
  return d;
};
