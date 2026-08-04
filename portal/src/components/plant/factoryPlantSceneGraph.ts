/**
 * Three.js scene graph for the WebGL Plant.
 *
 * The hall is retained for one mounted canvas and resized from dynamic layout
 * bounds. Bay decks, machines, and declared track segments are instanced by
 * mesh/material archetype; keyed entity anchors preserve semantic identity
 * while crates and posted goobers retain their one-shot motion objects.
 *
 * Art direction is a legible miniature industrial world. Matte architecture,
 * utility tanks, storage, gantries, windows, commons planting, and machine trim
 * make the hall feel inhabited without inventing work. Every stage kind still
 * gets a distinct silhouette so the hall reads in grayscale and colour carries
 * status on top of shape, never instead of it. There is no simulated activity:
 * a thing moves only because the daemon confirmed it moved.
 *
 * Geometry is shared through one cache per runtime. Two crates are the same
 * box, so the box is allocated once, kept alive while the runtime lives, and
 * disposed with it — never by an entity that happens to disappear first.
 */

import * as THREE from "three";
import type {
  FactoryPlantLayout,
  PlantInstanceBatch,
  PlantInstanceTransform,
} from "../../factoryPlantLayout";
import {
  desaturateHexColor,
  mixHexColor,
  plantScenePalette,
  readPlantTheme,
  type PlantScenePalette,
} from "../../plantPalette";
import {
  PLANT_RISK_CONTEXT_DESATURATION,
  PLANT_RISK_CONTEXT_OPACITY,
  type PlantRiskLevel,
} from "../../plantRisk";
import {
  plantPhase,
  type PlantEntitySpec,
  type PlantTone,
  type PlantWorldPoint,
} from "./factoryPlantEntities";

/** The scene palette, named for the runtime that consumes it. */
export type PlantPalette = PlantScenePalette;

export type PlantPaletteKey = keyof PlantScenePalette;

export interface PlantEntityObject {
  readonly object: THREE.Object3D;
  apply: (spec: PlantEntitySpec, palette: PlantPalette) => void;
  animate: (elapsed: number) => void;
  /** Offset from the confirmed position while a stage transfer plays. */
  setTransfer: (offset: PlantWorldPoint | undefined) => void;
  dispose: () => void;
}

export interface PlantStatics {
  applyLayout: (layout: FactoryPlantLayout) => void;
  applyPalette: (palette: PlantPalette) => void;
  dispose: () => void;
  readonly drawCalls: number;
}

export interface PlantInstanceScene {
  apply: (
    layout: FactoryPlantLayout,
    risk: boolean,
    palette: PlantPalette,
  ) => void;
  animate: (elapsed: number) => void;
  /** Applies one confirmed carrier-transfer offset to its instanced crate. */
  setTransfer: (id: string, offset: PlantWorldPoint | undefined) => void;
  dispose: () => void;
  readonly drawCalls: number;
  /** Confirmed-hazard beacons and unread markers currently drawn. */
  readonly markers: number;
}

export const PLANT_FALLBACK_PALETTE: PlantPalette = plantScenePalette("light");

const TONE_KEYS: Record<PlantTone, PlantPaletteKey> = {
  crate: "crate",
  crateBlocked: "crateBlocked",
  crateHeld: "crateHeld",
  crateUnknown: "crateUnknown",
  machineBody: "machineBody",
  machineBodyAlt: "machineBodyAlt",
  machineTrim: "machineTrim",
  statusBlocked: "statusBlocked",
  statusHeld: "statusHeld",
  statusIdle: "statusIdle",
  statusImpeded: "statusImpeded",
  statusRunning: "statusRunning",
  statusUnknown: "statusUnknown",
  structure: "structure",
  worker: "worker",
  workerIdle: "workerIdle",
};

/** Bounded counts, so the draw-call budget stays legible at a glance. */
const HALL_COLUMNS = 8;
const HALL_FIXTURES = 6;
const MAX_AISLES = 10;
const MAX_AISLE_MARKS = 60;
const MAX_GUARDRAILS = 48;
const MAX_CONSOLES = 24;
const MAX_PYLONS = 24;
const MAX_BAY_DETAILS = 24;
const MAX_CANOPY_PARTS = MAX_BAY_DETAILS * 4 + 8;
const MAX_STORAGE_RACKS = MAX_BAY_DETAILS * 3 + 12;
const MAX_UTILITY_TANKS = MAX_BAY_DETAILS * 2 + 6;
const MAX_PALLETS = MAX_BAY_DETAILS * 3 + 12;
const MAX_WINDOWS = 26;
const MAX_PIPES = 18;
const MAX_COMMONS_TREES = 20;
const MAX_LANDMARK_BODIES = 8;
const MAX_LANDMARK_ROOFS = 4;
const MAX_LANDMARK_STACKS = 6;
const MAX_BAY_HOUSES = MAX_BAY_DETAILS;

/**
 * Tracks every GPU-backed resource release so double disposal is a measurement
 * rather than a silent Three.js warning.
 */
export class PlantResourceLedger {
  private readonly released = new WeakSet<object>();
  private releases = 0;
  private doubles = 0;

  release(target: { dispose?: () => void } | undefined | null): void {
    if (!target) {
      return;
    }
    if (this.released.has(target)) {
      this.doubles += 1;
      return;
    }
    this.released.add(target);
    this.releases += 1;
    target.dispose?.();
  }

  has(target: object): boolean {
    return this.released.has(target);
  }

  get disposals(): number {
    return this.releases;
  }

  get doubleDisposals(): number {
    return this.doubles;
  }
}

/** One shared geometry per shape, owned by the runtime that created it. */
export class PlantGeometryCache {
  private readonly geometries = new Map<string, THREE.BufferGeometry>();

  constructor(private readonly ledger: PlantResourceLedger) {}

  get(key: string, factory: () => THREE.BufferGeometry): THREE.BufferGeometry {
    const existing = this.geometries.get(key);
    if (existing) {
      return existing;
    }
    const created = factory();
    this.geometries.set(key, created);
    return created;
  }

  get size(): number {
    return this.geometries.size;
  }

  dispose(): void {
    for (const geometry of this.geometries.values()) {
      this.ledger.release(geometry);
    }
    this.geometries.clear();
  }
}

/**
 * Resolves the authored scene palette for the current theme.
 *
 * Deliberately *not* a CSS custom-property read. Panel tokens describe chrome;
 * a hall needs concrete, painted decks, machine steel and real light. Reading
 * `--panel-raised` for the key light is exactly how a dark theme ends up as a
 * black scene lit by a black lamp.
 */
export function readPlantPalette(element?: HTMLElement): PlantPalette {
  return plantScenePalette(readPlantTheme(element));
}

export function plantToneColor(palette: PlantPalette, tone: PlantTone): string {
  return palette[TONE_KEYS[tone]] as string;
}

/* --------------------------------------------------------------------------
 * Static hall
 * ----------------------------------------------------------------------- */

interface ThemedMaterial {
  material: THREE.MeshStandardMaterial;
  color: PlantPaletteKey;
  emissive?: PlantPaletteKey;
  emissiveIntensity?: number;
}

export function createPlantStatics(
  world: THREE.Scene,
  ledger: PlantResourceLedger,
): PlantStatics {
  const group = new THREE.Group();
  group.name = "plant:statics";
  const geometries: THREE.BufferGeometry[] = [];
  const themed: ThemedMaterial[] = [];
  const shadows: THREE.LightShadow[] = [];
  const instanced: THREE.InstancedMesh[] = [];
  let disposed = false;

  const matte = (
    color: PlantPaletteKey,
    parameters: THREE.MeshStandardMaterialParameters = {},
    emissive?: PlantPaletteKey,
    emissiveIntensity?: number,
  ) => {
    const material = new THREE.MeshStandardMaterial({
      metalness: 0,
      roughness: 0.92,
      ...parameters,
    });
    themed.push({
      color,
      ...(emissive ? { emissive } : {}),
      ...(emissiveIntensity === undefined ? {} : { emissiveIntensity }),
      material,
    });
    return material;
  };
  const geometry = <TGeometry extends THREE.BufferGeometry>(created: TGeometry) => {
    geometries.push(created);
    return created;
  };
  const batch = (
    name: string,
    source: THREE.BufferGeometry,
    material: THREE.Material,
    count: number,
  ) => {
    const mesh = new THREE.InstancedMesh(source, material, count);
    mesh.name = name;
    mesh.count = 0;
    instanced.push(mesh);
    group.add(mesh);
    return mesh;
  };

  // Real lights. Their colours come from the light entries of the palette and
  // stay light in every theme; a surface token here is the bug this replaced.
  const fill = new THREE.HemisphereLight(0xffffff, 0x555555, 1);
  group.add(fill);
  const key = new THREE.DirectionalLight(0xffffff, 1);
  key.position.set(-12, 24, 12);
  key.castShadow = true;
  key.shadow.mapSize.set(1024, 1024);
  key.shadow.bias = -0.0015;
  shadows.push(key.shadow);
  group.add(key);
  const rim = new THREE.DirectionalLight(0xffffff, 1);
  rim.position.set(18, 10, -18);
  group.add(rim);

  const unitBox = geometry(new THREE.BoxGeometry(1, 1, 1));
  const unitCylinder = geometry(new THREE.CylinderGeometry(0.5, 0.5, 1, 12));
  const unitCrown = geometry(new THREE.IcosahedronGeometry(0.5, 1));
  const unitGable = geometry(new THREE.CylinderGeometry(0.58, 0.58, 1, 3));
  unitGable.rotateZ(Math.PI / 2);

  const floorMaterial = matte("floor", { roughness: 0.96 });
  const floor = new THREE.Mesh(unitBox, floorMaterial);
  floor.position.y = -0.3;
  floor.receiveShadow = true;
  group.add(floor);

  const grid = new THREE.GridHelper(1, 32);
  grid.position.y = -0.062;
  geometries.push(grid.geometry);
  group.add(grid);

  // Painted circulation aisles and their restrained kerb markings. These are
  // the cheapest legible separation between one workflow bay and the next.
  const aisleMaterial = matte("aisle", { roughness: 0.98 });
  const aisles = batch("plant:aisles", unitBox, aisleMaterial, MAX_AISLES);
  aisles.receiveShadow = true;
  const markingMaterial = matte("aisleMarking", { roughness: 0.85 });
  const aisleMarks = batch(
    "plant:aisle-markings",
    unitBox,
    markingMaterial,
    MAX_AISLE_MARKS,
  );

  const guardMaterial = matte("guardrail", { metalness: 0.15, roughness: 0.6 });
  const guardPosts = batch("plant:guard-posts", unitBox, guardMaterial, MAX_GUARDRAILS);
  guardPosts.castShadow = true;
  const guardRails = batch("plant:guard-rails", unitBox, guardMaterial, MAX_GUARDRAILS);

  const consoleMaterial = matte("console", { roughness: 0.7 });
  const consoles = batch("plant:consoles", unitBox, consoleMaterial, MAX_CONSOLES);
  consoles.castShadow = true;

  const pylonMaterial = matte("signPost", { roughness: 0.75 });
  const pylons = batch("plant:sign-pylons", unitBox, pylonMaterial, MAX_PYLONS);
  pylons.castShadow = true;

  const wallMaterial = matte("wall", { roughness: 0.95 });
  const backWall = new THREE.Mesh(unitBox, wallMaterial);
  backWall.receiveShadow = true;
  group.add(backWall);
  const sideWall = new THREE.Mesh(unitBox, wallMaterial);
  sideWall.receiveShadow = true;
  group.add(sideWall);
  const wallTrimMaterial = matte("wallTrim", { roughness: 0.8 });
  const wallTrim = new THREE.Mesh(unitBox, wallTrimMaterial);
  group.add(wallTrim);

  const structureMaterial = matte("structure", { metalness: 0.2, roughness: 0.62 });
  const columns = batch("plant:hall-columns", unitBox, structureMaterial, HALL_COLUMNS);
  columns.castShadow = true;

  const housingMaterial = matte("lightHousing", { roughness: 0.7 });
  const fixtures = batch("plant:light-housings", unitBox, housingMaterial, HALL_FIXTURES);
  const lensMaterial = matte(
    "lightEmissive",
    { roughness: 0.4 },
    "lightEmissive",
    1.15,
  );
  const lenses = batch("plant:light-lenses", unitBox, lensMaterial, HALL_FIXTURES);

  const gantryMaterial = matte("structureTrim", { metalness: 0.2, roughness: 0.55 });
  const crossbeam = new THREE.Mesh(unitBox, gantryMaterial);
  crossbeam.name = "plant:gantry-crossbeam";
  crossbeam.castShadow = true;
  group.add(crossbeam);
  const gantryLegs = batch("plant:gantry-legs", unitBox, gantryMaterial, 2);
  gantryLegs.castShadow = true;

  // The reference factory reads as a place because it has districts and
  // secondary industrial systems, not just primary machines. These bounded
  // instanced details are architectural context only: they never imply work.
  const windowMaterial = matte(
    "window",
    { metalness: 0.05, roughness: 0.28 },
    "window",
    0.08,
  );
  const windows = batch("plant:clerestory-windows", unitBox, windowMaterial, MAX_WINDOWS);
  const roofMaterial = matte("roof", { metalness: 0.18, roughness: 0.58 });
  const canopies = batch("plant:bay-canopies", unitBox, roofMaterial, MAX_CANOPY_PARTS);
  canopies.castShadow = true;
  const pipeMaterial = matte("pipe", { metalness: 0.35, roughness: 0.46 });
  const pipes = batch("plant:service-pipes", unitBox, pipeMaterial, MAX_PIPES);
  const utilityMaterial = matte("utility", { metalness: 0.18, roughness: 0.5 });
  const utilityTanks = batch(
    "plant:utility-tanks",
    unitCylinder,
    utilityMaterial,
    MAX_UTILITY_TANKS,
  );
  utilityTanks.castShadow = true;
  const storageMaterial = matte("storage", { roughness: 0.62 });
  const storageRacks = batch(
    "plant:storage-racks",
    unitBox,
    storageMaterial,
    MAX_STORAGE_RACKS,
  );
  storageRacks.castShadow = true;
  const palletMaterial = matte("pallet", { roughness: 0.82 });
  const pallets = batch("plant:material-pallets", unitBox, palletMaterial, MAX_PALLETS);
  pallets.castShadow = true;
  const commonsMaterial = matte("commons", { roughness: 0.9 });
  const treeTrunks = batch(
    "plant:commons-trunks",
    unitCylinder,
    palletMaterial,
    MAX_COMMONS_TREES,
  );
  const treeCrowns = batch(
    "plant:commons-crowns",
    unitCrown,
    commonsMaterial,
    MAX_COMMONS_TREES,
  );
  treeCrowns.castShadow = true;
  const planter = new THREE.Mesh(unitCylinder, commonsMaterial);
  planter.name = "plant:commons-planter";
  planter.receiveShadow = true;
  planter.visible = false;
  group.add(planter);
  const waterMaterial = matte(
    "water",
    { metalness: 0.05, roughness: 0.2 },
    "water",
    0.06,
  );
  const water = new THREE.Mesh(unitCylinder, waterMaterial);
  water.name = "plant:commons-water";
  water.visible = false;
  group.add(water);

  const landmarkBodyMaterial = matte("wall", { roughness: 0.88 });
  const landmarkBodies = batch(
    "plant:landmark-bodies",
    unitBox,
    landmarkBodyMaterial,
    MAX_LANDMARK_BODIES,
  );
  landmarkBodies.castShadow = true;
  landmarkBodies.receiveShadow = true;
  const landmarkRoofMaterial = matte("storage", {
    metalness: 0.08,
    roughness: 0.62,
  });
  const landmarkRoofs = batch(
    "plant:landmark-roofs",
    unitGable,
    landmarkRoofMaterial,
    MAX_LANDMARK_ROOFS,
  );
  landmarkRoofs.castShadow = true;
  const landmarkStacks = batch(
    "plant:landmark-stacks",
    unitCylinder,
    utilityMaterial,
    MAX_LANDMARK_STACKS,
  );
  landmarkStacks.castShadow = true;
  const bayHouses = batch(
    "plant:bay-service-houses",
    unitBox,
    landmarkBodyMaterial,
    MAX_BAY_HOUSES,
  );
  bayHouses.castShadow = true;
  bayHouses.receiveShadow = true;
  const bayHouseRoofs = batch(
    "plant:bay-service-roofs",
    unitGable,
    roofMaterial,
    MAX_BAY_HOUSES,
  );
  bayHouseRoofs.castShadow = true;

  world.add(group);
  const matrix = new THREE.Matrix4();
  const position = new THREE.Vector3();
  const quaternion = new THREE.Quaternion();
  const scale = new THREE.Vector3();

  const place = (
    mesh: THREE.InstancedMesh,
    index: number,
    x: number,
    y: number,
    z: number,
    width: number,
    height: number,
    depth: number,
  ) => {
    if (index >= mesh.instanceMatrix.count) {
      return index;
    }
    position.set(x, y, z);
    scale.set(width, height, depth);
    matrix.compose(position, quaternion, scale);
    mesh.setMatrixAt(index, matrix);
    return index + 1;
  };
  const updateInstances = (mesh: THREE.InstancedMesh, count: number) => {
    mesh.count = count;
    mesh.instanceMatrix.needsUpdate = true;
    mesh.boundingBox = null;
    mesh.boundingSphere = null;
  };

  return {
    applyLayout: (layout) => {
      if (disposed) {
        return;
      }
      const bounds = layout.hall.floor;
      const centerX = (bounds.minX + bounds.maxX) / 2;
      const centerZ = (bounds.minZ + bounds.maxZ) / 2;
      const width = Math.max(1, bounds.width);
      const depth = Math.max(1, bounds.depth);
      const wallHeight = layout.hall.wallHeight;

      floor.position.set(centerX, -0.3, centerZ);
      floor.scale.set(width, 0.45, depth);
      grid.position.set(centerX, -0.062, centerZ);
      grid.scale.set(width, 1, depth);
      backWall.position.set(centerX, wallHeight / 2 - 0.25, bounds.minZ - 0.18);
      backWall.scale.set(width, wallHeight, 0.35);
      sideWall.position.set(bounds.minX - 0.18, wallHeight / 2 - 0.25, centerZ);
      sideWall.scale.set(0.35, wallHeight, depth);
      wallTrim.position.set(centerX, 0.42, bounds.minZ - 0.16);
      wallTrim.scale.set(width, 0.22, 0.4);

      // Aisles run through the gaps the bay allocator already left between
      // rows, so circulation space is derived from the layout rather than
      // decorated onto it.
      const rows = [...layout.bays]
        .map((bay) => bay.bounds)
        .sort((a, b) => a.minZ - b.minZ);
      let aisleIndex = 0;
      let markIndex = 0;
      let previousMaxZ: number | undefined;
      for (const rect of rows) {
        if (previousMaxZ !== undefined && rect.minZ - previousMaxZ > 0.6) {
          const gapCenter = (previousMaxZ + rect.minZ) / 2;
          const gapDepth = Math.min(2.4, rect.minZ - previousMaxZ - 0.15);
          aisleIndex = place(
            aisles,
            aisleIndex,
            centerX,
            -0.055,
            gapCenter,
            width - 0.6,
            0.06,
            gapDepth,
          );
          const marks = Math.min(14, Math.max(2, Math.floor(width / 2.4)));
          for (let mark = 0; mark < marks; mark += 1) {
            markIndex = place(
              aisleMarks,
              markIndex,
              bounds.minX + ((mark + 0.5) / marks) * width,
              -0.045,
              gapCenter,
              0.9,
              0.05,
              0.14,
            );
          }
        }
        previousMaxZ = Math.max(previousMaxZ ?? rect.maxZ, rect.maxZ);
      }
      // Perimeter aisle: one band along the front of the hall.
      aisleIndex = place(
        aisles,
        aisleIndex,
        centerX,
        -0.055,
        bounds.maxZ - 0.9,
        width - 0.6,
        0.06,
        1.4,
      );
      updateInstances(aisles, aisleIndex);
      updateInstances(aisleMarks, markIndex);

      // Guardrails, consoles and identification pylons per bay. Bounded counts
      // keep a large floor inside the draw-call budget.
      let postIndex = 0;
      let railIndex = 0;
      let consoleIndex = 0;
      let pylonIndex = 0;
      for (const bay of layout.bays) {
        const rect = bay.bounds;
        const railZ = rect.maxZ + 0.12;
        railIndex = place(
          guardRails,
          railIndex,
          (rect.minX + rect.maxX) / 2,
          0.44,
          railZ,
          rect.width,
          0.06,
          0.06,
        );
        const posts = Math.min(4, Math.max(2, Math.round(rect.width / 4)));
        for (let post = 0; post < posts; post += 1) {
          postIndex = place(
            guardPosts,
            postIndex,
            rect.minX + ((post + 0.5) / posts) * rect.width,
            0.24,
            railZ,
            0.08,
            0.48,
            0.08,
          );
        }
        consoleIndex = place(
          consoles,
          consoleIndex,
          rect.minX + 0.45,
          0.28,
          rect.maxZ - 0.5,
          0.6,
          0.56,
          0.34,
        );
        pylonIndex = place(
          pylons,
          pylonIndex,
          rect.minX + 0.2,
          0.8,
          rect.minZ + 0.2,
          0.12,
          1.6,
          0.12,
        );
      }
      updateInstances(guardRails, railIndex);
      updateInstances(guardPosts, postIndex);
      updateInstances(consoles, consoleIndex);
      updateInstances(pylons, pylonIndex);

      let columnIndex = 0;
      for (let index = 0; index < HALL_COLUMNS; index += 1) {
        columnIndex = place(
          columns,
          columnIndex,
          bounds.minX + ((index + 0.5) / HALL_COLUMNS) * width,
          wallHeight / 2 - 0.25,
          bounds.minZ + 0.2,
          0.18,
          wallHeight,
          0.18,
        );
      }
      updateInstances(columns, columnIndex);

      let fixtureIndex = 0;
      let lensIndex = 0;
      const fixtureWidth = Math.max(1.8, width / HALL_FIXTURES / 2);
      const fixtureZ = bounds.minZ + Math.min(2.2, depth * 0.15);
      for (let index = 0; index < HALL_FIXTURES; index += 1) {
        const x = bounds.minX + ((index + 0.5) / HALL_FIXTURES) * width;
        fixtureIndex = place(
          fixtures,
          fixtureIndex,
          x,
          wallHeight - 0.66,
          fixtureZ,
          fixtureWidth,
          0.12,
          0.3,
        );
        lensIndex = place(
          lenses,
          lensIndex,
          x,
          wallHeight - 0.75,
          fixtureZ,
          fixtureWidth * 0.86,
          0.05,
          0.2,
        );
      }
      updateInstances(fixtures, fixtureIndex);
      updateInstances(lenses, lensIndex);

      const gantryWidth = Math.max(5, Math.min(12, width * 0.32));
      const gantryZ = centerZ - Math.min(2, depth * 0.08);
      crossbeam.position.set(centerX, Math.min(3.65, wallHeight - 0.8), gantryZ);
      crossbeam.scale.set(gantryWidth, 0.24, 0.3);
      let legIndex = 0;
      for (let index = 0; index < 2; index += 1) {
        legIndex = place(
          gantryLegs,
          legIndex,
          centerX + (index === 0 ? -1 : 1) * (gantryWidth / 2 - 0.25),
          1.7,
          gantryZ,
          0.26,
          3.7,
          0.26,
        );
      }
      updateInstances(gantryLegs, legIndex);

      let windowIndex = 0;
      const backWindowCount = Math.min(12, Math.max(4, Math.floor(width / 3.2)));
      for (let index = 0; index < backWindowCount; index += 1) {
        windowIndex = place(
          windows,
          windowIndex,
          bounds.minX + ((index + 0.5) / backWindowCount) * width,
          Math.max(1.8, wallHeight * 0.62),
          bounds.minZ + 0.015,
          Math.max(1.2, width / backWindowCount - 0.55),
          0.72,
          0.035,
        );
      }
      const sideWindowCount = Math.min(
        MAX_WINDOWS - windowIndex,
        Math.max(3, Math.floor(depth / 3.6)),
      );
      for (let index = 0; index < sideWindowCount; index += 1) {
        windowIndex = place(
          windows,
          windowIndex,
          bounds.minX + 0.015,
          Math.max(1.8, wallHeight * 0.62),
          bounds.minZ + ((index + 0.5) / sideWindowCount) * depth,
          0.035,
          0.72,
          Math.max(1.2, depth / sideWindowCount - 0.55),
        );
      }
      updateInstances(windows, windowIndex);

      let pipeIndex = 0;
      for (let index = 0; index < 3; index += 1) {
        pipeIndex = place(
          pipes,
          pipeIndex,
          centerX,
          1.15 + index * 0.28,
          bounds.minZ + 0.32 + index * 0.12,
          width - 1.1,
          0.08,
          0.08,
        );
      }

      let canopyIndex = 0;
      let rackIndex = 0;
      let tankIndex = 0;
      let palletIndex = 0;
      let bayHouseIndex = 0;
      let bayHouseRoofIndex = 0;
      for (const bay of layout.bays.slice(0, MAX_BAY_DETAILS)) {
        const rect = bay.bounds;
        const canopyZ = rect.minZ + Math.min(0.7, rect.depth * 0.2);
        const canopyHeight = Math.min(2.45, wallHeight - 0.55);
        canopyIndex = place(
          canopies,
          canopyIndex,
          rect.minX + 0.32,
          canopyHeight / 2,
          canopyZ,
          0.1,
          canopyHeight,
          0.1,
        );
        canopyIndex = place(
          canopies,
          canopyIndex,
          rect.maxX - 0.32,
          canopyHeight / 2,
          canopyZ,
          0.1,
          canopyHeight,
          0.1,
        );
        canopyIndex = place(
          canopies,
          canopyIndex,
          (rect.minX + rect.maxX) / 2,
          canopyHeight,
          canopyZ,
          Math.max(0.6, rect.width - 0.55),
          0.12,
          0.18,
        );

        const serviceZ = rect.maxZ - Math.min(0.62, rect.depth * 0.18);
        const houseWidth = Math.min(2.2, Math.max(1.45, rect.width * 0.34));
        const houseDepth = Math.min(1.55, Math.max(1.1, rect.depth * 0.28));
        const houseX = rect.maxX - houseWidth / 2 - 0.28;
        bayHouseIndex = place(
          bayHouses,
          bayHouseIndex,
          houseX,
          0.62,
          serviceZ,
          houseWidth,
          1.24,
          houseDepth,
        );
        bayHouseRoofIndex = place(
          bayHouseRoofs,
          bayHouseRoofIndex,
          houseX,
          1.39,
          serviceZ,
          houseWidth * 1.08,
          houseWidth * 0.58,
          houseDepth * 0.74,
        );
        canopyIndex = place(
          canopies,
          canopyIndex,
          rect.minX + 1.15,
          1.5,
          serviceZ,
          1.8,
          0.12,
          1.35,
        );
        rackIndex = place(
          storageRacks,
          rackIndex,
          rect.minX + 1.25,
          0.42,
          serviceZ,
          0.68,
          0.84,
          1.2,
        );
        rackIndex = place(
          storageRacks,
          rackIndex,
          rect.minX + 1.25,
          0.86,
          serviceZ,
          0.76,
          0.08,
          1.28,
        );
        rackIndex = place(
          storageRacks,
          rackIndex,
          rect.maxX - 0.9,
          0.72,
          serviceZ,
          1.35,
          1.35,
          0.72,
        );
        tankIndex = place(
          utilityTanks,
          tankIndex,
          rect.minX + 0.5,
          0.58,
          serviceZ,
          0.58,
          1.16,
          0.58,
        );
        if (rect.width > 5.4) {
          tankIndex = place(
            utilityTanks,
            tankIndex,
            rect.minX + 1.15,
            0.48,
            serviceZ,
            0.46,
            0.96,
            0.46,
          );
        }
        for (let pallet = 0; pallet < 3; pallet += 1) {
          palletIndex = place(
            pallets,
            palletIndex,
            rect.minX + 1.85 + pallet * 0.62,
            0.1,
            serviceZ,
            0.48,
            0.2 + (pallet % 2) * 0.12,
            0.56,
          );
        }
        pipeIndex = place(
          pipes,
          pipeIndex,
          (rect.minX + rect.maxX) / 2,
          canopyHeight - 0.2,
          rect.maxZ - 0.18,
          Math.max(0.8, rect.width - 0.8),
          0.07,
          0.07,
        );
      }

      // Two authored landmarks stop a sparse live model from reading as an
      // empty diagram: a utility silo bank at the rear and a material yard at
      // the loading edge. They are static architecture, never fake activity.
      const landmarkScale = Math.max(0.8, Math.min(1.35, width / 18));
      for (let silo = 0; silo < 3; silo += 1) {
        tankIndex = place(
          utilityTanks,
          tankIndex,
          bounds.minX + 0.8 + silo * 0.82 * landmarkScale,
          1.05 + (silo % 2) * 0.18,
          bounds.minZ + 0.9,
          0.72 * landmarkScale,
          (2.1 + (silo % 2) * 0.35) * landmarkScale,
          0.72 * landmarkScale,
        );
      }
      const yardStartX = Math.max(bounds.minX + 1.4, bounds.maxX - 5.2);
      const yardZ = bounds.maxZ - 2.2;
      for (let container = 0; container < 4; container += 1) {
        const upper = container >= 2;
        rackIndex = place(
          storageRacks,
          rackIndex,
          yardStartX + (container % 2) * 1.65,
          upper ? 0.72 : 0.34,
          yardZ - (upper ? 0.05 : 0),
          1.45,
          0.58,
          0.72,
        );
        palletIndex = place(
          pallets,
          palletIndex,
          yardStartX + (container % 2) * 1.65,
          upper ? 0.39 : 0.04,
          yardZ,
          1.58,
          0.12,
          0.84,
        );
      }
      updateInstances(canopies, canopyIndex);
      updateInstances(bayHouses, bayHouseIndex);
      updateInstances(bayHouseRoofs, bayHouseRoofIndex);
      updateInstances(storageRacks, rackIndex);
      updateInstances(utilityTanks, tankIndex);
      updateInstances(pallets, palletIndex);
      updateInstances(pipes, pipeIndex);

      let trunkIndex = 0;
      let crownIndex = 0;
      const commons = layout.hall.commons;
      planter.visible = Boolean(commons);
      water.visible = Boolean(commons);
      if (commons) {
        const commonsCenterX = (commons.minX + commons.maxX) / 2;
        const commonsCenterZ = (commons.minZ + commons.maxZ) / 2;
        planter.position.set(commonsCenterX, 0.04, commonsCenterZ);
        planter.scale.set(
          Math.max(1.4, commons.width * 0.72),
          0.08,
          Math.max(1.4, commons.depth * 0.72),
        );
        water.position.set(
          commons.minX + commons.width * 0.72,
          0.11,
          commons.minZ + commons.depth * 0.55,
        );
        water.scale.set(
          Math.max(0.7, commons.width * 0.16),
          0.04,
          Math.max(0.7, commons.depth * 0.16),
        );
        const waterRadiusX = water.scale.x / 2;
        const waterRadiusZ = water.scale.z / 2;
        const treeCount = Math.min(
          MAX_COMMONS_TREES,
          Math.max(4, Math.floor((commons.width + commons.depth) / 2.4)),
        );
        for (let index = 0; index < treeCount; index += 1) {
          const side = index % 4;
          const progress = (Math.floor(index / 4) + 1) / (Math.ceil(treeCount / 4) + 1);
          const x =
            side === 0
              ? commons.minX + 0.65
              : side === 1
                ? commons.maxX - 0.65
                : commons.minX + progress * commons.width;
          const z =
            side === 2
              ? commons.minZ + 0.65
              : side === 3
                ? commons.maxZ - 0.65
                : commons.minZ + progress * commons.depth;
          if (
            Math.abs(x - water.position.x) <= waterRadiusX + 0.5 &&
            Math.abs(z - water.position.z) <= waterRadiusZ + 0.5
          ) {
            continue;
          }
          trunkIndex = place(
            treeTrunks,
            trunkIndex,
            x,
            0.38,
            z,
            0.16,
            0.76,
            0.16,
          );
          crownIndex = place(
            treeCrowns,
            crownIndex,
            x,
            0.96,
            z,
            0.82,
            0.95,
            0.82,
          );
        }
      }

      // A few perimeter buildings give the hall a memorable skyline even
      // when live data contains only one or two workflows. They remain outside
      // the bay interiors and do not claim any stage, owner, or activity.
      let landmarkBodyIndex = 0;
      let landmarkRoofIndex = 0;
      let landmarkStackIndex = 0;
      const buildingScale = Math.max(0.82, Math.min(1.18, width / 20));
      const placeGabledBuilding = (
        x: number,
        z: number,
        buildingWidth: number,
        buildingDepth: number,
      ) => {
        landmarkBodyIndex = place(
          landmarkBodies,
          landmarkBodyIndex,
          x,
          0.72 * buildingScale,
          z,
          buildingWidth,
          1.44 * buildingScale,
          buildingDepth,
        );
        landmarkRoofIndex = place(
          landmarkRoofs,
          landmarkRoofIndex,
          x,
          1.62 * buildingScale,
          z,
          buildingWidth * 1.08,
          buildingWidth * 0.62,
          buildingDepth * 0.72,
        );
      };
      placeGabledBuilding(
        bounds.minX + 2.1,
        bounds.maxZ - 2.25,
        2.7 * buildingScale,
        1.55 * buildingScale,
      );
      placeGabledBuilding(
        bounds.maxX - 2.2,
        bounds.minZ + 1.15,
        2.5 * buildingScale,
        1.5 * buildingScale,
      );
      landmarkBodyIndex = place(
        landmarkBodies,
        landmarkBodyIndex,
        bounds.maxX - 0.72,
        1.45 * buildingScale,
        bounds.minZ + 2.7,
        1.1 * buildingScale,
        2.9 * buildingScale,
        1.1 * buildingScale,
      );
      landmarkRoofIndex = place(
        landmarkRoofs,
        landmarkRoofIndex,
        bounds.maxX - 0.72,
        3.08 * buildingScale,
        bounds.minZ + 2.7,
        1.24 * buildingScale,
        0.85 * buildingScale,
        0.86 * buildingScale,
      );
      for (let stack = 0; stack < 2; stack += 1) {
        landmarkStackIndex = place(
          landmarkStacks,
          landmarkStackIndex,
          bounds.minX + 0.78 + stack * 0.62,
          1.45 * buildingScale,
          bounds.maxZ - 2.5,
          0.32 * buildingScale,
          (2.9 + stack * 0.45) * buildingScale,
          0.32 * buildingScale,
        );
      }
      updateInstances(landmarkBodies, landmarkBodyIndex);
      updateInstances(landmarkRoofs, landmarkRoofIndex);
      updateInstances(landmarkStacks, landmarkStackIndex);

      if (!commons) {
        const treeCenterX = centerX - Math.min(0.8, width * 0.04);
        const treeSites = [
          [treeCenterX - 1.15, bounds.maxZ - 2.15],
          [treeCenterX, bounds.maxZ - 2.2],
          [treeCenterX + 1.15, bounds.maxZ - 2.15],
        ] as const;
        for (const [x, z] of treeSites) {
          trunkIndex = place(treeTrunks, trunkIndex, x, 0.35, z, 0.14, 0.7, 0.14);
          crownIndex = place(treeCrowns, crownIndex, x, 0.9, z, 0.76, 0.88, 0.76);
        }
      }
      updateInstances(treeTrunks, trunkIndex);
      updateInstances(treeCrowns, crownIndex);

      const shadowRadius = Math.max(width, depth) * 0.75;
      key.shadow.camera.left = -shadowRadius;
      key.shadow.camera.right = shadowRadius;
      key.shadow.camera.top = shadowRadius;
      key.shadow.camera.bottom = -shadowRadius;
      key.shadow.camera.far = Math.max(60, shadowRadius * 4);
      key.shadow.camera.updateProjectionMatrix();
      key.position.set(
        bounds.minX - Math.min(12, width * 0.2),
        Math.max(24, wallHeight * 5),
        bounds.maxZ + Math.min(12, depth * 0.2),
      );
    },
    applyPalette: (palette) => {
      if (disposed) {
        return;
      }
      if (world.background instanceof THREE.Color) {
        world.background.set(palette.background);
      } else {
        world.background = new THREE.Color(palette.background);
      }
      fill.color.set(palette.fillSky);
      fill.groundColor.set(palette.fillGround);
      fill.intensity = palette.fillIntensity;
      key.color.set(palette.keyLight);
      key.intensity = palette.keyIntensity;
      rim.color.set(palette.rimLight);
      rim.intensity = palette.rimIntensity;
      for (const entry of themed) {
        entry.material.color.set(palette[entry.color] as string);
        if (entry.emissive) {
          entry.material.emissive.set(palette[entry.emissive] as string);
          entry.material.emissiveIntensity = entry.emissiveIntensity ?? 1;
        }
      }
      applyGridPalette(grid, palette.floorGridStrong, palette.floorGrid);
    },
    dispose: () => {
      if (disposed) {
        return;
      }
      disposed = true;
      world.remove(group);
      group.clear();
      for (const shadow of shadows) {
        // renderer.dispose() releases programs and buffers but never a light's
        // shadow render target; leaving it attached leaks a depth map per
        // mounted canvas.
        ledger.release(shadow);
      }
      for (const mesh of instanced) {
        ledger.release(mesh);
      }
      for (const entry of themed) {
        ledger.release(entry.material);
      }
      ledger.release(grid.material as THREE.Material);
      for (const item of geometries) {
        ledger.release(item);
      }
      geometries.length = 0;
      themed.length = 0;
      instanced.length = 0;
    },
    get drawCalls() {
      // Lights and the group itself never draw; meshes and the grid helper do.
      return 8 + instanced.length;
    },
  };
}

/* --------------------------------------------------------------------------
 * Instanced hall content
 * ----------------------------------------------------------------------- */

interface BatchRecord {
  batch: PlantInstanceBatch;
  mesh: THREE.InstancedMesh;
  material: THREE.MeshStandardMaterial;
  baseEmissive: number;
  trim?: {
    mesh: THREE.InstancedMesh;
    material: THREE.MeshStandardMaterial;
  };
  /** Confirmed-hazard beacon or unread marker riding on the same transforms. */
  marker?: {
    mesh: THREE.InstancedMesh;
    material: THREE.Material;
    level: PlantRiskLevel;
  };
  ring?: {
    mesh: THREE.InstancedMesh;
    material: THREE.MeshStandardMaterial;
  };
}

export function createPlantInstanceScene(
  world: THREE.Scene,
  cache: PlantGeometryCache,
  ledger: PlantResourceLedger,
): PlantInstanceScene {
  const group = new THREE.Group();
  group.name = "plant:instance-batches";
  world.add(group);
  const records = new Map<string, BatchRecord>();
  const transfers = new Map<string, PlantWorldPoint>();
  let disposed = false;
  let markerCount = 0;
  let elapsedSeconds = 0;

  const remove = (key: string, record: BatchRecord) => {
    group.remove(record.mesh);
    ledger.release(record.mesh);
    ledger.release(record.material);
    if (record.trim) {
      group.remove(record.trim.mesh);
      ledger.release(record.trim.mesh);
      ledger.release(record.trim.material);
    }
    if (record.marker) {
      group.remove(record.marker.mesh);
      ledger.release(record.marker.mesh);
      ledger.release(record.marker.material);
    }
    if (record.ring) {
      group.remove(record.ring.mesh);
      ledger.release(record.ring.mesh);
      ledger.release(record.ring.material);
    }
    records.delete(key);
  };

  const create = (
    batch: PlantInstanceBatch,
    risk: boolean,
    palette: PlantPalette,
  ): BatchRecord => {
    const surface = batchSurface(batch.meshArchetype);
    const material = new THREE.MeshStandardMaterial({
      metalness: surface.metalness,
      roughness: surface.roughness,
    });
    const mesh = new THREE.InstancedMesh(
      instanceGeometry(batch.meshArchetype, cache),
      material,
      batch.instances.length,
    );
    mesh.name = `batch:${batch.key}`;
    mesh.castShadow = surface.castShadow;
    mesh.receiveShadow = surface.receiveShadow;
    const record: BatchRecord = { baseEmissive: 0, batch, material, mesh };
    applyInstanceIdentity(record);
    group.add(mesh);
    if (batch.meshArchetype.startsWith("machine")) {
      const trimMaterial = new THREE.MeshStandardMaterial({
        metalness: 0.18,
        roughness: 0.48,
      });
      const trimMesh = new THREE.InstancedMesh(
        machineTrimGeometry(batch.meshArchetype, cache),
        trimMaterial,
        batch.instances.length,
      );
      trimMesh.name = `trim:${batch.key}`;
      trimMesh.castShadow = true;
      group.add(trimMesh);
      record.trim = { material: trimMaterial, mesh: trimMesh };
    }

    const level = batchRiskLevel(batch.materialArchetype);
    if (
      level &&
      (batch.meshArchetype.startsWith("machine") ||
        batch.meshArchetype === "carrier")
    ) {
      // A beacon for a confirmed hazard, an open marker for an unread signal.
      // Both are shapes first, so they survive grayscale and a screenshot.
      const markerMaterial =
        level === "unknown"
          ? new THREE.MeshBasicMaterial({ wireframe: true })
          : new THREE.MeshStandardMaterial({ metalness: 0, roughness: 0.45 });
      const markerMesh = new THREE.InstancedMesh(
        markerGeometry(level, cache),
        markerMaterial,
        batch.instances.length,
      );
      markerMesh.name = `marker:${batch.key}`;
      group.add(markerMesh);
      record.marker = { level, material: markerMaterial, mesh: markerMesh };

      if (level !== "unknown") {
        const ringMaterial = new THREE.MeshStandardMaterial({
          metalness: 0,
          roughness: 0.9,
        });
        const ringMesh = new THREE.InstancedMesh(
          cache.get(
            "marker:ring",
            () => new THREE.TorusGeometry(0.9, 0.06, 6, 24),
          ),
          ringMaterial,
          batch.instances.length,
        );
        ringMesh.name = `ring:${batch.key}`;
        group.add(ringMesh);
        record.ring = { material: ringMaterial, mesh: ringMesh };
      }
    }

    applyBatchRecord(record, risk, palette);
    return record;
  };

  return {
    apply: (layout, risk, palette) => {
      if (disposed) {
        return;
      }
      const live = new Set<string>();
      for (const batch of layout.instanceBatches.filter(renderedAsInstanceBatch)) {
        live.add(batch.key);
        const current = records.get(batch.key);
        if (
          current &&
          current.batch.meshArchetype === batch.meshArchetype &&
          current.mesh.count === batch.instances.length
        ) {
          current.batch = batch;
          applyInstanceIdentity(current);
          applyBatchRecord(current, risk, palette);
          applyBatchMatrices(current, elapsedSeconds, transfers);
          continue;
        }
        if (current) {
          remove(batch.key, current);
        }
        const created = create(batch, risk, palette);
        applyBatchMatrices(created, elapsedSeconds, transfers);
        records.set(batch.key, created);
      }
      for (const [key, record] of [...records]) {
        if (!live.has(key)) {
          remove(key, record);
        }
      }
      markerCount = [...records.values()].reduce(
        (total, record) => total + (record.marker ? record.marker.mesh.count : 0),
        0,
      );
    },
    animate: (elapsed) => {
      if (disposed) {
        return;
      }
      elapsedSeconds = elapsed;
      for (const record of records.values()) {
        record.material.emissiveIntensity = record.batch.active
          ? record.baseEmissive + (Math.sin(elapsed * 3.1) + 1) * 0.05
          : record.baseEmissive;
        if (
          record.batch.meshArchetype === "carrier" ||
          record.batch.meshArchetype === "worker"
        ) {
          applyBatchMatrices(record, elapsed, transfers);
        }
      }
    },
    setTransfer: (id, offset) => {
      if (offset) {
        transfers.set(id, offset);
      } else {
        transfers.delete(id);
      }
      for (const record of records.values()) {
        if (
          record.batch.meshArchetype === "carrier" &&
          record.batch.instances.some((instance) => instance.id === id)
        ) {
          applyBatchMatrices(record, elapsedSeconds, transfers);
        }
      }
    },
    dispose: () => {
      if (disposed) {
        return;
      }
      disposed = true;
      for (const [key, record] of [...records]) {
        remove(key, record);
      }
      world.remove(group);
      group.clear();
      markerCount = 0;
      transfers.clear();
    },
    get drawCalls() {
      return [...records.values()].reduce(
        (total, record) =>
          total +
          1 +
          (record.trim ? 1 : 0) +
          (record.marker ? 1 : 0) +
          (record.ring ? 1 : 0),
        0,
      );
    },
    get markers() {
      return markerCount;
    },
  };
}

function renderedAsInstanceBatch(_batch: PlantInstanceBatch): boolean {
  return true;
}

interface BatchSurface {
  metalness: number;
  roughness: number;
  castShadow: boolean;
  receiveShadow: boolean;
}

function batchSurface(archetype: string): BatchSurface {
  if (archetype.startsWith("machine")) {
    return { castShadow: true, metalness: 0.12, receiveShadow: true, roughness: 0.68 };
  }
  if (archetype.startsWith("track")) {
    return { castShadow: false, metalness: 0.25, receiveShadow: true, roughness: 0.55 };
  }
  if (archetype === "dock") {
    return { castShadow: true, metalness: 0.1, receiveShadow: true, roughness: 0.7 };
  }
  return { castShadow: false, metalness: 0, receiveShadow: true, roughness: 0.95 };
}

/**
 * One silhouette per stage kind.
 *
 * The shapes are chosen to be told apart at a glance and in grayscale: an
 * agentic cell is a hexagonal silo with a hopper, a gate is an arch you pass
 * through, a deterministic stage is a rectangular press, a declared parallel
 * stage is a twin press, and a stage of unknown kind is an open solid.
 */
function instanceGeometry(
  archetype: string,
  cache: PlantGeometryCache,
): THREE.BufferGeometry {
  switch (archetype) {
    case "machine:agentic":
      return cache.get("instance:machine:agentic", () =>
        mergePlantGeometries([
          new THREE.CylinderGeometry(0.42, 0.5, 1, 6),
          translated(new THREE.CylinderGeometry(0.26, 0.4, 0.28, 6), 0, 0.58, 0),
        ]),
      );
    case "machine:gate":
      return cache.get("instance:machine:gate", () =>
        mergePlantGeometries([
          translated(new THREE.BoxGeometry(0.2, 1, 0.24), -0.36, 0, 0),
          translated(new THREE.BoxGeometry(0.2, 1, 0.24), 0.36, 0, 0),
          translated(new THREE.BoxGeometry(0.92, 0.22, 0.28), 0, 0.55, 0),
          translated(new THREE.BoxGeometry(0.5, 0.7, 0.08), 0, -0.08, 0.08),
        ]),
      );
    case "machine:evaluator":
      return cache.get("instance:machine:evaluator", () =>
        mergePlantGeometries([
          new THREE.CylinderGeometry(0.34, 0.34, 0.7, 8),
          translated(new THREE.BoxGeometry(1.02, 0.12, 0.24), 0, 0.42, 0),
          translated(new THREE.BoxGeometry(0.2, 0.24, 0.2), -0.42, 0.6, 0),
          translated(new THREE.BoxGeometry(0.2, 0.24, 0.2), 0.42, 0.6, 0),
        ]),
      );
    case "machine:parallel":
      return cache.get("instance:machine:parallel", () =>
        mergePlantGeometries([
          translated(new THREE.BoxGeometry(0.4, 1, 0.72), -0.26, 0, 0),
          translated(new THREE.BoxGeometry(0.4, 0.78, 0.72), 0.26, -0.11, 0),
        ]),
      );
    case "machine:deterministic":
      return cache.get("instance:machine:deterministic", () =>
        mergePlantGeometries([
          new THREE.BoxGeometry(0.92, 1, 0.78),
          translated(new THREE.BoxGeometry(0.62, 0.18, 0.52), 0, 0.56, 0),
        ]),
      );
    case "dock":
      return cache.get(
        "instance:dock",
        () => new THREE.CylinderGeometry(0.12, 0.55, 1, 4),
      );
    case "carrier":
      return cache.get(
        "instance:carrier",
        () => new THREE.BoxGeometry(1, 1, 1),
      );
    case "worker":
      return cache.get("instance:worker", () =>
        mergePlantGeometries([
          translated(new THREE.CapsuleGeometry(0.34, 0.45, 4, 8), 0, 0.22, 0),
          translated(new THREE.SphereGeometry(0.32, 10, 7), 0, 0.9, 0),
        ]),
      );
    default:
      break;
  }
  if (archetype.startsWith("machine:")) {
    return cache.get(
      "instance:machine:unknown",
      () => new THREE.OctahedronGeometry(0.62, 0),
    );
  }
  return cache.get("instance:box", () => new THREE.BoxGeometry(1, 1, 1));
}

function markerGeometry(
  level: PlantRiskLevel,
  cache: PlantGeometryCache,
): THREE.BufferGeometry {
  switch (level) {
    case "blocked":
      return cache.get("marker:blocked", () =>
        mergePlantGeometries([
          translated(new THREE.BoxGeometry(0.08, 0.42, 0.08), 0, 0.2, 0),
          translated(new THREE.CylinderGeometry(0.3, 0.3, 0.12, 8), 0, 0.54, 0),
        ]),
      );
    case "held":
      return cache.get("marker:held", () =>
        mergePlantGeometries([
          translated(new THREE.BoxGeometry(0.1, 0.44, 0.1), -0.13, 0.3, 0),
          translated(new THREE.BoxGeometry(0.1, 0.44, 0.1), 0.13, 0.3, 0),
        ]),
      );
    case "impeded":
      return cache.get("marker:impeded", () =>
        mergePlantGeometries([
          translated(new THREE.BoxGeometry(0.08, 0.34, 0.08), 0, 0.17, 0),
          translated(new THREE.ConeGeometry(0.34, 0.18, 3), 0, 0.48, 0),
        ]),
      );
    case "unknown":
      return cache.get(
        "marker:unknown",
        () => new THREE.OctahedronGeometry(0.3, 0),
      );
    default:
      return cache.get("marker:none", () => new THREE.BoxGeometry(0, 0, 0));
  }
}

function machineTrimGeometry(
  archetype: string,
  cache: PlantGeometryCache,
): THREE.BufferGeometry {
  switch (archetype) {
    case "machine:agentic":
      return cache.get("trim:machine:agentic", () =>
        mergePlantGeometries([
          translated(horizontalTorus(0.39, 0.055, 16), 0, 0.18, 0),
          translated(horizontalTorus(0.31, 0.045, 16), 0, 0.62, 0),
        ]),
      );
    case "machine:gate":
      return cache.get("trim:machine:gate", () =>
        mergePlantGeometries([
          translated(new THREE.BoxGeometry(0.1, 0.7, 0.3), -0.36, 0, 0),
          translated(new THREE.BoxGeometry(0.1, 0.7, 0.3), 0.36, 0, 0),
          translated(new THREE.BoxGeometry(0.62, 0.09, 0.34), 0, 0.55, 0),
        ]),
      );
    case "machine:evaluator":
      return cache.get("trim:machine:evaluator", () =>
        mergePlantGeometries([
          translated(horizontalTorus(0.35, 0.045, 18), 0, 0.18, 0),
          translated(new THREE.BoxGeometry(0.8, 0.06, 0.3), 0, 0.43, 0),
        ]),
      );
    case "machine:parallel":
      return cache.get("trim:machine:parallel", () =>
        mergePlantGeometries([
          translated(new THREE.BoxGeometry(0.42, 0.1, 0.76), -0.26, 0.42, 0),
          translated(new THREE.BoxGeometry(0.42, 0.1, 0.76), 0.26, 0.28, 0),
        ]),
      );
    case "machine:deterministic":
      return cache.get("trim:machine:deterministic", () =>
        mergePlantGeometries([
          translated(new THREE.BoxGeometry(0.96, 0.1, 0.82), 0, 0.2, 0),
          translated(new THREE.BoxGeometry(0.66, 0.08, 0.56), 0, 0.58, 0),
        ]),
      );
    default:
      return cache.get("trim:machine:unknown", () =>
        horizontalTorus(0.48, 0.05, 16),
      );
  }
}

function applyBatchRecord(
  record: BatchRecord,
  risk: boolean,
  palette: PlantPalette,
): void {
  const level = batchRiskLevel(record.batch.materialArchetype);
  const markedRisk = level !== undefined;
  const context = risk && record.batch.dimmedInRisk && !markedRisk;
  const base = batchColor(record.batch.materialArchetype, palette);
  const color = context
    ? desaturateHexColor(base, PLANT_RISK_CONTEXT_DESATURATION)
    : base;
  record.material.color.set(color);
  record.material.emissive.set(color);
  const darkMachine =
    palette.theme === "dark" &&
    record.batch.meshArchetype.startsWith("machine");
  record.baseEmissive = darkMachine
    ? record.batch.active
    ? 0.22
    : 0.2
    : record.batch.active
      ? 0.07
      : 0.015;
  record.material.emissiveIntensity = record.baseEmissive;
  // Healthy context stays visible: the hazard has to be *somewhere*, and an
  // erased plant deletes the map it is somewhere on.
  applyDim([record.material], context);
  if (record.trim) {
    const trim = context
      ? desaturateHexColor(palette.machineTrim, PLANT_RISK_CONTEXT_DESATURATION)
      : palette.machineTrim;
    record.trim.material.color.set(trim);
    record.trim.material.emissive.set(trim);
    record.trim.material.emissiveIntensity = record.batch.active ? 0.18 : 0.06;
    applyDim([record.trim.material], context);
  }

  if (record.marker) {
    const markerLevel = record.marker.level;
    if (record.marker.material instanceof THREE.MeshBasicMaterial) {
      record.marker.material.color.set(palette.riskMarkerUnknown);
    } else if (record.marker.material instanceof THREE.MeshStandardMaterial) {
      record.marker.material.color.set(riskBeaconColor(markerLevel, palette));
      record.marker.material.emissive.set(riskStatusColor(markerLevel, palette));
      record.marker.material.emissiveIntensity = 0.55;
    }
  }

  if (record.ring) {
    record.ring.material.color.set(palette.riskRing);
    record.ring.material.emissive.set(riskStatusColor(level ?? "blocked", palette));
    record.ring.material.emissiveIntensity = 0.35;
  }
}

function matrixForInstance(
  instance: PlantInstanceTransform,
  target: THREE.Matrix4,
  offset?: PlantWorldPoint,
  yOffset = 0,
): void {
  const { position, rotationY, scale } = instance.transform;
  target.compose(
    new THREE.Vector3(
      position.x + (offset?.x ?? 0),
      position.y + yOffset,
      position.z + (offset?.z ?? 0),
    ),
    new THREE.Quaternion().setFromAxisAngle(
      new THREE.Vector3(0, 1, 0),
      rotationY,
    ),
    new THREE.Vector3(scale.x, scale.y, scale.z),
  );
}

function applyInstanceIdentity(record: BatchRecord): void {
  record.mesh.userData.plantInstanceIds = record.batch.instances.map(
    (instance) => instance.id,
  );
  record.mesh.userData.plantEntityKind =
    record.batch.meshArchetype === "carrier"
      ? "carrier"
      : record.batch.meshArchetype === "worker"
        ? "worker"
        : record.batch.meshArchetype.startsWith("machine")
          ? "station"
          : record.batch.meshArchetype;
}

function applyBatchMatrices(
  record: BatchRecord,
  elapsed: number,
  transfers: ReadonlyMap<string, PlantWorldPoint>,
): void {
  const matrix = new THREE.Matrix4();
  const markerMatrix = new THREE.Matrix4();
  const ringMatrix = new THREE.Matrix4();
  const identity = new THREE.Quaternion();
  const flat = new THREE.Quaternion().setFromAxisAngle(
    new THREE.Vector3(1, 0, 0),
    Math.PI / 2,
  );
  const unit = new THREE.Vector3(1, 1, 1);
  const animated =
    record.batch.meshArchetype === "carrier" ||
    record.batch.meshArchetype === "worker";

  for (let index = 0; index < record.batch.instances.length; index += 1) {
    const instance = record.batch.instances[index]!;
    const transfer =
      record.batch.meshArchetype === "carrier"
        ? transfers.get(instance.id)
        : undefined;
    const bob =
      animated && instance.active
        ? Math.sin(
            elapsed *
              (record.batch.meshArchetype === "worker" ? 3.2 : 2.8) +
              plantPhase(instance.animationKey ?? instance.id),
          ) * 0.1
        : 0;
    matrixForInstance(instance, matrix, transfer, bob);
    record.mesh.setMatrixAt(index, matrix);
    record.trim?.mesh.setMatrixAt(index, matrix);

    const { position, scale } = instance.transform;
    const x = position.x + (transfer?.x ?? 0);
    const y = position.y + bob;
    const z = position.z + (transfer?.z ?? 0);
    if (record.marker) {
      markerMatrix.compose(
        new THREE.Vector3(x, y + Math.max(0.5, scale.y) * 0.95, z),
        identity,
        unit,
      );
      record.marker.mesh.setMatrixAt(index, markerMatrix);
    }
    if (record.ring) {
      ringMatrix.compose(
        new THREE.Vector3(
          x,
          y - Math.max(0.5, scale.y) * 0.48 + 0.06,
          z,
        ),
        flat,
        unit,
      );
      record.ring.mesh.setMatrixAt(index, ringMatrix);
    }
  }
  record.mesh.instanceMatrix.needsUpdate = true;
  record.mesh.boundingBox = null;
  record.mesh.boundingSphere = null;
  if (record.trim) {
    record.trim.mesh.instanceMatrix.needsUpdate = true;
    record.trim.mesh.boundingBox = null;
    record.trim.mesh.boundingSphere = null;
  }
  if (record.marker) {
    record.marker.mesh.instanceMatrix.needsUpdate = true;
    record.marker.mesh.boundingBox = null;
    record.marker.mesh.boundingSphere = null;
  }
  if (record.ring) {
    record.ring.mesh.instanceMatrix.needsUpdate = true;
    record.ring.mesh.boundingBox = null;
    record.ring.mesh.boundingSphere = null;
  }
}

/**
 * The risk level a material archetype carries, if any.
 *
 * `unknown` is returned so the unread treatment can be applied, but it never
 * routes to a hazard colour: an unread signal is not a confirmed hazard.
 */
export function batchRiskLevel(archetype: string): PlantRiskLevel | undefined {
  const status = archetype.split(":").at(-1) ?? "";
  if (status === "blocked") {
    return "blocked";
  }
  if (status === "held" || status === "paused") {
    return "held";
  }
  if (status === "impeded") {
    return "impeded";
  }
  if (status === "unknown") {
    return "unknown";
  }
  return undefined;
}

function riskBeaconColor(level: PlantRiskLevel, palette: PlantPalette): string {
  switch (level) {
    case "blocked":
      return palette.riskBeaconBlocked;
    case "held":
      return palette.riskBeaconHeld;
    case "impeded":
      return palette.riskBeaconImpeded;
    default:
      return palette.riskMarkerUnknown;
  }
}

function riskStatusColor(level: PlantRiskLevel, palette: PlantPalette): string {
  switch (level) {
    case "blocked":
      return palette.statusBlocked;
    case "held":
      return palette.statusHeld;
    case "impeded":
      return palette.statusImpeded;
    case "unknown":
      return palette.statusUnknown;
    default:
      return palette.statusRunning;
  }
}

/**
 * Colour for one material archetype.
 *
 * Decks stay neutral. Status is painted on the kerb that edges a bay and on the
 * marker above a machine, not across the whole deck — a hall where every bay is
 * a flat sheet of alarm red has no legible alarm left in it.
 */
export function batchColor(archetype: string, palette: PlantPalette): string {
  const [family = "", ...rest] = archetype.split(":");
  const status = rest.at(-1) ?? "";

  if (family === "bay") {
    return status === "running" ? palette.pad : palette.padAlternate;
  }
  if (family === "bay-edge") {
    switch (status) {
      case "blocked":
        return palette.statusBlocked;
      case "held":
        return palette.statusHeld;
      case "impeded":
        return palette.statusImpeded;
      case "unknown":
        return palette.statusUnknown;
      case "running":
        return palette.statusRunning;
      default:
        return palette.padEdge;
    }
  }
  if (family === "machine") {
    switch (status) {
      case "blocked":
      case "held":
      case "impeded":
        // A hazard machine keeps a neutral body; the beacon and ring carry the
        // alarm so the silhouette never dissolves into a colour field.
        return mixHexColor(palette.machineBody, palette.machineCap, 0.25);
      case "unknown":
        // Unread, not alarmed. The body stays a legible neutral - pulling it
        // away from the primary body distinguishes it without borrowing the
        // hold colour or sinking the silhouette into the deck.
        return palette.machineBodyAlt;
      case "running":
        return palette.machineBody;
      default:
        return palette.machineBodyAlt;
    }
  }
  if (family === "track") {
    return status === "active" ? palette.statusRunning : palette.structure;
  }
  if (family === "carrier") {
    switch (status) {
      case "blocked":
        return palette.crateBlocked;
      case "paused":
        return palette.crateHeld;
      case "unknown":
        return palette.crateUnknown;
      default:
        return palette.crate;
    }
  }
  if (family === "worker") {
    return status === "active" ? palette.worker : palette.workerIdle;
  }
  if (family === "dock") {
    return palette.structureTrim;
  }
  if (family === "yard" || family === "commons") {
    return palette.aisle;
  }
  return palette.pad;
}

/* --------------------------------------------------------------------------
 * Keyed entity objects
 * ----------------------------------------------------------------------- */

export function createPlantEntityObject(
  spec: PlantEntitySpec,
  _palette: PlantPalette,
  _cache: PlantGeometryCache,
  _ledger: PlantResourceLedger,
): PlantEntityObject {
  // Every visible entity is instanced. These retained anchors carry identity,
  // projection and transfer lifecycle without adding one draw call per object.
  return createBatchedAnchor(spec);
}

function createBatchedAnchor(initial: PlantEntitySpec): PlantEntityObject {
  const object = new THREE.Object3D();
  let spec = initial;
  let transfer: PlantWorldPoint | undefined;
  const place = () => {
    object.position.set(
      spec.position.x + (transfer?.x ?? 0),
      0,
      spec.position.z + (transfer?.z ?? 0),
    );
    object.rotation.y = spec.orientation;
  };
  place();
  return {
    animate: () => undefined,
    apply: (next) => {
      spec = next;
      place();
    },
    dispose: () => {
      object.removeFromParent();
    },
    object,
    setTransfer: (offset) => {
      transfer = offset;
      place();
    },
  };
}

function createCrate(
  initial: PlantEntitySpec,
  palette: PlantPalette,
  cache: PlantGeometryCache,
  ledger: PlantResourceLedger,
): PlantEntityObject {
  const group = new THREE.Group();
  const material = new THREE.MeshStandardMaterial({ metalness: 0, roughness: 0.85 });
  const crate = new THREE.Mesh(
    cache.get("crate", () => new THREE.BoxGeometry(0.52, 0.4, 0.52)),
    material,
  );
  crate.castShadow = true;
  group.add(crate);

  // A work order that is confirmed blocked or held carries its own pin, so it
  // stays findable without reading the colour of a six-pixel box.
  const pinMaterial = new THREE.MeshStandardMaterial({
    metalness: 0,
    roughness: 0.4,
  });
  const pin = new THREE.Mesh(
    cache.get("crate:pin", () =>
      mergePlantGeometries([
        translated(new THREE.BoxGeometry(0.05, 0.34, 0.05), 0, 0.17, 0),
        translated(new THREE.OctahedronGeometry(0.13, 0), 0, 0.42, 0),
      ]),
    ),
    pinMaterial,
  );
  pin.position.y = 0.2;
  pin.visible = false;
  group.add(pin);

  let spec = initial;
  let transfer: PlantWorldPoint | undefined;
  let bob = 0;
  const baseY = 0.34;

  // The transfer moves the crate through the hall; the bob is idle-cycle
  // motion at the destination. Composing them here keeps a mid-transfer theme
  // change or reconcile from snapping the crate to either extreme.
  const place = () => {
    group.position.set(
      spec.position.x + (transfer?.x ?? 0),
      baseY + bob,
      spec.position.z + (transfer?.z ?? 0),
    );
  };

  const apply = (next: PlantEntitySpec, nextPalette: PlantPalette) => {
    spec = next;
    const context = next.emphasis === "context";
    const tone = plantToneColor(nextPalette, next.tone);
    const color = context
      ? desaturateHexColor(tone, PLANT_RISK_CONTEXT_DESATURATION)
      : tone;
    material.color.set(color);
    material.emissive.set(color);
    material.emissiveIntensity = next.active ? 0.08 : 0.015;
    applyDim([material, pinMaterial], context);
    const marker = next.marker;
    pin.visible = marker !== undefined;
    if (marker) {
      pinMaterial.color.set(
        marker.level === "unknown"
          ? nextPalette.riskMarkerUnknown
          : riskBeaconColor(marker.level, nextPalette),
      );
      pinMaterial.emissive.set(riskStatusColor(marker.level, nextPalette));
      pinMaterial.emissiveIntensity = 0.5;
    }
    if (!next.active) {
      bob = 0;
    }
    place();
  };

  apply(initial, palette);

  return {
    animate: (elapsed) => {
      bob = spec.active ? Math.sin(elapsed * 2.8 + spec.phase) * 0.1 : 0;
      place();
    },
    apply,
    dispose: () => {
      group.removeFromParent();
      group.clear();
      ledger.release(material);
      ledger.release(pinMaterial);
    },
    object: group,
    setTransfer: (offset) => {
      transfer = offset;
      place();
    },
  };
}

function createWorker(
  initial: PlantEntitySpec,
  palette: PlantPalette,
  cache: PlantGeometryCache,
  ledger: PlantResourceLedger,
): PlantEntityObject {
  const group = new THREE.Group();
  const bodyMaterial = new THREE.MeshStandardMaterial({
    metalness: 0,
    roughness: 0.82,
  });
  const visorMaterial = new THREE.MeshStandardMaterial({
    metalness: 0.1,
    roughness: 0.5,
  });
  const materials = [bodyMaterial, visorMaterial];

  const body = new THREE.Mesh(
    cache.get("worker:body", () => new THREE.CapsuleGeometry(0.16, 0.32, 4, 8)),
    bodyMaterial,
  );
  body.position.y = 0.48;
  body.castShadow = true;
  group.add(body);

  const visor = new THREE.Mesh(
    cache.get("worker:visor", () => new THREE.SphereGeometry(0.17, 12, 8)),
    visorMaterial,
  );
  visor.position.y = 0.84;
  visor.scale.z = 0.72;
  visor.castShadow = true;
  group.add(visor);

  let spec = initial;
  let bob = 0;
  const place = () => {
    group.position.set(spec.position.x, bob, spec.position.z);
  };

  const apply = (next: PlantEntitySpec, nextPalette: PlantPalette) => {
    spec = next;
    const context = next.emphasis === "context";
    const tone = plantToneColor(nextPalette, next.tone);
    bodyMaterial.color.set(
      context ? desaturateHexColor(tone, PLANT_RISK_CONTEXT_DESATURATION) : tone,
    );
    visorMaterial.color.set(nextPalette.workerVisor);
    applyDim(materials, context);
    if (!next.active) {
      bob = 0;
    }
    place();
  };

  apply(initial, palette);

  return {
    animate: (elapsed) => {
      bob = spec.active ? Math.sin(elapsed * 3.2 + spec.phase) * 0.1 : 0;
      place();
    },
    apply,
    dispose: () => {
      group.removeFromParent();
      group.clear();
      for (const material of materials) {
        ledger.release(material);
      }
    },
    object: group,
    setTransfer: () => undefined,
  };
}

/**
 * Risk-lens context treatment.
 *
 * Context is desaturated and held at {@link PLANT_RISK_CONTEXT_OPACITY}, above
 * the legibility floor the Plant promises. The previous 28% wipe is exactly
 * what made the Risk lens unusable.
 */
function applyDim(materials: readonly THREE.Material[], context: boolean): void {
  for (const material of materials) {
    material.opacity = context ? PLANT_RISK_CONTEXT_OPACITY : 1;
    if (material.transparent !== context) {
      material.transparent = context;
      material.needsUpdate = true;
    }
  }
}

/* --------------------------------------------------------------------------
 * Geometry helpers
 * ----------------------------------------------------------------------- */

function translated(
  geometry: THREE.BufferGeometry,
  x: number,
  y: number,
  z: number,
): THREE.BufferGeometry {
  geometry.translate(x, y, z);
  return geometry;
}

function horizontalTorus(
  radius: number,
  tube: number,
  radialSegments: number,
): THREE.BufferGeometry {
  const geometry = new THREE.TorusGeometry(radius, tube, 6, radialSegments);
  geometry.rotateX(Math.PI / 2);
  return geometry;
}

/**
 * Concatenates simple untextured geometries into one buffer.
 *
 * Composite silhouettes — an arch, a twin press, a beacon — need to be one
 * geometry so they can still be drawn as a single instanced batch. Only
 * position and normal are carried, because nothing in the hall is textured.
 */
export function mergePlantGeometries(
  parts: readonly THREE.BufferGeometry[],
): THREE.BufferGeometry {
  const flattened = parts.map((part) => (part.index ? part.toNonIndexed() : part));
  let vertices = 0;
  for (const part of flattened) {
    vertices += part.getAttribute("position").count;
  }
  const positions = new Float32Array(vertices * 3);
  const normals = new Float32Array(vertices * 3);
  let offset = 0;
  for (const part of flattened) {
    const position = part.getAttribute("position");
    const normal = part.getAttribute("normal");
    positions.set(position.array as Float32Array, offset * 3);
    if (normal) {
      normals.set(normal.array as Float32Array, offset * 3);
    }
    offset += position.count;
  }
  const merged = new THREE.BufferGeometry();
  merged.setAttribute("position", new THREE.BufferAttribute(positions, 3));
  merged.setAttribute("normal", new THREE.BufferAttribute(normals, 3));
  merged.computeBoundingSphere();
  for (const part of flattened) {
    part.dispose();
  }
  for (const part of parts) {
    if (!flattened.includes(part)) {
      part.dispose();
    }
  }
  return merged;
}

/**
 * Recolours a GridHelper without replacing its geometry.
 *
 * GridHelper bakes its two colours into a vertex attribute, so a theme change
 * used to mean a new helper and a new buffer every time. Rewriting the existing
 * attribute keeps the allocation stable for the life of the runtime.
 */
export function applyGridPalette(
  grid: THREE.GridHelper,
  centerColor: string,
  outerColor: string,
): void {
  const attribute = grid.geometry.getAttribute("color");
  if (!attribute) {
    return;
  }
  const center = new THREE.Color(centerColor);
  const outer = new THREE.Color(outerColor);
  const vertices = attribute.count;
  // GridHelper emits two lines (four vertices) per division step, and colours
  // the middle step with the center colour.
  const steps = vertices / 4;
  const middle = Math.floor(steps / 2);
  for (let step = 0; step < steps; step += 1) {
    const color = step === middle ? center : outer;
    for (let vertex = 0; vertex < 4; vertex += 1) {
      attribute.setXYZ(step * 4 + vertex, color.r, color.g, color.b);
    }
  }
  attribute.needsUpdate = true;
}
