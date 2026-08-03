import { useEffect, useRef, useState } from "react";
import * as THREE from "three";
import type { FactoryFloorModel, FactoryLens } from "../factoryModel";
import { carrierIsWorking } from "../factoryModel";
import { webGLMotionEnabled } from "../factoryWebGL";
import {
  CLASSIC_PLANT_HEIGHT,
  CLASSIC_PLANT_WIDTH,
  type ClassicPlantScene,
  type ClassicPoint,
} from "../factoryClassicPlant";

interface AnimatedObject {
  object: THREE.Object3D;
  phase: number;
  speed: number;
  kind: "bob" | "roll" | "spin" | "pulse";
}

export function FactoryWebGLScene({
  lens,
  model,
  reducedMotion,
  scene,
}: {
  lens: FactoryLens;
  model: FactoryFloorModel;
  reducedMotion: boolean;
  scene: ClassicPlantScene;
}) {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const [themeKey, setThemeKey] = useState(
    () => document.documentElement.getAttribute("data-theme") ?? "light",
  );
  const [rendererState, setRendererState] = useState<"pending" | "ready" | "fallback">(
    "pending",
  );

  useEffect(() => {
    const observer = new MutationObserver(() => {
      setThemeKey(document.documentElement.getAttribute("data-theme") ?? "light");
    });
    observer.observe(document.documentElement, {
      attributeFilter: ["data-theme"],
      attributes: true,
    });
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas || typeof WebGLRenderingContext === "undefined") {
      setRendererState("fallback");
      return;
    }

    let renderer: THREE.WebGLRenderer;
    try {
      renderer = new THREE.WebGLRenderer({
        alpha: true,
        antialias: true,
        canvas,
        powerPreference: "high-performance",
      });
    } catch {
      setRendererState("fallback");
      return;
    }

    const world = new THREE.Scene();
    const camera = new THREE.OrthographicCamera(-15, 15, 10, -10, 0.1, 100);
    camera.position.set(18, 19, 22);
    camera.lookAt(0, 0, 0);
    renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 1.6));
    renderer.shadowMap.enabled = true;
    renderer.shadowMap.type = THREE.PCFSoftShadowMap;
    renderer.outputColorSpace = THREE.SRGBColorSpace;
    renderer.toneMapping = THREE.ACESFilmicToneMapping;
    renderer.toneMappingExposure = 1.08;

    const colors = readSceneColors();
    const animated: AnimatedObject[] = [];
    buildLighting(world, colors);
    buildFactoryHall(world, colors);
    buildDistricts(world, colors);
    buildMachines(world, scene, lens, colors, animated);
    buildLocalConveyors(world, scene, lens, colors, animated);
    buildWorkOrders(world, scene, lens, colors, animated);
    buildWorkers(world, scene, model, lens, colors, animated);

    if (lens === "risk") {
      world.fog = new THREE.Fog(colors.background, 22, 48);
    }

    const resize = () => {
      const parent = canvas.parentElement;
      if (!parent) {
        return;
      }
      const width = Math.max(1, parent.clientWidth);
      const height = Math.max(1, parent.clientHeight);
      const aspect = width / height;
      const viewHeight = 20;
      camera.left = (-viewHeight * aspect) / 2;
      camera.right = (viewHeight * aspect) / 2;
      camera.top = viewHeight / 2;
      camera.bottom = -viewHeight / 2;
      camera.updateProjectionMatrix();
      renderer.setSize(width, height, false);
      renderer.render(world, camera);
    };

    const observer =
      typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(resize);
    if (observer && canvas.parentElement) {
      observer.observe(canvas.parentElement);
    } else {
      window.addEventListener("resize", resize);
    }

    let frame = 0;
    let stopped = false;
    const handleContextLost = (event: Event) => {
      event.preventDefault();
      setRendererState("fallback");
    };
    const handleContextRestored = () => {
      resize();
      setRendererState("ready");
    };
    canvas.addEventListener("webglcontextlost", handleContextLost);
    canvas.addEventListener("webglcontextrestored", handleContextRestored);
    const start = performance.now();
    const hasMotion = webGLMotionEnabled(lens, reducedMotion, animated.length);
    const render = (time: number) => {
      if (stopped) {
        return;
      }
      const elapsed = (time - start) / 1000;
      for (const item of animated) {
        const cycle = elapsed * item.speed + item.phase;
        if (item.kind === "spin") {
          item.object.rotation.y = cycle;
        } else if (item.kind === "roll") {
          item.object.rotation.z = cycle;
        } else if (item.kind === "bob") {
          item.object.position.y = item.object.userData.baseY + Math.sin(cycle) * 0.1;
        } else {
          const scale = 1 + (Math.sin(cycle) + 1) * 0.035;
          item.object.scale.setScalar(scale);
        }
      }
      renderer.render(world, camera);
      if (hasMotion) {
        frame = window.requestAnimationFrame(render);
      }
    };

    resize();
    setRendererState("ready");
    render(start);

    return () => {
      stopped = true;
      window.cancelAnimationFrame(frame);
      observer?.disconnect();
      window.removeEventListener("resize", resize);
      canvas.removeEventListener("webglcontextlost", handleContextLost);
      canvas.removeEventListener("webglcontextrestored", handleContextRestored);
      disposeScene(world);
      renderer.dispose();
    };
  }, [lens, model, reducedMotion, scene, themeKey]);

  return (
    <div
      aria-hidden="true"
      className="factory-plant-renderer"
      data-webgl={rendererState}
    >
      <img
        alt=""
        className="factory-plant-backdrop"
        draggable="false"
        src="/factory-plant-base.png"
      />
      <canvas className="factory-plant-webgl" ref={canvasRef} />
    </div>
  );
}

function buildLighting(
  scene: THREE.Scene,
  colors: ReturnType<typeof readSceneColors>,
) {
  scene.background = new THREE.Color(colors.background);
  scene.add(new THREE.HemisphereLight(colors.sky, colors.ground, 2.1));
  const key = new THREE.DirectionalLight(colors.key, 3.4);
  key.position.set(-12, 24, 12);
  key.castShadow = true;
  key.shadow.mapSize.set(2048, 2048);
  key.shadow.camera.left = -20;
  key.shadow.camera.right = 20;
  key.shadow.camera.top = 16;
  key.shadow.camera.bottom = -16;
  scene.add(key);
  const rim = new THREE.DirectionalLight(colors.accent, 1.2);
  rim.position.set(18, 10, -18);
  scene.add(rim);
}

function buildFactoryHall(
  scene: THREE.Scene,
  colors: ReturnType<typeof readSceneColors>,
) {
  const floor = new THREE.Mesh(
    new THREE.BoxGeometry(29, 0.45, 18),
    new THREE.MeshStandardMaterial({
      color: colors.floor,
      metalness: 0.08,
      roughness: 0.72,
    }),
  );
  floor.position.y = -0.3;
  floor.receiveShadow = true;
  scene.add(floor);

  const grid = new THREE.GridHelper(28, 28, colors.gridStrong, colors.grid);
  grid.position.y = -0.065;
  scene.add(grid);

  const wallMaterial = new THREE.MeshStandardMaterial({
    color: colors.wall,
    metalness: 0.12,
    roughness: 0.55,
  });
  const backWall = new THREE.Mesh(new THREE.BoxGeometry(29, 4.6, 0.35), wallMaterial);
  backWall.position.set(0, 2, -8.75);
  backWall.receiveShadow = true;
  scene.add(backWall);
  const sideWall = new THREE.Mesh(new THREE.BoxGeometry(0.35, 4.6, 18), wallMaterial);
  sideWall.position.set(-14.35, 2, 0);
  sideWall.receiveShadow = true;
  scene.add(sideWall);

  const railMaterial = new THREE.MeshStandardMaterial({
    color: colors.rail,
    metalness: 0.65,
    roughness: 0.28,
  });
  for (let x = -12; x <= 12; x += 4) {
    const beam = new THREE.Mesh(new THREE.BoxGeometry(0.16, 4.8, 0.16), railMaterial);
    beam.position.set(x, 2.15, -8.45);
    scene.add(beam);
  }

  for (let x = -11; x <= 11; x += 5.5) {
    const light = new THREE.Mesh(
      new THREE.BoxGeometry(2.6, 0.09, 0.24),
      new THREE.MeshStandardMaterial({
        color: colors.machineHighlight,
        emissive: colors.machineHighlight,
        emissiveIntensity: 1.7,
      }),
    );
    light.position.set(x, 4.05, -7.95);
    scene.add(light);
  }

  const gantryMaterial = new THREE.MeshStandardMaterial({
    color: colors.accent,
    metalness: 0.62,
    roughness: 0.26,
  });
  const gantry = new THREE.Group();
  const crossbeam = new THREE.Mesh(new THREE.BoxGeometry(9.8, 0.28, 0.34), gantryMaterial);
  crossbeam.position.y = 3.65;
  gantry.add(crossbeam);
  [-4.6, 4.6].forEach((x) => {
    const leg = new THREE.Mesh(new THREE.BoxGeometry(0.3, 3.7, 0.3), gantryMaterial);
    leg.position.set(x, 1.7, 0);
    gantry.add(leg);
  });
  gantry.position.set(0.8, 0, -1.8);
  scene.add(gantry);
}

function buildDistricts(
  scene: THREE.Scene,
  colors: ReturnType<typeof readSceneColors>,
) {
  const districts = [
    [-10.5, -4.8, 5.2, 4.8],
    [-8.6, 3.2, 6.2, 4],
    [-1.4, 0.4, 6.4, 5],
    [5.8, -2.6, 5.4, 5],
    [9.6, 4.5, 5.5, 4.2],
  ] as const;
  districts.forEach(([x, z, width, depth], index) => {
    const pad = new THREE.Mesh(
      new THREE.BoxGeometry(width, 0.16, depth),
      new THREE.MeshStandardMaterial({
        color: index % 2 === 0 ? colors.pad : colors.padAlternate,
        metalness: 0.16,
        roughness: 0.62,
      }),
    );
    pad.position.set(x, -0.02, z);
    pad.receiveShadow = true;
    scene.add(pad);

    const edgeMaterial = new THREE.MeshStandardMaterial({
      color: index % 2 === 0 ? colors.accent : colors.active,
      emissive: index % 2 === 0 ? colors.accent : colors.active,
      emissiveIntensity: 0.12,
      metalness: 0.32,
      roughness: 0.38,
    });
    const edge = new THREE.Mesh(new THREE.BoxGeometry(width, 0.06, 0.1), edgeMaterial);
    edge.position.set(x, 0.09, z - depth / 2 + 0.08);
    scene.add(edge);
  });
}

function buildMachines(
  world: THREE.Scene,
  scene: ClassicPlantScene,
  lens: FactoryLens,
  colors: ReturnType<typeof readSceneColors>,
  animated: AnimatedObject[],
) {
  scene.stations.forEach(({ machine, station }, index) => {
    const point = toWorld(machine);
    const statusColor =
      station.status === "blocked"
        ? colors.danger
        : station.status === "held" || station.status === "unknown"
          ? colors.warning
          : station.status === "running"
            ? colors.success
            : station.kind === "gate"
              ? colors.active
              : colors.accent;
    const group = new THREE.Group();
    group.position.set(point.x, 0, point.z);

    const base = new THREE.Mesh(
      new THREE.CylinderGeometry(0.56, 0.68, 0.3, 10),
      new THREE.MeshStandardMaterial({
        color: colors.machineBase,
        metalness: 0.58,
        roughness: 0.32,
      }),
    );
    base.position.y = 0.16;
    base.castShadow = true;
    base.receiveShadow = true;
    group.add(base);

    const body = new THREE.Mesh(
      station.kind === "gate"
        ? new THREE.BoxGeometry(0.78, 0.8, 0.78)
        : new THREE.CylinderGeometry(0.43, 0.5, 0.82, 10),
      new THREE.MeshPhysicalMaterial({
        clearcoat: 0.45,
        color: statusColor,
        emissive: statusColor,
        emissiveIntensity: station.status === "running" ? 0.16 : 0.03,
        metalness: 0.44,
        roughness: 0.27,
      }),
    );
    body.position.y = 0.72;
    body.castShadow = true;
    group.add(body);

    const rotor = new THREE.Mesh(
      new THREE.TorusGeometry(0.32, 0.07, 8, 20),
      new THREE.MeshStandardMaterial({
        color: colors.machineHighlight,
        emissive: statusColor,
        emissiveIntensity: station.status === "running" ? 0.48 : 0.08,
        metalness: 0.6,
        roughness: 0.2,
      }),
    );
    rotor.position.y = 1.17;
    rotor.rotation.x = Math.PI / 2;
    rotor.castShadow = true;
    group.add(rotor);

    const console = new THREE.Mesh(
      new THREE.BoxGeometry(0.46, 0.38, 0.28),
      new THREE.MeshStandardMaterial({
        color: colors.machineBase,
        emissive: statusColor,
        emissiveIntensity: station.status === "running" ? 0.38 : 0.05,
        metalness: 0.42,
        roughness: 0.3,
      }),
    );
    console.position.set(0.58, 0.38, 0.18);
    console.rotation.y = -0.24;
    console.castShadow = true;
    group.add(console);
    if (
      lens === "risk" &&
      !["blocked", "held", "impeded"].includes(station.status)
    ) {
      setObjectOpacity(group, 0.28);
    }
    world.add(group);

    if (station.status === "running") {
      animated.push({ kind: "spin", object: rotor, phase: index, speed: 2.4 });
    }
  });
}

function buildLocalConveyors(
  world: THREE.Scene,
  scene: ClassicPlantScene,
  lens: FactoryLens,
  colors: ReturnType<typeof readSceneColors>,
  animated: AnimatedObject[],
) {
  scene.stations.forEach(({ machine, station }, index) => {
    const point = toWorld(machine);
    const group = new THREE.Group();
    const horizontal = index % 2 === 0;
    group.position.set(
      point.x + (horizontal ? 1.15 : 0),
      0.16,
      point.z + (horizontal ? 0 : 1.15),
    );
    group.rotation.y = horizontal ? 0 : Math.PI / 2;

    const bed = new THREE.Mesh(
      new THREE.BoxGeometry(2.15, 0.22, 0.62),
      new THREE.MeshStandardMaterial({
        color: colors.rail,
        metalness: 0.68,
        roughness: 0.25,
      }),
    );
    bed.castShadow = true;
    bed.receiveShadow = true;
    group.add(bed);

    for (let rollerIndex = -4; rollerIndex <= 4; rollerIndex += 1) {
      const roller = new THREE.Mesh(
        new THREE.CylinderGeometry(0.09, 0.09, 0.54, 10),
        new THREE.MeshStandardMaterial({
          color: station.status === "running" ? colors.success : colors.machineBase,
          emissive: station.status === "running" ? colors.success : colors.machineBase,
          emissiveIntensity: station.status === "running" ? 0.22 : 0.02,
          metalness: 0.72,
          roughness: 0.2,
        }),
      );
      roller.rotation.x = Math.PI / 2;
      roller.position.set(rollerIndex * 0.22, 0.18, 0);
      group.add(roller);
      if (station.status === "running") {
        animated.push({
          kind: "roll",
          object: roller,
          phase: rollerIndex * 0.25,
          speed: 5.4,
        });
      }
    }
    if (
      lens === "risk" &&
      !["blocked", "held", "impeded"].includes(station.status)
    ) {
      setObjectOpacity(group, 0.28);
    }
    world.add(group);
  });
}

function buildWorkOrders(
  world: THREE.Scene,
  scene: ClassicPlantScene,
  lens: FactoryLens,
  colors: ReturnType<typeof readSceneColors>,
  animated: AnimatedObject[],
) {
  scene.carriers.forEach(({ carrier, point }, index) => {
    const position = toWorld(point);
    const color =
      carrier.state === "blocked"
        ? colors.danger
        : carrier.state === "paused"
          ? colors.warning
          : colors.crate;
    const crate = new THREE.Mesh(
      new THREE.BoxGeometry(0.52, 0.4, 0.52),
      new THREE.MeshStandardMaterial({
        color,
        emissive: color,
        emissiveIntensity: carrierIsWorking(carrier) ? 0.12 : 0.02,
        metalness: 0.18,
        roughness: 0.48,
      }),
    );
    crate.position.set(position.x, 0.34, position.z);
    crate.userData.baseY = crate.position.y;
    crate.castShadow = true;
    if (
      lens === "risk" &&
      carrier.state !== "blocked" &&
      carrier.state !== "paused"
    ) {
      setObjectOpacity(crate, 0.28);
    }
    world.add(crate);
    if (carrierIsWorking(carrier)) {
      animated.push({ kind: "bob", object: crate, phase: index * 0.7, speed: 2.8 });
    }
  });
}

function buildWorkers(
  world: THREE.Scene,
  scene: ClassicPlantScene,
  model: FactoryFloorModel,
  lens: FactoryLens,
  colors: ReturnType<typeof readSceneColors>,
  animated: AnimatedObject[],
) {
  const stationById = new Map(model.stations.map((station) => [station.id, station]));
  scene.workers.forEach(({ placement, point }, index) => {
    const position = toWorld(point);
    const group = new THREE.Group();
    group.position.set(position.x, 0, position.z);
    group.userData.baseY = 0;

    const body = new THREE.Mesh(
      new THREE.CapsuleGeometry(0.16, 0.32, 4, 8),
      new THREE.MeshStandardMaterial({
        color: placement.active ? colors.worker : colors.workerIdle,
        metalness: 0.12,
        roughness: 0.5,
      }),
    );
    body.position.y = 0.48;
    body.castShadow = true;
    group.add(body);
    const visor = new THREE.Mesh(
      new THREE.SphereGeometry(0.17, 12, 8),
      new THREE.MeshPhysicalMaterial({
        color: colors.visor,
        metalness: 0.65,
        roughness: 0.16,
        transmission: 0.08,
      }),
    );
    visor.position.y = 0.84;
    visor.scale.z = 0.72;
    visor.castShadow = true;
    group.add(visor);
    if (lens === "risk") {
      setObjectOpacity(group, 0.28);
    }
    world.add(group);

    const working =
      placement.stationId &&
      stationById.get(placement.stationId)?.status === "running";
    if (working) {
      animated.push({ kind: "bob", object: group, phase: index * 0.9, speed: 3.2 });
    }
  });
}

function toWorld(point: ClassicPoint): { x: number; z: number } {
  return {
    x: (point.x / CLASSIC_PLANT_WIDTH - 0.5) * 27,
    z: (point.y / CLASSIC_PLANT_HEIGHT - 0.5) * 17,
  };
}

function readSceneColors() {
  const styles = getComputedStyle(document.documentElement);
  const color = (name: string, fallback: string) =>
    styles.getPropertyValue(name).trim() || fallback;
  return {
    accent: color("--accent", "#b11f4b"),
    active: color("--active", "#2563eb"),
    background: color("--surface", "#ffffff"),
    crate: color("--accent", "#b11f4b"),
    danger: color("--danger", "#dc2626"),
    floor: color("--bg", "#f7f4ef"),
    grid: color("--line", "#dedede"),
    gridStrong: color("--line-strong", "#919191"),
    ground: color("--muted-strong", "#5c5c5c"),
    key: color("--panel-raised", "#ffffff"),
    machineBase: color("--muted-strong", "#5c5c5c"),
    machineHighlight: color("--panel-raised", "#ffffff"),
    pad: color("--panel", "#ffffff"),
    padAlternate: color("--surface", "#f5f5f5"),
    rail: color("--line-strong", "#919191"),
    sky: color("--panel-raised", "#ffffff"),
    success: color("--success", "#16a34a"),
    visor: color("--ink", "#242424"),
    wall: color("--panel", "#ffffff"),
    warning: color("--warning", "#f59e0b"),
    worker: color("--accent", "#b11f4b"),
    workerIdle: color("--muted", "#919191"),
  };
}

function setObjectOpacity(object: THREE.Object3D, opacity: number) {
  object.traverse((child) => {
    if (!(child instanceof THREE.Mesh)) {
      return;
    }
    const materials = Array.isArray(child.material) ? child.material : [child.material];
    for (const material of materials) {
      material.opacity = opacity;
      material.transparent = true;
    }
  });
}

function disposeScene(scene: THREE.Scene) {
  scene.traverse((object) => {
    if (object instanceof THREE.Mesh) {
      object.geometry.dispose();
      const materials = Array.isArray(object.material) ? object.material : [object.material];
      materials.forEach((material) => material.dispose());
    }
  });
}
