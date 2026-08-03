import type { CSSProperties } from "react";
import {
  CLASSIC_PLANT_HEIGHT,
  CLASSIC_PLANT_WIDTH,
  type ClassicPoint,
} from "./factoryClassicPlant";
import type { FactoryLens } from "./factoryModel";

type ProjectedPointStyle = CSSProperties & {
  "--factory-webgl-left": string;
  "--factory-webgl-top": string;
};

export function projectedPointStyle(
  point: ClassicPoint,
  elevation: number,
): ProjectedPointStyle {
  const worldX = (point.x / CLASSIC_PLANT_WIDTH - 0.5) * 27;
  const worldZ = (point.y / CLASSIC_PLANT_HEIGHT - 0.5) * 17;
  const cameraLength = Math.hypot(18, 19, 22);
  const cameraZ = {
    x: 18 / cameraLength,
    y: 19 / cameraLength,
    z: 22 / cameraLength,
  };
  const cameraXLength = Math.hypot(cameraZ.z, cameraZ.x);
  const cameraX = {
    x: cameraZ.z / cameraXLength,
    z: -cameraZ.x / cameraXLength,
  };
  const cameraY = {
    x: cameraZ.y * cameraX.z,
    y: cameraZ.z * cameraX.x - cameraZ.x * cameraX.z,
    z: -cameraZ.y * cameraX.x,
  };
  const projectedX = worldX * cameraX.x + worldZ * cameraX.z;
  const projectedY =
    worldX * cameraY.x + elevation * cameraY.y + worldZ * cameraY.z;
  const aspect = CLASSIC_PLANT_WIDTH / CLASSIC_PLANT_HEIGHT;

  return {
    left: `${(point.x / CLASSIC_PLANT_WIDTH) * 100}%`,
    top: `${(point.y / CLASSIC_PLANT_HEIGHT) * 100}%`,
    "--factory-webgl-left": `${(0.5 + projectedX / (20 * aspect)) * 100}%`,
    "--factory-webgl-top": `${(0.5 - projectedY / 20) * 100}%`,
  };
}

export function webGLMotionEnabled(
  lens: FactoryLens,
  reducedMotion: boolean,
  animatedObjectCount: number,
) {
  return !reducedMotion && lens !== "risk" && animatedObjectCount > 0;
}
