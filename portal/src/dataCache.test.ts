import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { dataCacheKey, SessionDataCache } from "./dataCache";

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(1_000);
});

afterEach(() => {
  vi.useRealTimers();
});

describe("SessionDataCache", () => {
  it("returns a cached resource within its TTL", () => {
    const cache = new SessionDataCache(5_000);
    const key = dataCacheKey("run-detail", "run-1");
    cache.set(key, { id: "run-1" }, [{ model: "run", runId: "run-1" }]);

    vi.setSystemTime(5_999);

    expect(cache.get(key)).toEqual({ id: "run-1" });
  });

  it("treats an expired resource as a cache miss", () => {
    const cache = new SessionDataCache(5_000);
    const key = dataCacheKey("run-detail", "run-1");
    cache.set(key, { id: "run-1" }, [{ model: "run", runId: "run-1" }]);

    vi.setSystemTime(6_000);

    expect(cache.get(key)).toBeUndefined();
  });

  it("keeps a long-lived resource after the default cache tier expires", () => {
    const cache = new SessionDataCache(5_000);
    const runKey = dataCacheKey("run-detail", "run-1");
    const inventoryKey = dataCacheKey("operational-inventory");
    cache.set(runKey, "run", [{ model: "run" }]);
    cache.set(
      inventoryKey,
      "inventory",
      [{ model: "inventory" }],
      undefined,
      60_000,
    );

    vi.setSystemTime(6_000);

    expect(cache.get(runKey)).toBeUndefined();
    expect(cache.get(inventoryKey)).toBe("inventory");
  });

  it("coalesces concurrent loads for the same resource", async () => {
    const cache = new SessionDataCache();
    const key = dataCacheKey("operational-inventory");
    const load = vi.fn(async () => {
      await Promise.resolve();
      return "inventory";
    });

    const first = cache.getOrLoad(key, [{ model: "workflow" }], load, 60_000);
    const second = cache.getOrLoad(key, [{ model: "workflow" }], load, 60_000);

    await expect(Promise.all([first, second])).resolves.toEqual(["inventory", "inventory"]);
    expect(load).toHaveBeenCalledOnce();
  });

  it("evicts expired entries and pending write metadata without another read", async () => {
    const cache = new SessionDataCache(5_000);
    const key = dataCacheKey("run-detail", "run-1");
    const dependencies = [{ model: "run" as const, runId: "run-1" }];
    cache.set(key, { id: "run-1" }, dependencies);
    const revision = cache.beginWrite(key, dependencies);

    await vi.advanceTimersByTimeAsync(5_000);

    expect(cache.set(key, "obsolete", dependencies, revision)).toBe(false);
    expect(cache.get(key)).toBeUndefined();
  });

  it("invalidates only resources referenced by an SSE update", () => {
    const cache = new SessionDataCache();
    const runOne = dataCacheKey("run-detail", "run-1");
    const runTwo = dataCacheKey("run-detail", "run-2");
    const workflowOne = dataCacheKey("workflow-detail", "core", "implementation");
    const workflowTwo = dataCacheKey("workflow-detail", "tools", "implementation");
    cache.set(runOne, "run one", [{ model: "run", runId: "run-1" }]);
    cache.set(runTwo, "run two", [{ model: "run", runId: "run-2" }]);
    cache.set(workflowOne, "core workflow", [
      { model: "workflow", gaggle: "core", workflow: "implementation" },
    ]);
    cache.set(workflowTwo, "tools workflow", [
      { model: "workflow", gaggle: "tools", workflow: "implementation" },
    ]);

    cache.invalidate({
      cursor: "session:1",
      models: ["run", "workflow"],
      runIds: ["run-1"],
      workflows: [{ gaggle: "core", name: "implementation" }],
    });

    expect(cache.get(runOne)).toBeUndefined();
    expect(cache.get(workflowOne)).toBeUndefined();
    expect(cache.get(runTwo)).toBe("run two");
    expect(cache.get(workflowTwo)).toBe("tools workflow");
  });

  it("invalidates inventory only for a definition-wide workflow update", () => {
    const cache = new SessionDataCache();
    const inventory = dataCacheKey("operational-inventory");
    cache.set(inventory, "inventory", [{ model: "inventory" }], undefined, 60_000);

    cache.invalidate({
      cursor: "session:1",
      models: ["instance", "run", "workflow"],
      runIds: ["run-1"],
      workflows: [{ gaggle: "core", name: "implementation" }],
    });
    expect(cache.get(inventory)).toBe("inventory");

    cache.invalidate({
      cursor: "session:2",
      models: ["instance", "workflow"],
    });
    expect(cache.get(inventory)).toBeUndefined();
  });

  it("rejects a response that started before its resource was invalidated", () => {
    const cache = new SessionDataCache();
    const key = dataCacheKey("run-detail", "run-1");
    const dependencies = [{ model: "run" as const, runId: "run-1" }];
    const revision = cache.beginWrite(key, dependencies);

    cache.invalidate({
      cursor: "session:1",
      models: ["run"],
      runIds: ["run-1"],
    });

    expect(cache.set(key, "obsolete", dependencies, revision)).toBe(false);
    expect(cache.get(key)).toBeUndefined();
  });

  it("does not leak data between resource keys", () => {
    const cache = new SessionDataCache();
    const core = dataCacheKey("workflow-detail", "core", "implementation");
    const tools = dataCacheKey("workflow-detail", "tools", "implementation");
    cache.set(core, { gaggle: "core" }, [
      { model: "workflow", gaggle: "core", workflow: "implementation" },
    ]);

    expect(cache.get(core)).toEqual({ gaggle: "core" });
    expect(cache.get(tools)).toBeUndefined();
  });
});
