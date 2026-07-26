import type { ModelInvalidation, UpdateModel, WorkflowUpdateReference } from "./api/types";

export const DATA_CACHE_TTL_MS = 30_000;

export type DataCacheDependency =
  | { model: "instance" }
  | {
      model: "run";
      runId?: string;
      gaggle?: string;
      workflow?: string;
    }
  | {
      model: "workflow";
      gaggle?: string;
      workflow?: string;
    };

interface DataCacheEntry {
  data: unknown;
  dependencies: readonly DataCacheDependency[];
  expiresAt: number;
}

interface DataCacheWrite {
  dependencies: readonly DataCacheDependency[];
  revision: number;
  expiresAt: number;
}

export class SessionDataCache {
  private readonly entries = new Map<string, DataCacheEntry>();
  private readonly writes = new Map<string, DataCacheWrite>();
  private evictionTimer: ReturnType<typeof setTimeout> | undefined;
  private revision = 0;

  constructor(private readonly ttlMs = DATA_CACHE_TTL_MS) {}

  get<T>(key: string): T | undefined {
    const entry = this.entries.get(key);
    if (!entry) {
      return undefined;
    }
    if (entry.expiresAt <= Date.now()) {
      this.entries.delete(key);
      this.scheduleEviction();
      return undefined;
    }
    return entry.data as T;
  }

  beginWrite(key: string, dependencies: readonly DataCacheDependency[]): number {
    this.revision += 1;
    this.writes.set(key, {
      dependencies,
      revision: this.revision,
      expiresAt: Date.now() + this.ttlMs,
    });
    this.scheduleEviction();
    return this.revision;
  }

  set<T>(
    key: string,
    data: T,
    dependencies: readonly DataCacheDependency[],
    revision?: number,
  ): boolean {
    if (revision !== undefined) {
      if (this.writes.get(key)?.revision !== revision) {
        return false;
      }
      this.writes.delete(key);
    } else {
      this.writes.delete(key);
    }
    this.entries.set(key, {
      data,
      dependencies,
      expiresAt: Date.now() + this.ttlMs,
    });
    this.scheduleEviction();
    return true;
  }

  remove(key: string): void {
    this.entries.delete(key);
    this.writes.delete(key);
    this.scheduleEviction();
  }

  invalidate(invalidation: ModelInvalidation): void {
    const models = new Set<UpdateModel>(invalidation.models);
    for (const [key, entry] of this.entries) {
      if (
        entry.dependencies.some(
          (dependency) =>
            models.has(dependency.model) && invalidatesDependency(dependency, invalidation),
        )
      ) {
        this.entries.delete(key);
      }
    }
    for (const [key, write] of this.writes) {
      if (
        write.dependencies.some(
          (dependency) =>
            models.has(dependency.model) && invalidatesDependency(dependency, invalidation),
        )
      ) {
        this.writes.delete(key);
      }
    }
    this.scheduleEviction();
  }

  // Cancels the pending eviction timer. LiveDataController.stop() calls this so
  // teardown leaves no live timers behind; the next write reschedules eviction.
  dispose(): void {
    if (this.evictionTimer !== undefined) {
      clearTimeout(this.evictionTimer);
      this.evictionTimer = undefined;
    }
  }

  private readonly evictExpired = (): void => {
    this.evictionTimer = undefined;
    const now = Date.now();
    for (const [key, entry] of this.entries) {
      if (entry.expiresAt <= now) {
        this.entries.delete(key);
      }
    }
    for (const [key, write] of this.writes) {
      if (write.expiresAt <= now) {
        this.writes.delete(key);
      }
    }
    this.scheduleEviction();
  };

  private scheduleEviction(): void {
    if (this.evictionTimer !== undefined) {
      clearTimeout(this.evictionTimer);
      this.evictionTimer = undefined;
    }
    let nextExpiration = Number.POSITIVE_INFINITY;
    for (const entry of this.entries.values()) {
      nextExpiration = Math.min(nextExpiration, entry.expiresAt);
    }
    for (const write of this.writes.values()) {
      nextExpiration = Math.min(nextExpiration, write.expiresAt);
    }
    if (Number.isFinite(nextExpiration)) {
      this.evictionTimer = setTimeout(
        this.evictExpired,
        Math.max(0, nextExpiration - Date.now()),
      );
    }
  }
}

export function dataCacheKey(namespace: string, ...parts: string[]): string {
  return JSON.stringify([namespace, ...parts]);
}

function invalidatesDependency(
  dependency: DataCacheDependency,
  invalidation: ModelInvalidation,
): boolean {
  if (dependency.model === "instance") {
    return true;
  }
  if (dependency.model === "workflow") {
    return matchesWorkflow(dependency, invalidation.workflows);
  }
  if (dependency.runId) {
    return invalidation.runIds?.length
      ? invalidation.runIds.includes(dependency.runId)
      : true;
  }
  return matchesWorkflow(dependency, invalidation.workflows);
}

function matchesWorkflow(
  dependency: { gaggle?: string; workflow?: string },
  workflows: WorkflowUpdateReference[] | undefined,
): boolean {
  if (!dependency.gaggle && !dependency.workflow) {
    return true;
  }
  if (!workflows?.length) {
    return true;
  }
  return workflows.some(
    (workflow) =>
      (!dependency.gaggle || workflow.gaggle === dependency.gaggle) &&
      (!dependency.workflow || workflow.name === dependency.workflow),
  );
}
