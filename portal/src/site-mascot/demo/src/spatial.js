// Goober-to-goober spatial layer — MASCOTS.md §6.
// Multiple `walk(to)` calls know nothing about each other; this layer is the
// shared spatial awareness between cast members on one floor. It resolves
// personal-space overlap with a gentle, capped positional correction — never
// an impulse: no knockback, no bounce. Pure position math (no three.js) so
// it runs under node --test.

// Body half-width at the widest point is ~0.405 GU (§2: 0.81 wide); a small
// margin on top gives the personal-space disc every grounded goober projects
// onto the floor.
export const PERSONAL_RADIUS = 0.42; // GU
export const SEPARATION_DIST = PERSONAL_RADIUS * 2;
// Closing to 80% of separation distance counts as an actual bump — the
// avoidance push makes this rare (fast head-on closes, landings), and when
// it happens it's an opportunity for comic reaction, not a bug to hide.
export const CONTACT_DIST = SEPARATION_DIST * 0.8;

const PUSH_RATE = 6;    // 1/s — correction speed per GU of overlap
const MAX_PUSH = 1.4;   // GU/s — cap so a correction never reads as a shove
const SWIRL = 0.35;     // lateral bias for walkers, so crossing paths reads
                        // as routing around, not a head-on grind
const Y_BAND = 1.0;     // GU — bodies further apart vertically don't interact

// bodies: [{ x, z, y, movable, moving }]
// Returns per-body { dx, dz, crowded }. Overlapping pairs push apart along
// the pair axis, split by who can move (an immovable body stands its
// ground; the other yields fully). Walkers pick up an opposite-side lateral
// bias with consistent chirality so two crossers pass rather than stall.
export function separationOffsets(bodies, dt) {
  const out = bodies.map(() => ({ dx: 0, dz: 0, crowded: false, contact: null }));
  for (let i = 0; i < bodies.length; i++) {
    for (let j = i + 1; j < bodies.length; j++) {
      const a = bodies[i], b = bodies[j];
      if (!a.movable && !b.movable) continue;
      if (Math.abs((a.y || 0) - (b.y || 0)) > Y_BAND) continue;
      let dx = a.x - b.x, dz = a.z - b.z;
      let d = Math.hypot(dx, dz);
      if (d >= SEPARATION_DIST) continue;
      if (d < 1e-6) { dx = 1; dz = 0; d = 1e-6; } // coincident: deterministic axis
      const nx = dx / d, nz = dz / d;
      // Real contact: report the away-normal to both parties (even the
      // immovable one — a sleeping goober still gets woken by a bonk).
      if (d < CONTACT_DIST && (a.moving || b.moving)) {
        out[i].contact = { nx, nz };
        out[j].contact = { nx: -nx, nz: -nz };
      }
      const overlap = SEPARATION_DIST - Math.min(d, SEPARATION_DIST);
      const push = Math.min(overlap * PUSH_RATE, MAX_PUSH) * dt;
      const wa = a.movable ? 1 : 0, wb = b.movable ? 1 : 0;
      const tot = wa + wb;
      const sx = -nz, sz = nx; // perpendicular, fixed chirality
      if (wa) {
        const s = a.moving ? SWIRL : 0;
        out[i].dx += (nx + sx * s) * push * (wa / tot);
        out[i].dz += (nz + sz * s) * push * (wa / tot);
        out[i].crowded = true;
      }
      if (wb) {
        const s = b.moving ? SWIRL : 0;
        out[j].dx -= (nx + sx * s) * push * (wb / tot);
        out[j].dz -= (nz + sz * s) * push * (wb / tot);
        out[j].crowded = true;
      }
    }
  }
  // Cap the aggregate per body so a pile-up never flings anyone.
  const cap = MAX_PUSH * dt;
  for (const o of out) {
    const m = Math.hypot(o.dx, o.dz);
    if (m > cap) { o.dx *= cap / m; o.dz *= cap / m; }
  }
  return out;
}

// Adapter for Goober instances. Airborne, sleeping, and anchored
// (carry/interact) goobers stand their ground — others route around them.
// Overlap created by a landing resolves over the next grounded frames.
//
// `moving` (contact-detection eligibility, separate from `movable` above)
// counts an active walk target OR being airborne — a goober falling into
// someone standing there is real contact regardless of whether anything
// has a walkTarget (MS5's scroll-fall companion never calls walkTo; it's
// entirely floor-driven vertical motion). Safe to widen: an airborne
// goober is never `movable` (so this doesn't change who gets pushed), and
// bump() itself already refuses to fire while airborne
// (`if (... || !this.grounded) return;`) — so this only ever lets the
// *other*, grounded party correctly register contact from something
// falling past or into it, never causes a mid-air bump.
export function applySeparation(goobers, dt) {
  const active = goobers.filter((g) => g.active);
  const bodies = active.map((g) => ({
    x: g.x, z: g.z, y: g.y,
    movable: g.grounded && !g.sleeping && !g.anchored,
    moving: !!g.walkTarget || !g.grounded,
  }));
  const offs = separationOffsets(bodies, dt);
  active.forEach((g, i) => {
    g.x += offs[i].dx;
    g.z += offs[i].dz;
    g.crowded = offs[i].crowded;
    const c = offs[i].contact;
    if (c && typeof g.bump === 'function') g.bump(c.nx, c.nz);
  });
}
