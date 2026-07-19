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
  UpdateModel,
} from "./api/types";

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
) => boolean | void | Promise<boolean | void>;
type StateListener = (state: LiveFreshness) => void;

interface LiveDataContextValue {
  freshness: LiveFreshness;
  isFresh: () => boolean;
  refresh: (models?: readonly UpdateModel[]) => void;
  subscribe: (models: readonly UpdateModel[], listener: ModelListener) => () => void;
}

const LiveDataContext = createContext<LiveDataContextValue | undefined>(undefined);

export function LiveDataProvider({
  children,
  client,
  config,
}: {
  children: ReactNode;
  client: DaemonClient;
  config?: Partial<LiveDataConfig>;
}) {
  const controller = useMemo(
    () => new LiveDataController(client, { ...defaultConfig, ...config }),
    [
      client,
      config?.failuresBeforePolling,
      config?.invalidationWindowMs,
      config?.pollingIntervalMs,
      config?.reconnectBaseDelayMs,
      config?.reconnectMaxDelayMs,
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
      freshness,
      isFresh: controller.isFresh,
      refresh: controller.refresh,
      subscribe: controller.subscribe,
    }),
    [controller, freshness],
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
  }>();
  private readonly stateListeners = new Set<StateListener>();
  private readonly pendingModels = new Set<UpdateModel>();
  private readonly seenEventIds = new Set<string>();
  private readonly seenEventOrder: string[] = [];
  private activeStream: DaemonEventStream | undefined;
  private connectController: AbortController | undefined;
  private cursor: string | undefined;
  private failureCount = 0;
  private generation = 0;
  private invalidationRevision = 0;
  private invalidationTimer: ReturnType<typeof setTimeout> | undefined;
  private polling = false;
  private pollingTimer: ReturnType<typeof setTimeout> | undefined;
  private reconnectTimer: ReturnType<typeof setTimeout> | undefined;
  private refreshQueue: Promise<void> = Promise.resolve();
  private started = false;
  freshness: LiveFreshness = "reconnecting";

  constructor(
    private readonly client: DaemonClient,
    private readonly config: LiveDataConfig = defaultConfig,
  ) {}

  readonly isFresh = (): boolean => this.freshness === "connected";

  readonly refresh = (models: readonly UpdateModel[] = ALL_MODELS): void => {
    this.queueRefresh(models, this.config.invalidationWindowMs);
  };

  private queueRefresh(models: readonly UpdateModel[], delay: number): void {
    this.invalidationRevision += 1;
    for (const model of models) {
      this.pendingModels.add(model);
    }
    if (delay === 0) {
      this.clearInvalidationTimer();
      void this.flushInvalidations();
      return;
    }
    this.scheduleInvalidationFlush(delay);
  }

  readonly subscribe = (
    models: readonly UpdateModel[],
    listener: ModelListener,
  ): (() => void) => {
    const subscription = { listener, models: new Set(models) };
    this.listeners.add(subscription);
    if (!this.started || this.freshness !== "reconnecting") {
      listener(new Set(models));
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
    this.connect();
  }

  stop(): void {
    if (!this.started) {
      return;
    }
    this.started = false;
    window.removeEventListener("online", this.onOnline);
    window.removeEventListener("offline", this.onOffline);
    document.removeEventListener("visibilitychange", this.onVisibilityChange);
    this.closeConnection();
    this.clearReconnectTimer();
    this.clearPollingTimer();
    this.clearInvalidationTimer();
    this.pendingModels.clear();
  }

  private readonly onOnline = (): void => {
    if (!this.started || document.visibilityState === "hidden") {
      return;
    }
    this.failureCount = 0;
    this.connect();
  };

  private readonly onOffline = (): void => {
    if (!this.started) {
      return;
    }
    this.closeConnection();
    this.clearReconnectTimer();
    this.clearPollingTimer();
    this.clearInvalidationTimer();
    this.pendingModels.clear();
    this.setFreshness("offline");
  };

  private readonly onVisibilityChange = (): void => {
    if (!this.started) {
      return;
    }
    if (document.visibilityState === "hidden") {
      this.closeConnection();
      this.clearReconnectTimer();
      this.clearPollingTimer();
      this.clearInvalidationTimer();
      this.pendingModels.clear();
      this.setFreshness("stale");
      return;
    }
    if (!navigator.onLine) {
      this.setFreshness("offline");
      return;
    }
    this.failureCount = 0;
    this.connect();
  };

  private connect(): void {
    if (!this.started || !navigator.onLine || document.visibilityState === "hidden") {
      return;
    }
    this.clearReconnectTimer();
    this.closeConnection();
    const generation = this.generation;
    const controller = new AbortController();
    this.connectController = controller;
    void this.consumeStream(generation, controller);
  }

  private async consumeStream(generation: number, controller: AbortController): Promise<void> {
    let stream: DaemonEventStream | undefined;
    let receivedEvent = false;
    try {
      stream = await this.client.connectEvents(
        this.cursor ? { cursor: this.cursor } : undefined,
        { signal: controller.signal },
      );
      if (!this.isCurrent(generation, controller)) {
        stream.close();
        return;
      }
      this.activeStream = stream;
      this.clearPollingTimer();
      this.setFreshness("stale");
      this.queueRefresh(ALL_MODELS, 0);

      for await (const event of stream) {
        if (!this.isCurrent(generation, controller)) {
          return;
        }
        receivedEvent = true;
        this.failureCount = 0;
        this.applyEvent(event);
      }
      if (this.isCurrent(generation, controller)) {
        this.handleDisconnect();
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
      this.handleDisconnect();
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
    if (event.type === "invalidate") {
      this.refresh(event.data.models);
    }
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
    this.closeConnection();
    this.cursor = undefined;
    this.seenEventIds.clear();
    this.seenEventOrder.length = 0;
    window.sessionStorage.removeItem(CURSOR_STORAGE_KEY);
    this.failureCount = 0;
    this.setFreshness("stale");
    this.scheduleReconnect(0);
  }

  private handleDisconnect(): void {
    this.closeConnection();
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
    this.scheduleReconnect(delay);
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
    await this.runRefresh(new Set(ALL_MODELS));
    if (!this.polling || !this.started) {
      return;
    }
    this.pollingTimer = setTimeout(() => {
      this.pollingTimer = undefined;
      void this.runPollingCycle();
    }, this.config.pollingIntervalMs);
  }

  private scheduleReconnect(delay: number): void {
    this.clearReconnectTimer();
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = undefined;
      this.connect();
    }, delay);
  }

  private closeConnection(): void {
    this.generation += 1;
    this.connectController?.abort();
    this.connectController = undefined;
    this.activeStream?.close();
    this.activeStream = undefined;
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
    if (this.invalidationTimer !== undefined) {
      return;
    }
    this.invalidationTimer = setTimeout(() => void this.flushInvalidations(), delay);
  }

  private async flushInvalidations(): Promise<void> {
    this.invalidationTimer = undefined;
    if (this.pendingModels.size === 0) {
      return;
    }
    const revision = this.invalidationRevision;
    const models = new Set(this.pendingModels);
    this.pendingModels.clear();
    const stream = this.activeStream;
    const restoreConnected = stream !== undefined;
    if (restoreConnected) {
      this.setFreshness("stale");
    }
    const refreshed = await this.runRefresh(models);
    if (!this.started) {
      return;
    }
    if (!refreshed && stream === this.activeStream) {
      this.invalidationRevision += 1;
      for (const model of models) {
        this.pendingModels.add(model);
      }
      this.scheduleInvalidationFlush(this.config.pollingIntervalMs);
      return;
    }
    if (
      restoreConnected &&
      refreshed &&
      stream === this.activeStream &&
      this.pendingModels.size === 0 &&
      revision === this.invalidationRevision
    ) {
      this.setFreshness("connected");
    }
  }

  private runRefresh(models: ReadonlySet<UpdateModel>): Promise<boolean> {
    const refresh = this.refreshQueue.then(() => this.notifyListeners(models));
    this.refreshQueue = refresh.then(
      () => undefined,
      () => undefined,
    );
    return refresh;
  }

  private async notifyListeners(models: ReadonlySet<UpdateModel>): Promise<boolean> {
    const refreshes: Promise<boolean | void>[] = [];
    for (const subscription of this.listeners) {
      if ([...subscription.models].some((model) => models.has(model))) {
        refreshes.push(Promise.resolve(subscription.listener(models)));
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
