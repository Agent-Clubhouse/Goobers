import {
  createContext,
  useContext,
  useLayoutEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { DaemonApiError } from "./api/errors";
import type {
  DaemonClient,
  DaemonEventStream,
  DaemonUpdateEvent,
  ModelInvalidation,
  UpdateModel,
} from "./api/types";
import { SessionDataCache } from "./dataCache";
import type { PortalDiagnostics } from "./portalDiagnostics";

const ALL_MODELS: UpdateModel[] = ["instance", "run", "workflow"];
const CURSOR_STORAGE_KEY = "goobers-live-event-cursor";
const SEEN_EVENT_LIMIT = 512;

export type LiveFreshness =
  | "connected"
  | "reconnecting"
  | "stale"
  | "offline"
  | "polling-fallback";

export interface LiveDataConfig {
  invalidationWindowMs: number;
  reconnectBaseDelayMs: number;
  reconnectMaxDelayMs: number;
  failuresBeforePolling: number;
  pollingIntervalMs: number;
}

const defaultConfig: LiveDataConfig = {
  invalidationWindowMs: 50,
  reconnectBaseDelayMs: 250,
  reconnectMaxDelayMs: 30_000,
  failuresBeforePolling: 3,
  pollingIntervalMs: 5_000,
};

type ModelListener = (
  models: ReadonlySet<UpdateModel>,
  reason: "initial" | "refresh",
) => boolean | void | Promise<boolean | void>;
type StateListener = (state: LiveFreshness) => void;

export interface LiveDataScope {
  gaggle?: string;
  runId?: string;
  workflow?: string;
}

export interface LiveDataDependencies {
  diagnostics?: PortalDiagnostics;
  // Injected so a test (or a future caller) can observe cache behaviour; the
  // controller owns a session-scoped default when none is supplied.
  cache?: SessionDataCache;
}

interface LiveDataContextValue {
  cache: SessionDataCache;
  freshness: LiveFreshness;
  isFresh: () => boolean;
  refresh: (models?: readonly UpdateModel[]) => void;
  subscribe: (
    models: readonly UpdateModel[],
    listener: ModelListener,
    scope?: LiveDataScope,
  ) => () => void;
}

const LiveDataContext = createContext<LiveDataContextValue | undefined>(undefined);

export function LiveDataProvider({
  children,
  client,
  config,
  diagnostics,
}: {
  children: ReactNode;
  client: DaemonClient;
  config?: Partial<LiveDataConfig>;
  diagnostics?: PortalDiagnostics;
}) {
  const cache = useMemo(() => new SessionDataCache(), [client]);
  const controller = useMemo(
    () =>
      new LiveDataController(client, { ...defaultConfig, ...config }, { diagnostics, cache }),
    [
      cache,
      client,
      config?.failuresBeforePolling,
      config?.invalidationWindowMs,
      config?.pollingIntervalMs,
      config?.reconnectBaseDelayMs,
      config?.reconnectMaxDelayMs,
      diagnostics,
    ],
  );
  const [freshness, setFreshness] = useState<LiveFreshness>(() => controller.freshness);

  useLayoutEffect(() => {
    const unsubscribe = controller.subscribeState(setFreshness);
    controller.start();
    return () => {
      unsubscribe();
      controller.stop();
    };
  }, [controller]);

  const value = useMemo<LiveDataContextValue>(
    () => ({
      cache,
      freshness,
      isFresh: controller.isFresh,
      refresh: controller.refresh,
      subscribe: controller.subscribe,
    }),
    [cache, controller, freshness],
  );

  return <LiveDataContext.Provider value={value}>{children}</LiveDataContext.Provider>;
}

export function useLiveData(): LiveDataContextValue {
  const value = useContext(LiveDataContext);
  if (!value) {
    throw new Error("Live data hooks require a LiveDataProvider.");
  }
  return value;
}

export class LiveDataController {
  private readonly listeners = new Set<{
    listener: ModelListener;
    models: ReadonlySet<UpdateModel>;
    scope: LiveDataScope | undefined;
  }>();
  private readonly stateListeners = new Set<StateListener>();
  private readonly pendingInvalidations: ModelInvalidation[] = [];
  private readonly seenEventIds = new Set<string>();
  private readonly seenEventOrder: string[] = [];
  private activeStream: DaemonEventStream | undefined;
  private connectController: AbortController | undefined;
  private cursor: string | undefined;
  private failureCount = 0;
  private generation = 0;
  private invalidationFlush: Promise<void> | undefined;
  private invalidationsPaused = false;
  private invalidationRevision = 0;
  private invalidationTimer: ReturnType<typeof setTimeout> | undefined;
  private polling = false;
  private pollingTimer: ReturnType<typeof setTimeout> | undefined;
  private reconnectTimer: ReturnType<typeof setTimeout> | undefined;
  private refreshQueue: Promise<void> = Promise.resolve();
  private readonly cache: SessionDataCache;
  private skipNextSnapshotRefresh = false;
  private started = false;
  freshness: LiveFreshness = "reconnecting";

  constructor(
    private readonly client: DaemonClient,
    private readonly config: LiveDataConfig = defaultConfig,
    private readonly dependencies: LiveDataDependencies = {},
  ) {
    this.cache = dependencies.cache ?? new SessionDataCache();
  }

  readonly isFresh = (): boolean => this.freshness === "connected";

  readonly refresh = (models: readonly UpdateModel[] = ALL_MODELS): void => {
    this.queueRefresh({ cursor: "", models: [...models] }, this.config.invalidationWindowMs);
  };

  private queueRefresh(invalidation: ModelInvalidation, delay: number): void {
    this.invalidationRevision += 1;
    this.pendingInvalidations.push(copyInvalidation(invalidation));
    if (delay === 0) {
      this.clearInvalidationTimer();
      if (!this.invalidationsPaused) {
        void this.flushInvalidations();
      }
      return;
    }
    this.scheduleInvalidationFlush(delay);
  }

  readonly subscribe = (
    models: readonly UpdateModel[],
    listener: ModelListener,
    scope?: LiveDataScope,
  ): (() => void) => {
    const subscription = { listener, models: new Set(models), scope };
    this.listeners.add(subscription);
    if (!this.started || this.freshness !== "reconnecting" || this.cursor) {
      listener(new Set(models), "initial");
    }
    return () => this.listeners.delete(subscription);
  };

  subscribeState(listener: StateListener): () => void {
    this.stateListeners.add(listener);
    listener(this.freshness);
    return () => this.stateListeners.delete(listener);
  }

  start(): void {
    if (this.started) {
      return;
    }
    this.started = true;
    this.invalidationsPaused =
      !navigator.onLine || document.visibilityState === "hidden";
    this.cursor = window.sessionStorage.getItem(CURSOR_STORAGE_KEY) ?? undefined;
    window.addEventListener("online", this.onOnline);
    window.addEventListener("offline", this.onOffline);
    document.addEventListener("visibilitychange", this.onVisibilityChange);
    if (!navigator.onLine) {
      this.setFreshness("offline");
      return;
    }
    if (document.visibilityState === "hidden") {
      this.setFreshness("stale");
      return;
    }
    this.connect("initial");
  }

  stop(): void {
    if (!this.started) {
      return;
    }
    this.started = false;
    window.removeEventListener("online", this.onOnline);
    window.removeEventListener("offline", this.onOffline);
    document.removeEventListener("visibilitychange", this.onVisibilityChange);
    this.closeConnection("provider-stop");
    this.clearReconnectTimer();
    this.clearPollingTimer();
    this.clearInvalidationTimer();
    this.pendingInvalidations.length = 0;
  }

  private readonly onOnline = (): void => {
    if (!this.started || document.visibilityState === "hidden") {
      return;
    }
    this.invalidationsPaused = false;
    this.failureCount = 0;
    this.connect("online");
    this.resumeInvalidations();
  };

  private readonly onOffline = (): void => {
    if (!this.started) {
      return;
    }
    this.invalidationsPaused = true;
    this.closeConnection("offline");
    this.clearReconnectTimer();
    this.clearPollingTimer();
    this.clearInvalidationTimer();
    this.setFreshness("offline");
  };

  private readonly onVisibilityChange = (): void => {
    if (!this.started) {
      return;
    }
    if (document.visibilityState === "hidden") {
      this.invalidationsPaused = true;
      this.closeConnection("visibility-hidden");
      this.clearReconnectTimer();
      this.clearPollingTimer();
      this.clearInvalidationTimer();
      this.setFreshness("stale");
      return;
    }
    if (!navigator.onLine) {
      this.setFreshness("offline");
      return;
    }
    this.invalidationsPaused = false;
    this.failureCount = 0;
    this.connect("visibility-visible");
    this.resumeInvalidations();
  };

  private connect(cause: string, delayMs?: number): void {
    if (!this.started || !navigator.onLine || document.visibilityState === "hidden") {
      return;
    }
    if (cause !== "initial") {
      this.dependencies.diagnostics?.recordSSE({ event: "reconnect", cause, delayMs });
    }
    this.clearReconnectTimer();
    this.closeConnection("replaced");
    const generation = this.generation;
    const controller = new AbortController();
    this.connectController = controller;
    void this.consumeStream(generation, controller, cause);
  }

  private async consumeStream(
    generation: number,
    controller: AbortController,
    cause: string,
  ): Promise<void> {
    let stream: DaemonEventStream | undefined;
    let receivedEvent = false;
    const resumeCursor = this.cursor;
    try {
      stream = await this.client.connectEvents(
        resumeCursor ? { cursor: resumeCursor } : undefined,
        { signal: controller.signal },
      );
      if (!this.isCurrent(generation, controller)) {
        stream.close();
        return;
      }
      this.activeStream = stream;
      this.dependencies.diagnostics?.recordSSE({ event: "connect", cause });
      this.clearPollingTimer();
      this.setFreshness(resumeCursor ? "connected" : "stale");
      this.skipNextSnapshotRefresh = !resumeCursor;
      if (!resumeCursor) {
        this.queueRefresh({ cursor: "", models: ALL_MODELS }, 0);
      }

      for await (const event of stream) {
        if (!this.isCurrent(generation, controller)) {
          return;
        }
        receivedEvent = true;
        this.failureCount = 0;
        this.applyEvent(event);
      }
      if (this.isCurrent(generation, controller)) {
        this.handleDisconnect("stream-ended");
      }
    } catch (error) {
      if (!this.isCurrent(generation, controller)) {
        return;
      }
      if (isStaleCursorError(error)) {
        this.recoverStaleCursor();
        return;
      }
      if (receivedEvent) {
        this.failureCount = 0;
      }
      this.handleDisconnect("stream-error");
    } finally {
      if (this.activeStream === stream) {
        this.activeStream = undefined;
      }
      stream?.close();
    }
  }

  private applyEvent(event: DaemonUpdateEvent): void {
    if (event.type === "heartbeat" || this.hasApplied(event.id)) {
      return;
    }
    this.rememberEvent(event.id);
    this.cursor = event.id;
    window.sessionStorage.setItem(CURSOR_STORAGE_KEY, event.id);
    if (event.type === "snapshot" && this.skipNextSnapshotRefresh) {
      this.skipNextSnapshotRefresh = false;
      return;
    }
    this.cache.invalidate(event.data);
    this.queueRefresh(event.data, this.config.invalidationWindowMs);
  }

  private hasApplied(id: string): boolean {
    if (this.seenEventIds.has(id) || id === this.cursor) {
      return true;
    }
    const current = parseCursor(this.cursor);
    const candidate = parseCursor(id);
    return (
      current !== undefined &&
      candidate !== undefined &&
      current.session === candidate.session &&
      candidate.sequence <= current.sequence
    );
  }

  private rememberEvent(id: string): void {
    const current = parseCursor(this.cursor);
    const candidate = parseCursor(id);
    if (current && candidate && current.session !== candidate.session) {
      this.seenEventIds.clear();
      this.seenEventOrder.length = 0;
    }
    this.seenEventIds.add(id);
    this.seenEventOrder.push(id);
    if (this.seenEventOrder.length > SEEN_EVENT_LIMIT) {
      const expired = this.seenEventOrder.shift();
      if (expired) {
        this.seenEventIds.delete(expired);
      }
    }
  }

  private recoverStaleCursor(): void {
    this.closeConnection("stale-cursor");
    this.cursor = undefined;
    this.seenEventIds.clear();
    this.seenEventOrder.length = 0;
    window.sessionStorage.removeItem(CURSOR_STORAGE_KEY);
    this.failureCount = 0;
    this.setFreshness("stale");
    this.scheduleReconnect(0, "stale-cursor");
  }

  private handleDisconnect(cause: string): void {
    this.closeConnection(cause);
    if (!navigator.onLine) {
      this.clearPollingTimer();
      this.setFreshness("offline");
      return;
    }
    this.failureCount += 1;
    if (this.failureCount >= this.config.failuresBeforePolling) {
      this.startPollingFallback();
    } else {
      this.setFreshness("reconnecting");
    }
    const exponent = Math.max(0, this.failureCount - 1);
    const delay = Math.min(
      this.config.reconnectBaseDelayMs * 2 ** exponent,
      this.config.reconnectMaxDelayMs,
    );
    this.scheduleReconnect(delay, cause);
  }

  private startPollingFallback(): void {
    this.setFreshness("polling-fallback");
    if (this.polling) {
      return;
    }
    this.polling = true;
    void this.runPollingCycle();
  }

  private async runPollingCycle(): Promise<void> {
    await this.runRefresh([{ cursor: "", models: ALL_MODELS }]);
    if (!this.polling || !this.started) {
      return;
    }
    this.pollingTimer = setTimeout(() => {
      this.pollingTimer = undefined;
      void this.runPollingCycle();
    }, this.config.pollingIntervalMs);
  }

  private scheduleReconnect(delay: number, cause: string): void {
    this.clearReconnectTimer();
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = undefined;
      this.connect(cause, delay);
    }, delay);
  }

  private closeConnection(cause: string): void {
    const connected = this.activeStream !== undefined;
    this.generation += 1;
    this.connectController?.abort();
    this.connectController = undefined;
    this.activeStream?.close();
    this.activeStream = undefined;
    if (connected) {
      this.dependencies.diagnostics?.recordSSE({ event: "disconnect", cause });
    }
  }

  private clearReconnectTimer(): void {
    if (this.reconnectTimer !== undefined) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = undefined;
    }
  }

  private clearPollingTimer(): void {
    this.polling = false;
    if (this.pollingTimer !== undefined) {
      clearTimeout(this.pollingTimer);
      this.pollingTimer = undefined;
    }
  }

  private clearInvalidationTimer(): void {
    if (this.invalidationTimer !== undefined) {
      clearTimeout(this.invalidationTimer);
      this.invalidationTimer = undefined;
    }
  }

  private scheduleInvalidationFlush(delay: number): void {
    if (this.invalidationsPaused || this.invalidationTimer !== undefined) {
      return;
    }
    this.invalidationTimer = setTimeout(() => {
      this.invalidationTimer = undefined;
      void this.flushInvalidations();
    }, delay);
  }

  private async flushInvalidations(): Promise<void> {
    if (this.invalidationFlush) {
      return this.invalidationFlush;
    }
    const flush = this.drainInvalidations().finally(() => {
      if (this.invalidationFlush === flush) {
        this.invalidationFlush = undefined;
        if (
          !this.invalidationsPaused &&
          this.pendingInvalidations.length > 0 &&
          this.invalidationTimer === undefined
        ) {
          void this.flushInvalidations();
        }
      }
    });
    this.invalidationFlush = flush;
    return flush;
  }

  private async drainInvalidations(): Promise<void> {
    while (this.pendingInvalidations.length > 0) {
      if (this.invalidationsPaused) {
        return;
      }
      const revision = this.invalidationRevision;
      const invalidations = this.pendingInvalidations.splice(0);
      const stream = this.activeStream;
      const restoreConnected = stream !== undefined;
      if (restoreConnected) {
        this.setFreshness("stale");
      }
      const refreshed = await this.runRefresh(invalidations);
      if (!this.started) {
        return;
      }
      if (!refreshed) {
        this.invalidationRevision += 1;
        this.pendingInvalidations.unshift(...invalidations);
        this.scheduleInvalidationFlush(this.config.pollingIntervalMs);
        return;
      }
      if (this.invalidationTimer !== undefined) {
        return;
      }
      if (
        restoreConnected &&
        refreshed &&
        stream === this.activeStream &&
        this.pendingInvalidations.length === 0 &&
        revision === this.invalidationRevision
      ) {
        this.setFreshness("connected");
      }
    }
  }

  private resumeInvalidations(): void {
    if (this.pendingInvalidations.length > 0) {
      this.scheduleInvalidationFlush(0);
    }
  }

  private runRefresh(invalidations: readonly ModelInvalidation[]): Promise<boolean> {
    const refresh = this.refreshQueue.then(() => this.notifyListeners(invalidations));
    this.refreshQueue = refresh.then(
      () => undefined,
      () => undefined,
    );
    return refresh;
  }

  private async notifyListeners(invalidations: readonly ModelInvalidation[]): Promise<boolean> {
    const refreshes: Promise<boolean | void>[] = [];
    for (const subscription of this.listeners) {
      const models = new Set<UpdateModel>();
      for (const invalidation of invalidations) {
        if (!matchesScope(invalidation, subscription.scope)) {
          continue;
        }
        for (const model of invalidation.models) {
          if (subscription.models.has(model)) {
            models.add(model);
          }
        }
      }
      if (models.size > 0) {
        refreshes.push(Promise.resolve(subscription.listener(models, "refresh")));
      }
    }
    const results = await Promise.all(refreshes);
    return results.every((result) => result !== false);
  }

  private setFreshness(freshness: LiveFreshness): void {
    if (this.freshness === freshness) {
      return;
    }
    this.freshness = freshness;
    for (const listener of this.stateListeners) {
      listener(freshness);
    }
  }

  private isCurrent(generation: number, controller: AbortController): boolean {
    return this.started && generation === this.generation && !controller.signal.aborted;
  }
}

function isStaleCursorError(error: unknown): boolean {
  return (
    error instanceof DaemonApiError &&
    (error.code === "stale_cursor" || error.code === "invalid_cursor")
  );
}

function copyInvalidation(invalidation: ModelInvalidation): ModelInvalidation {
  return {
    cursor: invalidation.cursor,
    models: [...invalidation.models],
    ...(invalidation.runIds ? { runIds: [...invalidation.runIds] } : {}),
    ...(invalidation.workflows
      ? { workflows: invalidation.workflows.map((workflow) => ({ ...workflow })) }
      : {}),
  };
}

function matchesScope(
  invalidation: ModelInvalidation,
  scope: LiveDataScope | undefined,
): boolean {
  if (!scope) {
    return true;
  }
  if (
    scope.runId &&
    invalidation.runIds &&
    invalidation.runIds.length > 0 &&
    !invalidation.runIds.includes(scope.runId)
  ) {
    return false;
  }
  if (
    (scope.gaggle || scope.workflow) &&
    invalidation.workflows &&
    invalidation.workflows.length > 0 &&
    !invalidation.workflows.some(
      (workflow) =>
        (!scope.gaggle || workflow.gaggle === scope.gaggle) &&
        (!scope.workflow || workflow.name === scope.workflow),
    )
  ) {
    return false;
  }
  return true;
}

function parseCursor(cursor: string | undefined):
  | {
      sequence: bigint;
      session: string;
    }
  | undefined {
  if (!cursor) {
    return undefined;
  }
  const separator = cursor.lastIndexOf(":");
  if (separator <= 0) {
    return undefined;
  }
  const rawSequence = cursor.slice(separator + 1);
  if (!/^\d+$/.test(rawSequence)) {
    return undefined;
  }
  return {
    sequence: BigInt(rawSequence),
    session: cursor.slice(0, separator),
  };
}
