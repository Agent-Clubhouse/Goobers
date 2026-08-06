import * as THREE from "three";
import { describe, expect, it } from "vitest";
import {
  fitFactoryPlantCamera,
  fitFactoryPlantCameraToSafeArea,
} from "../factoryPlantLayout";
import {
  createPlantProjector,
  plantProjectionSignature,
  plantScreenRect,
  plantScreenToNdc,
  projectPlantWorldPoint,
  PLANT_PROJECTION_TOLERANCE_PX,
  type PlantProjectionState,
} from "../plantProjection";

/**
 * The projection contract, validated against a real Three.js camera.
 *
 * The whole wave rests on one claim: what `plantProjection` computes is what
 * the renderer drew. These tests hold the pure module to the library, so a
 * silent divergence fails here instead of as pixel drift in the browser.
 */

function cameraFor(fit: ReturnType<typeof fitFactoryPlantCamera>) {
  const camera = new THREE.OrthographicCamera(
    -fit.viewWidth / 2,
    fit.viewWidth / 2,
    fit.viewHeight / 2,
    -fit.viewHeight / 2,
    fit.near,
    fit.far,
  );
  camera.position.set(fit.position.x, fit.position.y, fit.position.z);
  camera.lookAt(fit.target.x, fit.target.y, fit.target.z);
  camera.updateProjectionMatrix();
  camera.updateMatrixWorld();
  return camera;
}

function stateFor(
  fit: ReturnType<typeof fitFactoryPlantCamera>,
  width: number,
  height: number,
): PlantProjectionState {
  const camera = cameraFor(fit);
  return {
    canvas: plantScreenRect(0, 0, width, height),
    matrix: new THREE.Matrix4()
      .multiplyMatrices(camera.projectionMatrix, camera.matrixWorldInverse)
      .toArray(),
    revision: 1,
    safeArea: plantScreenRect(0, 0, width, height),
    source: "webgl",
  };
}

const BOUNDS = {
  center: { x: 0, y: 1, z: 0 },
  max: { x: 12, y: 3, z: 8 },
  min: { x: -12, y: 0, z: -8 },
  size: { x: 24, y: 3, z: 16 },
};

describe("plant projection contract", () => {
  it("matches Three.js Vector3.project for arbitrary world points", () => {
    const fit = fitFactoryPlantCamera(BOUNDS, 1440 / 1000);
    const camera = cameraFor(fit);
    const state = stateFor(fit, 1440, 1000);

    const samples = [
      { x: 0, y: 0, z: 0 },
      { x: 11.5, y: 2.4, z: -7.5 },
      { x: -9.25, y: 0.8, z: 6.125 },
      { x: 3.5, y: 1.55, z: 2.25 },
    ];
    for (const sample of samples) {
      const expected = new THREE.Vector3(sample.x, sample.y, sample.z).project(
        camera,
      );
      const actual = projectPlantWorldPoint(sample, state.matrix, state.canvas);
      expect(actual.x).toBeCloseTo(((expected.x + 1) / 2) * 1440, 6);
      expect(actual.y).toBeCloseTo(((1 - expected.y) / 2) * 1000, 6);
      expect(actual.depth).toBeCloseTo(expected.z, 6);
    }
  });

  it("round-trips screen points through normalised device coordinates", () => {
    const canvas = plantScreenRect(0, 0, 800, 600);
    const centre = plantScreenToNdc({ x: 400, y: 300 }, canvas);
    expect(centre.x).toBeCloseTo(0, 12);
    expect(centre.y).toBeCloseTo(0, 12);
    expect(plantScreenToNdc({ x: 0, y: 0 }, canvas)).toEqual({ x: -1, y: 1 });
    expect(plantScreenToNdc({ x: 800, y: 600 }, canvas)).toEqual({ x: 1, y: -1 });
  });

  it("marks points outside the canvas or the frustum as not visible", () => {
    const fit = fitFactoryPlantCamera(BOUNDS, 1440 / 1000);
    const state = stateFor(fit, 1440, 1000);
    const projector = createPlantProjector(state);
    expect(projector.project({ x: 0, y: 0, z: 0 }).visible).toBe(true);
    expect(projector.project({ x: 4_000, y: 0, z: 0 }).visible).toBe(false);
  });

  it("keeps the projected world inside a shrunken safe area", () => {
    const safeArea = { height: 1000, left: 0, top: 0, width: 1090 };
    const fit = fitFactoryPlantCameraToSafeArea(
      BOUNDS,
      { height: 1000, width: 1440 },
      safeArea,
    );
    const state = {
      ...stateFor(fit, 1440, 1000),
      safeArea: plantScreenRect(0, 0, 1090, 1000),
    };
    const projector = createPlantProjector(state);
    const corners = [
      { x: BOUNDS.min.x, y: BOUNDS.min.y, z: BOUNDS.min.z },
      { x: BOUNDS.min.x, y: BOUNDS.max.y, z: BOUNDS.max.z },
      { x: BOUNDS.max.x, y: BOUNDS.min.y, z: BOUNDS.max.z },
      { x: BOUNDS.max.x, y: BOUNDS.max.y, z: BOUNDS.min.z },
    ];
    for (const corner of corners) {
      const point = projector.project(corner);
      expect(point.x).toBeGreaterThanOrEqual(-PLANT_PROJECTION_TOLERANCE_PX);
      expect(point.x).toBeLessThanOrEqual(1090 + PLANT_PROJECTION_TOLERANCE_PX);
      expect(point.y).toBeGreaterThanOrEqual(-PLANT_PROJECTION_TOLERANCE_PX);
      expect(point.y).toBeLessThanOrEqual(1000 + PLANT_PROJECTION_TOLERANCE_PX);
    }
  });

  it("changes its signature only when a projection input changes", () => {
    const fit = fitFactoryPlantCamera(BOUNDS, 1440 / 1000);
    const state = stateFor(fit, 1440, 1000);
    expect(plantProjectionSignature(state)).toBe(
      plantProjectionSignature({ ...state, revision: 99 } as PlantProjectionState),
    );
    expect(plantProjectionSignature(state)).not.toBe(
      plantProjectionSignature({
        ...state,
        canvas: plantScreenRect(0, 0, 1441, 1000),
      }),
    );
    expect(plantProjectionSignature(state)).not.toBe(
      plantProjectionSignature({
        ...state,
        safeArea: plantScreenRect(0, 0, 1090, 1000),
      }),
    );
  });
});
