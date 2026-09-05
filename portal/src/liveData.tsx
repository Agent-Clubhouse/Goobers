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

export interface LiveDataSSEFailure {
  cause: string;
  endpoint: string;
  result?: string;
}

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
  /**
   * How many distinct pending invalidations may be retained before the queue is
   * replaced by a single all-model refresh.
   *
   * Coalescing already collapses repeats of the same model/scope, but a long
   * read outage over a healthy stream still accumulates one entry per distinct
   * entity touched. Past this point replaying them individually costs more than
   * refetching everything, so the queue is discarded in favour of one complete
   * snapshot refresh — the same recovery a cold connect performs.
   */
  maxPendingInvalidations: number;
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
  maxPendingInvalidations: 64,
};

type ModelListener = (
  models: ReadonlySet<UpdateModel>,
  reason: "initial" | "refresh",
  invalidations?: readonly ModelInvalidation[],
) => boolean | void | Promise<boolean | void>;
type StateListener = (state: LiveFreshness, failure?: LiveDataSSEFailure) => void;

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
  lastSSEFailure?: LiveDataSSEFailure;
  /** How current the data is. Independent of `freshness`. */
  dataFreshness: DataFreshness;
  /** Called by the HTTP client for every response carrying a readState. */
  reportReadState: (state: ReadState) => void;
  isFresh: () => boolean;
  refresh: (models?: readonly UpdateModel[]) => void;
  retryConnection: () => void;
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
      config?.maxPendingInvalidations,
      diagnostics,
    ],
  );
  const [freshness, setFreshness] = useState<LiveFreshness>(() => controller.freshness);
  const [lastSSEFailure, setLastSSEFailure] = useState<LiveDataSSEFailure | undefined>(
    () => controller.lastSSEFailure,
  );
  const [dataFreshness, setDataFreshness] = useState<DataFreshness>({ kind: "unknown" });

  const reportReadState = useCallback((state: ReadState) => {
    setDataFreshness(deriveDataFreshness(state));
  }, []);

  // Registered in a layout effect so the sink is live before the first paint,
  // and torn down with the provider — a stale sink would keep a dead
  // component's setState alive across a provider swap.
  useLayoutEffect(() => setReadStateSink(reportReadState), [reportReadState]);

  useLayoutEffect(() => {
    const unsubscribe = controller.subscribeState((nextFreshness, failure) => {
      setFreshness(nextFreshness);
      setLastSSEFailure(failure);
    });
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
      lastSSEFailure,
      dataFreshness,
      reportReadState,
      isFresh: controller.isFresh,
      refresh: controller.refresh,
      retryConnection: controller.retryConnection,
      subscribe: controller.subscribe,
    }),
    [cache, controller, dataFreshness, freshness, lastSSEFailure, reportReadState],
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
  /**
   * Pending work keyed by model set and scope, in the order it was first seen.
   *
   * A map rather than a list because the queue is drained as a set of refresh
   * requests, not replayed as a log: two invalidations naming the same models
   * and the same entities produce exactly the same refetch, so retaining both
   * grew memory and recovery cost without changing the outcome (#2460).
   */
  private pendingInvalidations = new Map<string, ModelInvalidation>();
  /** Cursor of the most recently queued invalidation, for a collapsed refresh. */
  private lastQueuedCursor = "";
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
  lastSSEFailure: LiveDataSSEFailure | undefined;
  // Tracks the failure object last broadcast to state listeners, so a repeat
  // failure while freshness holds steady (e.g. a second SSE drop while
  // already in polling-fallback) still reaches subscribers — see
  // setFreshness below.
  private lastNotifiedSSEFailure: LiveDataSSEFailure | undefined;
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

  readonly retryConnection = (): void => {
    this.clearPollingTimer();
    this.clearReconnectTimer();
    this.clearInvalidationTimer();
    this.pendingInvalidations.clear();
    this.lastQueuedCursor = "";
    this.cursor = undefined;
    this.failureCount = 0;
    this.refreshFailureCount = 0;
    this.seenEventIds.clear();
    this.seenEventOrder.length = 0;
    this.lastSSEFailure = undefined;
    window.sessionStorage.removeItem(CURSOR_STORAGE_KEY);
    this.closeConnection("manual-retry");
    this.setFreshness("reconnecting");
    this.connect("manual-retry");
  };

  private queueRefresh(invalidation: ModelInvalidation, delay: number): void {
    this.invalidationRevision += 1;
    this.enqueueInvalidations([invalidation], "back");
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
    listener(this.freshness, this.lastSSEFailure);
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
    this.clearConnectWatchdog();
    this.clearIdleWatchdog();
    this.closeConnection("provider-stop");
    this.clearReconnectTimer();
    this.clearPollingTimer();
    this.clearInvalidationTimer();
    this.cache.dispose();
    this.pendingInvalidations.clear();
    this.lastQueuedCursor = "";
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
      this.lastSSEFailure = undefined;
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
      this.handleDisconnect("stream-error", describeSSEFailure(error));
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
      this.handleDisconnect("connect-timeout", {
        cause: "connect-timeout",
        endpoint: "/api/v1/events",
        result: "timeout",
      });
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
      this.handleDisconnect("stream-idle", {
        cause: "stream-idle",
        endpoint: "/api/v1/events",
        result: "idle-timeout",
      });
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

  private handleDisconnect(cause: string, failure?: LiveDataSSEFailure): void {
    this.lastSSEFailure = failure ?? { cause, endpoint: "/api/v1/events" };
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

  /**
   * Merges invalidations into the pending queue, coalescing equivalent work.
   *
   * `"front"` re-queues a batch whose refresh failed ahead of anything that
   * arrived while it was in flight, so retry order still matches arrival order
   * without the batch being retained separately from its own duplicates.
   */
  private enqueueInvalidations(
    incoming: readonly ModelInvalidation[],
    position: "back" | "front",
  ): void {
    if (position === "back") {
      for (const invalidation of incoming) {
        if (invalidation.cursor) {
          this.lastQueuedCursor = invalidation.cursor;
        }
        mergeInvalidation(this.pendingInvalidations, invalidation);
      }
    } else {
      const pending = new Map<string, ModelInvalidation>();
      for (const invalidation of incoming) {
        mergeInvalidation(pending, invalidation);
      }
      for (const invalidation of this.pendingInvalidations.values()) {
        mergeInvalidation(pending, invalidation);
      }
      this.pendingInvalidations = pending;
    }
    this.collapseOversizedQueue();
  }

  private collapseOversizedQueue(): void {
    if (this.pendingInvalidations.size <= this.config.maxPendingInvalidations) {
      return;
    }
    const complete: ModelInvalidation = {
      cursor: this.lastQueuedCursor,
      models: [...ALL_MODELS],
    };
    this.pendingInvalidations = new Map([[invalidationKey(complete), complete]]);
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
          this.pendingInvalidations.size > 0 &&
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
    while (this.pendingInvalidations.size > 0) {
      if (this.invalidationsPaused) {
        return;
      }
      const revision = this.invalidationRevision;
      const invalidations = [...this.pendingInvalidations.values()];
      this.pendingInvalidations = new Map();
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
        this.enqueueInvalidations(invalidations, "front");
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
        this.pendingInvalidations.size === 0 &&
        revision === this.invalidationRevision
      ) {
        this.setFreshness("connected");
      }
    }
  }

  private resumeInvalidations(): void {
    if (this.pendingInvalidations.size > 0) {
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
    // A repeated SSE failure while the freshness value doesn't change (e.g. a
    // second disconnect while already in polling-fallback) still needs to
    // reach subscribers, since it carries new failure details — so the
    // no-op guard only fires when BOTH the freshness value and the failure
    // are unchanged from what was last broadcast.
    if (
      this.freshness === freshness &&
      this.lastSSEFailure === this.lastNotifiedSSEFailure
    ) {
      return;
    }
    this.freshness = freshness;
    this.lastNotifiedSSEFailure = this.lastSSEFailure;
    for (const listener of this.stateListeners) {
      listener(freshness, this.lastSSEFailure);
    }
  }

  private isCurrent(generation: number, controller: AbortController): boolean {
    return this.started && generation === this.generation && !controller.signal.aborted;
  }
}

function describeSSEFailure(error: unknown): LiveDataSSEFailure {
  const endpoint = "/api/v1/events";
  if (error instanceof DaemonApiError) {
    return { cause: "stream-error", endpoint, result: `HTTP ${error.status}` };
  }
  if (error instanceof Error) {
    return { cause: "stream-error", endpoint, result: error.name };
  }
  return { cause: "stream-error", endpoint };
}

function isStaleCursorError(error: unknown): boolean {
  return (
    error instanceof DaemonApiError &&
    (error.code === "stale_cursor" || error.code === "invalid_cursor")
  );
}

/**
 * Identifies the refetch an invalidation asks for.
 *
 * Cursor is deliberately excluded: two invalidations naming the same models and
 * the same entities request identical work regardless of which event produced
 * them, and it is that equivalence the pending queue coalesces on.
 */
function invalidationKey(invalidation: ModelInvalidation): string {
  const models = [...invalidation.models].sort().join(",");
  const runIds = [...(invalidation.runIds ?? [])].sort().join(",");
  const workflows = (invalidation.workflows ?? [])
    .map((workflow) => `${workflow.gaggle}/${workflow.name}`)
    .sort()
    .join(",");
  return `${models}|${runIds}|${workflows}`;
}

/** Adds an invalidation, keeping the position of any equivalent entry. */
function mergeInvalidation(
  pending: Map<string, ModelInvalidation>,
  invalidation: ModelInvalidation,
): void {
  const key = invalidationKey(invalidation);
  const existing = pending.get(key);
  if (existing) {
    // Same work, newer cursor: the entry keeps its queue position so ordering
    // still follows first arrival.
    existing.cursor = invalidation.cursor;
    return;
  }
  pending.set(key, copyInvalidation(invalidation));
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
