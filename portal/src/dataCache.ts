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
  expiresAt: number;
}

interface DataCacheMetadata {
  dependencies: readonly DataCacheDependency[];
  revision: number;
}

export class SessionDataCache {
  private readonly entries = new Map<string, DataCacheEntry>();
  private readonly metadata = new Map<string, DataCacheMetadata>();

  constructor(
    private readonly ttlMs = DATA_CACHE_TTL_MS,
    private readonly now: () => number = Date.now,
  ) {}

  get<T>(key: string): T | undefined {
    const entry = this.entries.get(key);
    if (!entry) {
      return undefined;
    }
    if (entry.expiresAt <= this.now()) {
      this.entries.delete(key);
      return undefined;
    }
    return entry.data as T;
  }

  beginWrite(key: string, dependencies: readonly DataCacheDependency[]): number {
    const metadata = this.metadata.get(key);
    if (metadata) {
      metadata.dependencies = dependencies;
      return metadata.revision;
    }
    this.metadata.set(key, { dependencies, revision: 0 });
    return 0;
  }

  set<T>(
    key: string,
    data: T,
    dependencies: readonly DataCacheDependency[],
    revision?: number,
  ): boolean {
    const currentRevision = this.beginWrite(key, dependencies);
    if (revision !== undefined && revision !== currentRevision) {
      return false;
    }
    this.entries.set(key, {
      data,
      expiresAt: this.now() + this.ttlMs,
    });
    return true;
  }

  remove(key: string): void {
    this.entries.delete(key);
    const metadata = this.metadata.get(key);
    if (metadata) {
      metadata.revision += 1;
    }
  }

  invalidate(invalidation: ModelInvalidation): void {
    const models = new Set<UpdateModel>(invalidation.models);
    for (const [key, metadata] of this.metadata) {
      if (
        metadata.dependencies.some(
          (dependency) =>
            models.has(dependency.model) && invalidatesDependency(dependency, invalidation),
        )
      ) {
        this.entries.delete(key);
        metadata.revision += 1;
      }
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
