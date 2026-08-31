// Procedural goober — canon proportions per MASCOTS.md §2, 1 unit = 1 GU.
// The demo builds the model in code; production will load the same anatomy
// from glTF. Feet rest at y=0, crown at ~y=1, antenna above.

import * as THREE from 'three';
import { MarchingCubes } from 'three/addons/objects/MarchingCubes.js';
import { mergeVertices } from 'three/addons/utils/BufferGeometryUtils.js';
import { deriveShades } from './shades.js';

// ---------------------------------------------------------------- materials

const toonGradient = (() => {
  const data = new Uint8Array([90, 170, 255]);
  const tex = new THREE.DataTexture(data, 3, 1, THREE.RedFormat);
  tex.minFilter = THREE.NearestFilter;
  tex.magFilter = THREE.NearestFilter;
  tex.needsUpdate = true;
  return tex;
})();

export function skinMaterial(variant, colorHex) {
  const color = new THREE.Color(colorHex);
  if (variant === 'glossy') {
    return new THREE.MeshPhysicalMaterial({
      color, roughness: 0.32, metalness: 0.0,
      clearcoat: 1.0, clearcoatRoughness: 0.14,
    });
  }
  if (variant === 'toon') {
    return new THREE.MeshToonMaterial({ color, gradientMap: toonGradient });
  }
  // 'matte' — the spec's matte-vinyl default
  return new THREE.MeshStandardMaterial({ color, roughness: 0.6, metalness: 0.0 });
}

// -------------------------------------------------------------- color logic

// Moved to shades.js (imported above, re-exported here so this stays the
// place existing callers already import it from) — see that file's header
// for why: production code needs this pure formula without dragging in
// this file's module-level side effects (the toonGradient texture below)
// and everything they block a bundler from tree-shaking (MarchingCubes,
// canvas textures, the whole procedural model).
export { deriveShades };

// ---------------------------------------------------------------- geometry

function roundedRectShape(w, h, r) {
  const s = new THREE.Shape();
  const x = -w / 2, y = -h / 2;
  s.moveTo(x + r, y);
  s.lineTo(x + w - r, y);
  s.absarc(x + w - r, y + r, r, -Math.PI / 2, 0, false);
  s.lineTo(x + w, y + h - r);
  s.absarc(x + w - r, y + h - r, r, 0, Math.PI / 2, false);
  s.lineTo(x + r, y + h);
  s.absarc(x + r, y + h - r, r, Math.PI / 2, Math.PI, false);
  s.lineTo(x, y + r);
  s.absarc(x + r, y + r, r, Math.PI, Math.PI * 1.5, false);
  return s;
}

// Cross-section shape. Depth = width × BODY_DEPTH (1.0 would be a full body
// of revolution). BODY_EDGE is the superellipse exponent: 2 is an ellipse;
// higher flattens the front/back and tightens the corner radius where the
// body turns to the sides — still no hard edges. The front silhouette is
// unchanged by both. Tune here to explore.
export const BODY_DEPTH = 0.7;
export const BODY_EDGE = 2.6;

// Depth (z half-extent) of the cross-section at lateral offset x, for a
// cross-section of half-width r: |x/r|^E + |z/(r·D)|^E = 1.
export function crossDepth(x, r) {
  const c = Math.min(Math.abs(x) / r, 1);
  return r * BODY_DEPTH * Math.pow(1 - Math.pow(c, BODY_EDGE), 1 / BODY_EDGE);
}

// Rounded-slab profile per the reference art: near-vertical sides, broad
// dome, head almost as wide as the belly, body running all the way to the
// ground (the leg cleave is carved by the SDF below). Max radius ~0.405 GU.
const PROFILE_SAMPLES = (() => {
  const pts = [
    [0.29, 0.0], [0.32, 0.045], [0.355, 0.115], [0.385, 0.20],
    [0.405, 0.42], [0.40, 0.58], [0.385, 0.70], [0.355, 0.80],
    [0.30, 0.88], [0.215, 0.945], [0.10, 0.985], [0.015, 1.0],
  ].map(([r, y]) => new THREE.Vector3(r, y, 0));
  return new THREE.CatmullRomCurve3(pts).getPoints(200);
})();

// Body radius at a given height, from the same samples the lathe uses.
export function bodyRadiusAt(y) {
  const s = PROFILE_SAMPLES;
  if (y <= s[0].y) return s[0].x;
  for (let i = 1; i < s.length; i++) {
    if (s[i].y >= y) {
      const t = (y - s[i - 1].y) / (s[i].y - s[i - 1].y || 1e-6);
      return s[i - 1].x + (s[i].x - s[i - 1].x) * t;
    }
  }
  return s[s.length - 1].x;
}

// Leg cleave: an elliptical arch (semi-axes a×b, center cy, swept along z)
// smooth-subtracted from the body's underside. Widest exactly at the floor
// (cy=0) so the inner wall rises vertically from the ground and rolls over
// smoothly to the groin apex at cy + b — no under-tuck, which would leave a
// pointy overhanging "toe" wedge at the bottom inner corner.
// The arch widens sharply toward the feet's front/back tips (quartic in
// |z|/zr, zr ≈ foot depth): the footprint's inner edge sweeps outward to
// meet the outer curve tangentially, making each ground pad a smooth oval
// with no corners. Front-view gap stays 2a (the z=0 wall is the narrowest).
const LEG_ARCH = { a: 0.05, b: 0.105, cy: 0.0, zr: 0.2, tip: 1.3, k: 0.05 };
const GROUND_K = 0.04; // rounding of the flat stance cut at the floor

// Polynomial smooth max (inigo quilez): blends the max of two fields over
// width k, turning hard intersection/subtraction seams into soft fillets.
function smax(a, b, k) {
  const h = Math.min(Math.max(0.5 + 0.5 * (a - b) / k, 0), 1);
  return b + (a - b) * h + k * h * (1 - h);
}

// Signed "distance" to the full body: superellipse cross-section swept along
// the profile, floor-cut at y=0, crown pinhole capped, leg arch carved out.
// The zero set above the leg/floor blends matches crossDepth exactly, which
// is what keeps the decal projections flush.
function bodySDF(x, y, z) {
  const r = bodyRadiusAt(Math.min(Math.max(y, 0), 1));
  const n = Math.pow(
    Math.pow(Math.abs(x) / r, BODY_EDGE) +
    Math.pow(Math.abs(z) / (r * BODY_DEPTH), BODY_EDGE),
    1 / BODY_EDGE
  );
  let d = (n - 1) * r * BODY_DEPTH;
  d = smax(d, -y, GROUND_K);
  d = smax(d, y - 1.002, 0.008);
  const zt = Math.abs(z) / LEG_ARCH.zr;
  const az = LEG_ARCH.a * (1 + LEG_ARCH.tip * zt * zt * zt * zt);
  const arch = (Math.hypot(x / az, (y - LEG_ARCH.cy) / LEG_ARCH.b) - 1) * az;
  return smax(d, -arch, LEG_ARCH.k);
}

// Skin weights: root everywhere, blending into the left/right leg bones
// toward the bottom. Vertices on the arch walls split between both legs, so
// a single-foot lift stretches the cleave softly instead of tearing it.
function skinAttributes(geo) {
  const pos = geo.attributes.position;
  const idx = new Uint16Array(pos.count * 4);
  const wgt = new Float32Array(pos.count * 4);
  for (let i = 0; i < pos.count; i++) {
    let t = Math.min(Math.max((0.26 - pos.getY(i)) / 0.16, 0), 1);
    const drop = t * t * (3 - 2 * t); // 0 at hip (y>=0.26) -> 1 at foot (y<=0.10)
    const side = Math.min(Math.max(pos.getX(i) / 0.09, -1), 1);
    idx[i * 4 + 1] = 1; wgt[i * 4 + 1] = drop * 0.5 * (1 - side); // footL
    idx[i * 4 + 2] = 2; wgt[i * 4 + 2] = drop * 0.5 * (1 + side); // footR
    idx[i * 4] = 0; wgt[i * 4] = 1 - drop;                        // root
  }
  geo.setAttribute('skinIndex', new THREE.BufferAttribute(idx, 4));
  geo.setAttribute('skinWeight', new THREE.BufferAttribute(wgt, 4));
}

// Polygonize the SDF once per resolution and share the result — every goober
// at a given resolution uses the same body. Marching-cubes normals come from
// the field gradient, so shading is smooth with no seams. Flagged shared so
// despawn cleanup leaves it alone.
//
// `res` defaults to the live-render resolution (112, ~53.5k triangles); the
// canon glTF export (demo/src/canon-export.js) calls this with a much lower
// `res` to hit the §7 mesh budget (≤15k tris/goober) — skin weights are
// recomputed per-vertex from position (skinAttributes below), not from mesh
// topology, so a lower-res mesh gets correct skinning for free.
const BODY_GEO_CACHE = new Map();
export function bodyGeometry(res = 112) {
  if (BODY_GEO_CACHE.has(res)) return BODY_GEO_CACHE.get(res);
  const RES = res, HALF = 0.62, CY = 0.5;
  const mc = new MarchingCubes(RES, new THREE.MeshBasicMaterial(), false, false, 120000);
  mc.isolation = 0;
  for (let iz = 0; iz < RES; iz++) {
    const z = ((iz - RES / 2) / (RES / 2)) * HALF;
    for (let iy = 0; iy < RES; iy++) {
      const y = CY + ((iy - RES / 2) / (RES / 2)) * HALF;
      const row = RES * RES * iz + RES * iy;
      for (let ix = 0; ix < RES; ix++) {
        const x = ((ix - RES / 2) / (RES / 2)) * HALF;
        mc.field[row + ix] = -bodySDF(x, y, z); // MC wants inside-positive
      }
    }
  }
  mc.update();
  let geo = new THREE.BufferGeometry();
  geo.setAttribute('position', new THREE.BufferAttribute(mc.positionArray.slice(0, mc.count * 3), 3));
  geo.setAttribute('normal', new THREE.BufferAttribute(mc.normalArray.slice(0, mc.count * 3), 3));
  geo.scale(HALF, HALF, HALF);
  geo.translate(0, CY, 0);
  skinAttributes(geo);
  // Marching cubes emits one unshared vertex per triangle corner — weld
  // exact-duplicate positions into an indexed mesh. Skin weights and
  // normals are both pure functions of position (skinAttributes above,
  // the field gradient in MarchingCubes), so coincident vertices are true
  // duplicates across every attribute, not just position; welding is a
  // strict size win with no visual or skinning change.
  geo = mergeVertices(geo);
  geo.computeBoundingSphere();
  geo.userData.shared = true;
  mc.geometry.dispose();
  BODY_GEO_CACHE.set(res, geo);
  return geo;
}

// Project a subdivided plane onto the body of revolution: each vertex lands
// on the lathe surface at its own height (plus a small outward offset), so
// the decal truly wraps the belly instead of hanging off it like a bib.
export function conformToBody(geo, centerY, offset) {
  const pos = geo.attributes.position;
  for (let i = 0; i < pos.count; i++) {
    const lx = pos.getX(i);
    const y = centerY + pos.getY(i);
    // Graph projection along z: the superellipse front is flat enough that
    // chord ≈ arc, so lateral position passes through unchanged.
    pos.setY(i, y);
    pos.setZ(i, crossDepth(lx, bodyRadiusAt(y)) + offset);
  }
  pos.needsUpdate = true;
  geo.computeVertexNormals();
  return geo;
}

// Face screen decal: dark rounded rect + ambient halo + inset top shadow.
// Plane is 0.56 × 0.38 GU; the rect is 0.52 × 0.34.
function faceTexture() {
  const W = 512, H = 348;
  const px = W / 0.56; // px per GU
  const c = document.createElement('canvas');
  c.width = W; c.height = H;
  const g = c.getContext('2d');
  const rw = 0.52 * px, rh = 0.34 * px, r = 0.13 * px;
  const x = (W - rw) / 2, y = (H - rh) / 2;
  // soft ambient halo so the screen reads as set into the body
  g.save();
  g.shadowColor = 'rgba(20,16,35,0.32)';
  g.shadowBlur = 16;
  g.fillStyle = '#221F33';
  g.beginPath(); g.roundRect(x, y, rw, rh, r); g.fill();
  g.restore();
  // inner shadow along the top edge (recessed glass)
  g.save();
  g.beginPath(); g.roundRect(x, y, rw, rh, r); g.clip();
  const grad = g.createLinearGradient(0, y, 0, y + rh * 0.45);
  grad.addColorStop(0, 'rgba(0,0,0,0.45)');
  grad.addColorStop(1, 'rgba(0,0,0,0)');
  g.fillStyle = grad;
  g.fillRect(x, y, rw, rh * 0.45);
  g.restore();
  const tex = new THREE.CanvasTexture(c);
  tex.colorSpace = THREE.SRGBColorSpace;
  tex.anisotropy = 4;
  return tex;
}

// Chest plate decal: deeper-shade rounded rect + glyph, printed-badge flat.
// Plane is 0.30 × 0.23 GU; the rect is 0.26 × 0.19.
function plateTexture(glyph, plateHex, tintHex) {
  const W = 512, H = 393;
  const px = W / 0.3;
  const c = document.createElement('canvas');
  c.width = W; c.height = H;
  const g = c.getContext('2d');
  const rw = 0.26 * px, rh = 0.19 * px, r = 0.05 * px;
  const x = (W - rw) / 2, y = (H - rh) / 2;
  g.save();
  g.shadowColor = 'rgba(20,16,35,0.25)';
  g.shadowBlur = 12;
  g.fillStyle = plateHex;
  g.beginPath(); g.roundRect(x, y, rw, rh, r); g.fill();
  g.restore();
  g.fillStyle = tintHex;
  g.font = '700 190px ui-monospace, "SF Mono", Menlo, monospace';
  g.textAlign = 'center';
  g.textBaseline = 'middle';
  g.fillText(glyph, W / 2, H / 2 + 12);
  const tex = new THREE.CanvasTexture(c);
  tex.colorSpace = THREE.SRGBColorSpace;
  tex.anisotropy = 4;
  return tex;
}

function shadowTexture() {
  const c = document.createElement('canvas');
  c.width = c.height = 128;
  const g = c.getContext('2d');
  const grad = g.createRadialGradient(64, 64, 8, 64, 64, 64);
  grad.addColorStop(0, 'rgba(34,31,51,0.42)');
  grad.addColorStop(1, 'rgba(34,31,51,0)');
  g.fillStyle = grad;
  g.fillRect(0, 0, 128, 128);
  return new THREE.CanvasTexture(c);
}
const SHADOW_TEX = { tex: null };

// ------------------------------------------------------------------- build

// Returns { root, squash, parts, skin } — root is placed in the world;
// squash is the scale/lean group (origin at the feet).
export function buildGooberModel({ color, glyph }) {
  const shades = deriveShades(color);
  const root = new THREE.Group();
  const squash = new THREE.Group();
  root.add(squash);

  const skin = []; // meshes recolored by the material variant switch
  const addSkin = (mesh, hex) => {
    mesh.userData.baseColor = hex;
    mesh.material = skinMaterial('matte', hex);
    skin.push(mesh);
    return mesh;
  };

  // Body + legs — one continuous skinned surface. The leg bones sit at the
  // old foot rest position (y=0.115), so the walk gait in goober.js drives
  // parts.footL/footR.position.y exactly as it did the old foot meshes,
  // now bending the legs out of the shared surface instead.
  const body = addSkin(new THREE.SkinnedMesh(bodyGeometry()), shades.base);
  const rootBone = new THREE.Bone();
  const footL = new THREE.Bone();
  footL.position.set(-0.15, 0.115, 0.02);
  const footR = new THREE.Bone();
  footR.position.set(0.15, 0.115, 0.02);
  rootBone.add(footL, footR);
  body.add(rootBone);
  body.updateMatrixWorld(true);
  body.bind(new THREE.Skeleton([rootBone, footL, footR]));
  squash.add(body);

  // Arms — mitten stubs, pivot at the shoulder
  const armGeo = new THREE.CapsuleGeometry(0.088, 0.17, 6, 14);
  const mkArm = (sx) => {
    const grp = new THREE.Group();
    grp.position.set(0.375 * sx, 0.54, 0.01);
    const m = addSkin(new THREE.Mesh(armGeo), shades.base);
    m.position.y = -0.14;
    grp.add(m);
    grp.rotation.z = 0.26 * sx; // rest splay, hangs slightly outward
    squash.add(grp);
    return grp;
  };
  const armL = mkArm(-1);
  const armR = mkArm(1);

  // Antenna — 2-part chain with a jiggle pivot at the crown
  const antenna = new THREE.Group();
  antenna.position.set(0, 0.985, 0);
  const stalk = new THREE.Mesh(
    new THREE.CylinderGeometry(0.014, 0.018, 0.14, 10),
    new THREE.MeshStandardMaterial({ color: '#3A3550', roughness: 0.5 })
  );
  stalk.position.y = 0.07;
  antenna.add(stalk);
  const ball = addSkin(new THREE.Mesh(new THREE.SphereGeometry(0.047, 18, 14)), shades.ball);
  ball.position.y = 0.16;
  antenna.add(ball);
  squash.add(antenna);

  // Face screen — a curved decal hugging the body (reference look: carved
  // in flush, soft inset shadow). The rect and shading live in the texture.
  // Conformed row-by-row to the real body surface — dome tuck and belly bow
  // come from the profile itself, so the decal is offset proud everywhere
  // and the body can never occlude it. Geometry is then re-based to the
  // group origin, which stays at the face apex as the gaze/anim pivot.
  const SCREEN_Y = 0.665, SCREEN_OFF = 0.006;
  const FACE_R = bodyRadiusAt(SCREEN_Y);
  const faceApex = crossDepth(0, FACE_R) + SCREEN_OFF;
  const screenGeo = conformToBody(new THREE.PlaneGeometry(0.56, 0.38, 36, 24), SCREEN_Y, SCREEN_OFF);
  screenGeo.translate(0, -SCREEN_Y, -faceApex);
  const screenGrp = new THREE.Group();
  screenGrp.position.set(0, SCREEN_Y, faceApex);
  const screen = new THREE.Mesh(
    screenGeo,
    new THREE.MeshBasicMaterial({
      map: faceTexture(), transparent: true,
      polygonOffset: true, polygonOffsetFactor: -2,
    })
  );
  screenGrp.add(screen);
  squash.add(screenGrp);

  // Eyes — arc (open/squint/flat/blink via scaleY) + ring (wide)
  const eyeMat = new THREE.MeshBasicMaterial({ color: '#FBFAF7', transparent: true });
  // z keeps the torus tubes half-sunk into the screen so the eyes read as
  // set into the panel rather than floating in front of the flat face
  const eyesAnchor = new THREE.Group();
  eyesAnchor.position.set(0, 0.028, 0.004);
  screenGrp.add(eyesAnchor);
  const mkEye = (sx) => {
    const grp = new THREE.Group();
    const ex = 0.118 * sx;
    // follow the screen's curvature so both eyes sit on its surface
    grp.position.set(ex, 0, crossDepth(ex, FACE_R) - crossDepth(0, FACE_R));
    const arc = new THREE.Mesh(new THREE.TorusGeometry(0.062, 0.023, 10, 24, Math.PI), eyeMat);
    const wide = new THREE.Mesh(new THREE.TorusGeometry(0.054, 0.021, 10, 28), eyeMat);
    wide.visible = false;
    grp.add(arc, wide);
    eyesAnchor.add(grp);
    return { grp, arc, wide };
  };
  const eyeL = mkEye(-1);
  const eyeR = mkEye(1);

  // Chest plate + role glyph
  // Chest plate — printed-badge decal, flat against the belly, rect and
  // glyph drawn together in the texture.
  const plateGrp = new THREE.Group();
  const plate = new THREE.Mesh(
    conformToBody(new THREE.PlaneGeometry(0.3, 0.23, 24, 20), 0.3, 0.006),
    new THREE.MeshStandardMaterial({
      map: plateTexture(glyph, shades.plate, shades.tint), transparent: true,
      roughness: 0.8, polygonOffset: true, polygonOffsetFactor: -2,
    })
  );
  plateGrp.add(plate);
  const glyphMesh = plate; // kept as a named part for future per-glyph swaps
  squash.add(plateGrp);

  // Contact shadow blob — sibling of squash so it stays on the floor
  if (!SHADOW_TEX.tex) SHADOW_TEX.tex = shadowTexture();
  const shadow = new THREE.Mesh(
    new THREE.PlaneGeometry(1, 1),
    new THREE.MeshBasicMaterial({ map: SHADOW_TEX.tex, transparent: true, depthWrite: false })
  );
  shadow.rotation.x = -Math.PI / 2;

  return {
    root, squash, shadow, skin,
    parts: { body, footL, footR, armL, armR, antenna, ball, screenGrp, eyesAnchor, eyeL, eyeR, plateGrp },
  };
}
