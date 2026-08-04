import { describe, expect, it } from "vitest";

import {
  buildPlantEntitySpecs,
  plantEntityRegistryKey,
  plantPhase,
  reconcilePlantEntities,
  type PlantEntityRecord,
  type PlantEntitySpec,
} from "./factoryPlantEntities";
import { plantFixture, plantStageChangeFixture } from "../../test/plantFixtures";

interface FakeEntity {
  id: number;
  shape: string;
  disposals: number;
  updates: number;
}

function createRegistry() {
  const registry = new Map<string, PlantEntityRecord<FakeEntity>>();
  let nextId = 1;
  const handlers = {
    create: (spec: PlantEntitySpec): FakeEntity => {
      const entity = { disposals: 0, id: nextId, shape: spec.shape, updates: 0 };
      nextId += 1;
      return entity;
    },
    dispose: (entity: FakeEntity) => {
      entity.disposals += 1;
    },
    update: (entity: FakeEntity) => {
      entity.updates += 1;
    },
  };
  return { handlers, registry };
}

function keysOf(specs: readonly PlantEntitySpec[], entity: string): string[] {
  return specs.filter((spec) => spec.entity === entity).map((spec) => spec.key);
}

describe("Plant entity specifications", () => {
  it("keys entities by the semantic identity the HTML layer uses", () => {
    const { layout, model } = plantFixture();
    const specs = buildPlantEntitySpecs({ layout, lens: "world", model });

    expect(keysOf(specs, "machine")).toEqual(
      layout.machines.map((machine) => machine.id),
    );
    expect(keysOf(specs, "conveyor")).toEqual(
      layout.tracks.map((track) => track.id),
    );
    expect(keysOf(specs, "crate")).toEqual(
      layout.carriers.filter((carrier) => carrier.rendered).map((carrier) => carrier.id),
    );
    expect(keysOf(specs, "worker")).toEqual(
      layout.workers.filter((worker) => worker.rendered).map((worker) => worker.id),
    );
  });

  it("derives animation phase from identity, not array position", () => {
    const { layout, model } = plantFixture();
    const specs = buildPlantEntitySpecs({ layout, lens: "world", model });
    const crate = specs.find((spec) => spec.entity === "crate");

    expect(crate).toBeDefined();
    expect(crate?.phase).toBeCloseTo(plantPhase(crate?.key ?? ""), 10);

    const reordered = {
      ...layout,
      carriers: [...layout.carriers].reverse(),
    };
    const shuffled = buildPlantEntitySpecs({
      layout: reordered,
      lens: "world",
      model,
    });
    const same = shuffled.find((spec) => spec.key === crate?.key);
    expect(same?.phase).toBe(crate?.phase);
    expect(same?.orientation).toBe(crate?.orientation);
  });

  it("stops truthful motion under the Risk lens by demoting healthy work", () => {
    const { layout, model } = plantFixture();
    const world = buildPlantEntitySpecs({ layout, lens: "world", model });
    const risk = buildPlantEntitySpecs({ layout, lens: "risk", model });

    expect(world.every((spec) => spec.emphasis === "primary")).toBe(true);
    expect(world.every((spec) => spec.marker === undefined)).toBe(true);
    expect(risk.some((spec) => spec.emphasis === "context")).toBe(true);
    // Risk keeps the same identities, so the runtime never rebuilds for a lens.
    expect(risk.map((spec) => spec.key)).toEqual(world.map((spec) => spec.key));
    expect(risk.map((spec) => spec.shape)).toEqual(world.map((spec) => spec.shape));
  });

  it("marks a confirmed stage change with a replay-proof signature", () => {
    const { after, before } = plantStageChangeFixture();

    const beforeSpecs = buildPlantEntitySpecs({
      layout: before.layout,
      lens: "world",
      model: before.model,
    });
    const afterSpecs = buildPlantEntitySpecs({
      layout: after.layout,
      lens: "world",
      model: after.model,
    });

    const moved = afterSpecs.find(
      (spec) => spec.entity === "crate" && spec.key === "01RUNIMPLEMENT1",
    );
    const stationary = beforeSpecs.find(
      (spec) => spec.entity === "crate" && spec.key === "01RUNIMPLEMENT1",
    );
    expect(stationary?.transfer).toBeUndefined();
    expect(stationary?.position).not.toEqual(moved?.position);
    expect(moved?.transfer).toBeDefined();

    // The crate slides out of the machine it actually left.
    const fromStationId = before.model.carriers.find(
      (carrier) => carrier.runId === "01RUNIMPLEMENT1",
    )?.stationId;
    const fromMachine = after.layout.machines.find(
      (machine) => machine.id === fromStationId,
    );
    expect(fromMachine).toBeDefined();
    expect(moved?.transfer?.from).toEqual({
      x: fromMachine?.transform.position.x,
      z: fromMachine?.transform.position.z,
    });

    // Re-deriving the same confirmed state yields the same signature, which is
    // how a theme or lens change is prevented from replaying the move.
    const repeat = buildPlantEntitySpecs({
      layout: after.layout,
      lens: "flow",
      model: after.model,
    }).find((spec) => spec.entity === "crate" && spec.key === "01RUNIMPLEMENT1");
    expect(repeat?.transfer?.signature).toBe(moved?.transfer?.signature);
  });
});

describe("Plant entity reconciliation", () => {
  it("retains entity identity across model, lens and theme churn", () => {
    const { handlers, registry } = createRegistry();
    const { layout, model } = plantFixture();

    const first = reconcilePlantEntities(
      registry,
      buildPlantEntitySpecs({ layout, lens: "world", model }),
      handlers,
    );
    expect(first.created).toBeGreaterThan(0);
    expect(first.updated).toBe(0);
    const identities = new Map(
      [...registry].map(([key, record]) => [key, record.entity.id]),
    );

    const second = reconcilePlantEntities(
      registry,
      buildPlantEntitySpecs({ layout, lens: "risk", model }),
      handlers,
    );
    expect(second.created).toBe(0);
    expect(second.replaced).toBe(0);
    expect(second.removed).toBe(0);
    expect(second.updated).toBe(first.created);
    for (const [key, record] of registry) {
      expect(record.entity.id).toBe(identities.get(key));
      expect(record.entity.disposals).toBe(0);
    }
  });

  it("disposes a removed entity exactly once", () => {
    const { handlers, registry } = createRegistry();
    const full = plantFixture();
    const specs = buildPlantEntitySpecs({
      layout: full.layout,
      lens: "world",
      model: full.model,
    });
    reconcilePlantEntities(registry, specs, handlers);

    const dropped = specs.find((spec) => spec.entity === "crate");
    const droppedKey = plantEntityRegistryKey("crate", dropped?.key ?? "");
    const removedEntity = registry.get(droppedKey)?.entity;
    expect(removedEntity).toBeDefined();

    const stats = reconcilePlantEntities(
      registry,
      specs.filter((spec) => spec !== dropped),
      handlers,
    );
    expect(stats.removed).toBe(1);
    expect(registry.has(droppedKey)).toBe(false);
    expect(removedEntity?.disposals).toBe(1);

    // A second pass with the entity already gone must not dispose it again.
    reconcilePlantEntities(
      registry,
      specs.filter((spec) => spec !== dropped),
      handlers,
    );
    expect(removedEntity?.disposals).toBe(1);
  });

  it("replaces an object only when its structural shape changes", () => {
    const { handlers, registry } = createRegistry();
    const { layout, model } = plantFixture();
    const specs = buildPlantEntitySpecs({ layout, lens: "world", model });
    reconcilePlantEntities(registry, specs, handlers);

    const machine = specs.find((spec) => spec.entity === "machine");
    const machineKey = plantEntityRegistryKey("machine", machine?.key ?? "");
    const original = registry.get(machineKey)?.entity;
    expect(original).toBeDefined();

    const toneOnly = specs.map((spec) =>
      spec === machine ? { ...spec, tone: "machineTrim" as const } : spec,
    );
    reconcilePlantEntities(registry, toneOnly, handlers);
    expect(registry.get(machineKey)?.entity.id).toBe(original?.id);

    const reshaped = specs.map((spec) =>
      spec === machine
        ? {
            ...spec,
            shape:
              spec.shape === "machine:gate" ? "machine:agentic" : "machine:gate",
          }
        : spec,
    );
    const stats = reconcilePlantEntities(registry, reshaped, handlers);
    expect(stats.replaced).toBe(1);
    expect(original?.disposals).toBe(1);
    expect(registry.get(machineKey)?.entity.id).not.toBe(original?.id);
  });

  it("ignores duplicate keys instead of building two objects for one identity", () => {
    const { handlers, registry } = createRegistry();
    const { layout, model } = plantFixture();
    const specs = buildPlantEntitySpecs({ layout, lens: "world", model });

    const stats = reconcilePlantEntities(registry, [...specs, ...specs], handlers);
    expect(stats.created).toBe(specs.length);
    expect(stats.live).toBe(specs.length);
    expect(registry.size).toBe(specs.length);
  });
});
