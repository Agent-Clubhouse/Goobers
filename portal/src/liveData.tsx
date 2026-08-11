import {
  createContext,
  useContext,
  useLayoutEffect,
  useMemo,
  useState,
  type ReactNode,
  useCallback,
} from "react";
import { DaemonApiError } from "./api/errors";
import type {
  DaemonClient,
  DaemonEventStream,
  DaemonUpdateEvent,
  ModelInvalidation,
  UpdateModel,
  ReadState,
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
  /** How long the initial SSE handshake may remain pending. */
  connectTimeoutMs: number;
  /**
   * How long the stream may go completely silent before it is treated as dead.
   *
   * The daemon emits a heartbeat every 15s specifically so a client can detect
   * a dead-but-open connection. This is the deadline that makes that heartbeat
   * mean something: without it the client parsed heartbeats and threw them
   * away, so it could not detect the exact failure they exist to expose.
   *
   * Set above 2x the server interval so a single dropped or delayed heartbeat
   * does not tear down a healthy stream, but low enough that a wall-mounted
   * dashboard notices within a minute.
   */
  streamIdleTimeoutMs: number;
  /**
   * How long a connection must survive before its success clears the backoff.
   *
   * Receiving ANY event used to reset failureCount to 0. A buffering proxy or a
   * daemon in a restart loop accepts the request, flushes the initial snapshot,
   * then closes — which reset the count, so the delay never grew past the
   * 250ms base and the client reconnected four times a second indefinitely,
   * never reaching failuresBeforePolling. Time connected, not bytes received,
   * is what distinguishes a working stream from one that dies on arrival.
   */
  connectionSettledMs: number;
  failuresBeforePolling: number;
  pollingIntervalMs: number;
  /** Ceiling for the backoff applied to consecutively failing refreshes. */
  refreshMaxDelayMs: number;
}

const defaultConfig: LiveDataConfig = {
  invalidationWindowMs: 50,
  reconnectBaseDelayMs: 250,
  reconnectMaxDelayMs: 30_000,
  connectTimeoutMs: 10_000,
  streamIdleTimeoutMs: 45_000,
  connectionSettledMs: 10_000,
  failuresBeforePolling: 3,
  pollingIntervalMs: 5_000,
  refreshMaxDelayMs: 60_000,
};

type ModelListener = (
  models: ReadonlySet<UpdateModel>,
  reason: "initial" | "refresh",
  invalidations?: readonly ModelInvalidation[],
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

/**
 * How current the DATA is, as distinct from whether the socket is open.
 *
 * #1928: the existing indicator reports connection state — connected,
 * reconnecting, stale, offline, polling. That is not how current the data is,
 * and it is why an operator cannot distinguish "slow" from "broken". A stream
 * can be perfectly connected to a projector that is ten minutes behind, and a
 * stream can be reconnecting over data that is current to the second.
 */
export type DataFreshness =
  | { kind: "unknown" }
  | { kind: "current"; lagSeconds: number }
  | { kind: "lagging"; lagSeconds: number; degraded: readonly string[] }
  | {
      kind: "partial";
      lagSeconds: number;
      missing: readonly { name: string; reason: string; expectedBy: string }[];
    };

/**
 * Above this, data is described as lagging rather than current.
 *
 * Not zero: the reported lag is an upper bound that includes the repair sweep's
 * cycle age, so a perfectly healthy instance reports a few seconds at all times.
 * Treating that as "lagging" would make the warning state permanent and
 * therefore meaningless.
 */
const LAGGING_THRESHOLD_SECONDS = 30;

interface LiveDataContextValue {
  cache: SessionDataCache;
  freshness: LiveFreshness;
  /** How current the data is. Independent of `freshness`. */
  dataFreshness: DataFreshness;
  /** Called by the HTTP client for every response carrying a readState. */
  reportReadState: (state: ReadState) => void;
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
      config?.connectTimeoutMs,
      config?.streamIdleTimeoutMs,
      config?.connectionSettledMs,
      config?.refreshMaxDelayMs,
      diagnostics,
    ],
  );
  const [freshness, setFreshness] = useState<LiveFreshness>(() => controller.freshness);
  const [dataFreshness, setDataFreshness] = useState<DataFreshness>({ kind: "unknown" });

  const reportReadState = useCallback((state: ReadState) => {
    setDataFreshness(deriveDataFreshness(state));
  }, []);

  // Registered in a layout effect so the sink is live before the first paint,
  // and torn down with the provider — a stale sink would keep a dead
  // component's setState alive across a provider swap.
  useLayoutEffect(() => setReadStateSink(reportReadState), [reportReadState]);

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
      dataFreshness,
      reportReadState,
      isFresh: controller.isFresh,
      refresh: controller.refresh,
      subscribe: controller.subscribe,
    }),
    [cache, controller, dataFreshness, freshness, reportReadState],
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
  private connectTimer: ReturnType<typeof setTimeout> | undefined;
  /**
   * Fires when the stream has been silent past streamIdleTimeoutMs.
   *
   * Armed on connect and re-armed on EVERY frame including heartbeats — that is
   * the entire point. A heartbeat carries no data, so applyEvent still discards
   * it; what it now does is prove the connection is alive.
   */
  private idleTimer: ReturnType<typeof setTimeout> | undefined;
  private refreshFailureCount = 0;
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
    this.cache.dispose();
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
    let connectedAt = 0;
    const resumeCursor = this.cursor;
    const connectTimer = this.armConnectWatchdog(generation, controller);
    try {
      stream = await this.client.connectEvents(
        resumeCursor ? { cursor: resumeCursor } : undefined,
        { signal: controller.signal },
      );
      this.clearConnectWatchdog(connectTimer);
      if (!this.isCurrent(generation, controller)) {
        stream.close();
        return;
      }
      this.activeStream = stream;
      connectedAt = Date.now();
      this.armIdleWatchdog(generation, controller);
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
        // Re-arm on every frame, heartbeat included. A heartbeat is not data
        // and applyEvent still drops it, but it IS evidence the socket is
        // carrying bytes — which is the only thing that distinguishes a live
        // stream from a NAT rebind that left the connection open and silent.
        this.armIdleWatchdog(generation, controller);
        // The backoff clears only once the connection has lasted long enough to
        // count as working. Resetting on the first event let a proxy that
        // flushes the snapshot and closes hold the client at a 250ms retry
        // forever.
        if (connectedAt !== 0 && Date.now() - connectedAt >= this.config.connectionSettledMs) {
          this.failureCount = 0;
        }
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
      if (connectedAt !== 0 && Date.now() - connectedAt >= this.config.connectionSettledMs) {
        this.failureCount = 0;
      }
      this.handleDisconnect("stream-error");
    } finally {
      this.clearConnectWatchdog(connectTimer);
      this.clearIdleWatchdog();
      if (this.activeStream === stream) {
        this.activeStream = undefined;
      }
      stream?.close();
    }
  }

  private armConnectWatchdog(
    generation: number,
    controller: AbortController,
  ): ReturnType<typeof setTimeout> {
    this.clearConnectWatchdog();
    const timer = setTimeout(() => {
      if (!this.isCurrent(generation, controller)) {
        return;
      }
      this.dependencies.diagnostics?.recordSSE({
        event: "reconnect",
        cause: "connect-timeout",
        delayMs: this.config.connectTimeoutMs,
      });
      controller.abort();
      this.handleDisconnect("connect-timeout");
    }, this.config.connectTimeoutMs);
    this.connectTimer = timer;
    return timer;
  }

  private clearConnectWatchdog(expected?: ReturnType<typeof setTimeout>): void {
    if (expected !== undefined && this.connectTimer !== expected) {
      return;
    }
    if (this.connectTimer !== undefined) {
      clearTimeout(this.connectTimer);
      this.connectTimer = undefined;
    }
  }

  /**
   * Arms the silence deadline for the current connection.
   *
   * The client's only existing timeout is cleared as soon as response HEADERS
   * arrive (httpClient.ts), so the body read loop had no deadline at all: on a
   * silently-dropped TCP connection `reader.read()` neither resolves nor
   * rejects, freshness stayed "connected", no reconnect was scheduled, and the
   * polling fallback never engaged. The portal showed arbitrarily stale state
   * behind a green "live" indicator.
   */
  private armIdleWatchdog(generation: number, controller: AbortController): void {
    this.clearIdleWatchdog();
    this.idleTimer = setTimeout(() => {
      if (!this.isCurrent(generation, controller)) {
        return;
      }
      this.dependencies.diagnostics?.recordSSE({
        event: "reconnect",
        cause: "idle-timeout",
        delayMs: this.config.streamIdleTimeoutMs,
      });
      // Abort the read so the socket is actually released rather than leaked;
      // handleDisconnect then applies normal backoff, so a stream that keeps
      // going silent escalates to polling instead of thrashing.
      controller.abort();
      this.handleDisconnect("stream-idle");
    }, this.config.streamIdleTimeoutMs);
  }

  private clearIdleWatchdog(): void {
    if (this.idleTimer !== undefined) {
      clearTimeout(this.idleTimer);
      this.idleTimer = undefined;
    }
  }

  private applyEvent(event: DaemonUpdateEvent): void {
    if (event.type === "heartbeat" || this.hasApplied(event.id)) {
      return;
    }
    // An epoch change forces a SNAPSHOT, not a quiet cursor swap (#1930, §8.2).
    //
    // The store was rebuilt, so this client's view predates a generation it can
    // no longer reason about — its sequence numbers came from a different
    // AUTOINCREMENT. Adopting the new cursor and carrying on would leave it
    // following the new feed while holding data from before the rebuild, and
    // nothing would ever correct that: every subsequent event applies cleanly,
    // so the staleness is permanent and invisible.
    //
    // Detected here rather than only on reconnect because the server can hand a
    // new epoch to an ESTABLISHED stream — a rebuild does not require the
    // connection to drop.
    if (this.epochChanged(event.id)) {
      this.dependencies.diagnostics?.recordSSE({ event: "reconnect", cause: "epoch-changed" });
      this.rememberEvent(event.id);
      this.cursor = event.id;
      window.sessionStorage.setItem(CURSOR_STORAGE_KEY, event.id);
      // Everything, not just what the event names: the rebuild may have changed
      // any of it, and the event's entity list describes one transition rather
      // than the generation gap.
      // Evict everything: a rebuild may have changed any of it, and the
      // event's entity list describes one transition rather than a generation
      // gap. invalidate() with ALL_MODELS is the widest eviction the cache
      // exposes and is what a fresh connection already does.
      this.cache.invalidate({ cursor: event.id, models: ALL_MODELS });
      this.queueRefresh({ cursor: event.id, models: ALL_MODELS }, 0);
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

  /**
   * Reports whether an event's cursor names a different projection generation.
   *
   * Equality, never ordering (§4.2). Epochs are opaque: a rebuilt store mints a
   * fresh one with no relationship to the last, so any inequality is a new
   * generation. Comparing them with < would read meaning into a value that has
   * none.
   */
  private epochChanged(id: string): boolean {
    const current = parseCursor(this.cursor);
    const candidate = parseCursor(id);
    if (!current || !candidate) {
      return false;
    }
    return current.epoch !== candidate.epoch;
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
      current.epoch === candidate.epoch &&
      candidate.sequence <= current.sequence
    );
  }

  private rememberEvent(id: string): void {
    const current = parseCursor(this.cursor);
    const candidate = parseCursor(id);
    if (current && candidate && current.epoch !== candidate.epoch) {
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
    const refreshed = await this.runRefresh([{ cursor: "", models: ALL_MODELS }]);
    if (!this.polling || !this.started) {
      return;
    }
    // The fallback poll is the other path that hammers a degraded daemon: it is
    // only active because the event stream is already failing, so a failing poll
    // backs off too rather than re-firing every 5s (#1710).
    if (refreshed) {
      this.refreshFailureCount = 0;
    } else {
      this.refreshFailureCount += 1;
    }
    this.pollingTimer = setTimeout(() => {
      this.pollingTimer = undefined;
      void this.runPollingCycle();
    }, refreshed ? this.config.pollingIntervalMs : this.refreshRetryDelay());
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
    this.clearConnectWatchdog();
    // The watchdog belongs to the connection, so it dies with it. Without this
    // it outlives every teardown path — provider stop, offline, tab hidden,
    // reconnect — and fires against a generation that no longer exists, leaving
    // a timer pending after stop() and re-arming reconnects on a stopped
    // controller.
    this.clearIdleWatchdog();
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

  // A refresh fails precisely when the daemon is slow or erroring, so retrying
  // it at a flat pollingIntervalMs applied maximum request pressure exactly when
  // the backend could least absorb it: an Overview whose run queries time out
  // re-issued the whole snapshot every 5s indefinitely (#1710). Back off the way
  // the reconnect path already does, and reset on the first success.
  private refreshRetryDelay(): number {
    const exponent = Math.max(0, this.refreshFailureCount - 1);
    return Math.min(
      this.config.pollingIntervalMs * 2 ** exponent,
      this.config.refreshMaxDelayMs,
    );
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
        this.refreshFailureCount += 1;
        this.invalidationRevision += 1;
        this.pendingInvalidations.unshift(...invalidations);
        this.scheduleInvalidationFlush(this.refreshRetryDelay());
        return;
      }
      this.refreshFailureCount = 0;
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
      const matchingInvalidations = invalidations.filter((invalidation) =>
        matchesScope(invalidation, subscription.scope),
      );
      for (const invalidation of matchingInvalidations) {
        for (const model of invalidation.models) {
          if (subscription.models.has(model)) {
            models.add(model);
          }
        }
      }
      if (models.size > 0) {
        refreshes.push(
          Promise.resolve(subscription.listener(models, "refresh", matchingInvalidations)),
        );
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

/**
 * Parses a stream cursor.
 *
 * Two forms, because both are on the wire during the transition (#1929): the
 * change feed emits `<schemaVersion>:<epoch>:<seq>`, and the filesystem poller
 * — still the source for topologies with no read model — emits
 * `<session>:<seq>`.
 *
 * `epoch` is whatever identifies the generation in either form: the epoch
 * proper, or the poller's random per-process session id. That is the right
 * unification rather than a convenient one — both answer "is this the same
 * generation of sequence numbers", which is the only question the client asks
 * of them. Splitting on the LAST colon keeps the three-part form working
 * without special-casing.
 */
function parseCursor(cursor: string | undefined):
  | {
      sequence: bigint;
      epoch: string;
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
    epoch: cursor.slice(0, separator),
  };
}

/**
 * Maps a server envelope onto the three states the UI renders.
 *
 * Three, not four: §11A requires "no fourth indefinite state". `unknown` is not
 * a fourth — it is the absence of any envelope at all (a standalone read, an
 * older daemon), and it renders as the connection indicator alone rather than
 * as a freshness claim.
 *
 * `partial` outranks `lagging` because a named missing partition is a stronger
 * statement than a number: the user needs to know something is absent before
 * they need to know how old the rest is.
 */
/**
 * The bridge between the HTTP client and the provider.
 *
 * The client is a module-level singleton constructed at import time
 * (App.tsx), before any provider exists, so it cannot be handed a callback at
 * construction. A registered sink is the smallest thing that connects them
 * without making the client depend on React or the provider depend on a
 * specific client instance.
 *
 * Unregistered by default: in tests that render without a provider, and in the
 * standalone build, reports are simply dropped.
 */
let readStateSink: ((state: ReadState) => void) | undefined;

/** Registers the provider's reporter. Returns an unregister function. */
export function setReadStateSink(sink: (state: ReadState) => void): () => void {
  readStateSink = sink;
  return () => {
    if (readStateSink === sink) {
      readStateSink = undefined;
    }
  };
}

/** Called by the HTTP client for every response carrying a readState. */
export function publishReadState(state: ReadState): void {
  readStateSink?.(state);
}

export function deriveDataFreshness(state: ReadState): DataFreshness {
  if (state.completeness === "partial" && state.missing && state.missing.length > 0) {
    return { kind: "partial", lagSeconds: state.lagSeconds, missing: state.missing };
  }
  if (state.lagSeconds > LAGGING_THRESHOLD_SECONDS || state.degraded.length > 0) {
    return { kind: "lagging", lagSeconds: state.lagSeconds, degraded: state.degraded };
  }
  return { kind: "current", lagSeconds: state.lagSeconds };
}
