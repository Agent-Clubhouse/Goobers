import { act, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { DaemonApiError, DaemonUnavailableError } from "./api/errors";
import { FixtureDaemonClient, type DaemonFixtures } from "./api/fixtureClient";
import type {
  DaemonEventStream,
  DaemonUpdateEvent,
  EventStreamRequest,
  Instance,
  ModelInvalidation,
  RequestOptions,
  ValidationWarning,
} from "./api/types";
import {
  LiveDataController,
  type LiveDataConfig,
  type LiveFreshness,
} from "./liveData";
import { dataCacheKey, SessionDataCache } from "./dataCache";
import type { PortalDiagnostics } from "./portalDiagnostics";
import { populatedDaemonFixtures } from "./test/daemonFixtures";

const testConfig: LiveDataConfig = {
  invalidationWindowMs: 10,
  reconnectBaseDelayMs: 100,
  reconnectMaxDelayMs: 200,
  connectTimeoutMs: 300_000,
  failuresBeforePolling: 2,
  pollingIntervalMs: 200,
  refreshMaxDelayMs: 1_000,
  // High enough that it never fires in tests about something else; the
  // watchdog gets its own config below.
  streamIdleTimeoutMs: 300_000,
  connectionSettledMs: 50,
};

beforeEach(() => {
  vi.useFakeTimers();
  window.sessionStorage.clear();
  Object.defineProperty(window.navigator, "onLine", { configurable: true, value: true });
  Object.defineProperty(document, "visibilityState", {
    configurable: true,
    value: "visible",
  });
  window.location.hash = "#/overview";
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe("LiveDataController", () => {
  it("invalidates the exact cached resources named by an SSE event", async () => {
    const stream = new ControlledEventStream();
    const client = new ScriptedClient([() => Promise.resolve(stream)]);
    const cache = new SessionDataCache();
    const runOne = dataCacheKey("run-detail", "run-1");
    const runTwo = dataCacheKey("run-detail", "run-2");
    cache.set(runOne, "run one", [{ model: "run", runId: "run-1" }]);
    cache.set(runTwo, "run two", [{ model: "run", runId: "run-2" }]);
    const controller = new LiveDataController(client, testConfig, { cache });

    controller.start();
    await settle();
    stream.push({
      id: "session:1",
      type: "invalidate",
      data: { cursor: "session:1", models: ["run"], runIds: ["run-1"] },
    });
    await settle();

    expect(cache.get(runOne)).toBeUndefined();
    expect(cache.get(runTwo)).toBe("run two");
    controller.stop();
  });

  it("falls back to polling when the initial SSE connection hangs", async () => {
    let connectSignal: AbortSignal | undefined;
    const client = new ScriptedClient([
      (_request, options) => {
        connectSignal = options?.signal;
        return new Promise<DaemonEventStream>(() => undefined);
      },
    ]);
    const controller = new LiveDataController(client, {
      ...testConfig,
      connectTimeoutMs: 1_000,
      failuresBeforePolling: 1,
    });
    const refresh = vi.fn();
    controller.subscribe(["instance", "run", "workflow"], refresh);
    refresh.mockClear();

    controller.start();
    await vi.advanceTimersByTimeAsync(999);
    expect(controller.freshness).toBe("reconnecting");
    expect(refresh).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(1);
    await settle();
    expect(connectSignal?.aborted).toBe(true);
    expect(controller.freshness).toBe("polling-fallback");
    expect(refresh).toHaveBeenCalledWith(
      new Set(["instance", "run", "workflow"]),
      "refresh",
      expect.any(Array),
    );

    controller.stop();
  });

  // Uses an `invalidate` event rather than the connect `snapshot`: a cold connect
  // already fires its own full refresh, so #1685 deliberately skips the snapshot
  // that follows it. The behaviour under test here is the cache contract — an
  // invalidating event must drop an in-flight write and force a refetch, so the
  // response it was about to store cannot land as fresh.
  it("refreshes again after an invalidation drops an in-flight cache write", async () => {
    const stream = new ControlledEventStream();
    const client = new ScriptedClient([() => Promise.resolve(stream)]);
    const cache = new SessionDataCache();
    const controller = new LiveDataController(client, testConfig, { cache });
    const firstRefresh = deferred<void>();
    const key = dataCacheKey("run-detail", "run-1");
    const dependencies = [{ model: "run" as const, runId: "run-1" }];
    let attempt = 0;
    const refresh = vi.fn(() => {
      attempt += 1;
      const revision = cache.beginWrite(key, dependencies);
      if (attempt === 1) {
        return firstRefresh.promise.then(() =>
          cache.set(key, "first snapshot", dependencies, revision),
        );
      }
      return cache.set(key, "refreshed snapshot", dependencies, revision);
    });

    controller.start();
    controller.subscribe(["run"], refresh);
    await settle();
    expect(refresh).toHaveBeenCalledOnce();

    stream.push({
      id: "session:0",
      type: "invalidate",
      data: { cursor: "session:0", models: ["instance", "run", "workflow"] },
    });
    await settle();
    firstRefresh.resolve();
    await settle();
    await vi.advanceTimersByTimeAsync(10);
    await settle();

    expect(refresh).toHaveBeenCalledTimes(2);
    expect(cache.get(key)).toBe("refreshed snapshot");
    controller.stop();
  });

  it("deduplicates ordered events into one effective model refresh window", async () => {
    const stream = new ControlledEventStream();
    const client = new ScriptedClient([() => Promise.resolve(stream)]);
    const controller = new LiveDataController(client, testConfig);
    const refresh = vi.fn();
    controller.subscribe(["instance", "run", "workflow"], refresh);
    refresh.mockClear();

    controller.start();
    await settle();
    await vi.advanceTimersByTimeAsync(10);
    refresh.mockClear();

    stream.push(update("session:1", ["run"]));
    stream.push(update("session:1", ["run"]));
    stream.push(update("session:2", ["workflow"]));
    stream.push(update("session:1", ["instance"]));
    await settle();
    await vi.advanceTimersByTimeAsync(9);
    expect(refresh).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(1);
    expect(refresh).toHaveBeenCalledOnce();
    expect(refresh.mock.calls[0]?.[0]).toEqual(new Set(["run", "workflow"]));
    expect(window.sessionStorage.getItem("goobers-live-event-cursor")).toBe("session:2");

    controller.stop();
  });

  it("refreshes only subscriptions matching event entity identities", async () => {
    const stream = new ControlledEventStream();
    const client = new ScriptedClient([() => Promise.resolve(stream)]);
    const controller = new LiveDataController(client, testConfig);
    const matchingRun = vi.fn();
    const unrelatedRun = vi.fn();
    const matchingWorkflow = vi.fn();
    const unrelatedWorkflow = vi.fn();
    controller.subscribe(["run"], matchingRun, { runId: "run-a" });
    controller.subscribe(["run"], unrelatedRun, { runId: "run-b" });
    controller.subscribe(["workflow", "run"], matchingWorkflow, {
      gaggle: "core",
      workflow: "implementation",
    });
    controller.subscribe(["workflow", "run"], unrelatedWorkflow, {
      gaggle: "other",
      workflow: "implementation",
    });
    controller.start();
    await settle();
    matchingRun.mockClear();
    unrelatedRun.mockClear();
    matchingWorkflow.mockClear();
    unrelatedWorkflow.mockClear();
    stream.push(
      update("session:1", ["run", "workflow"], {
        runIds: ["run-a"],
        workflows: [{ gaggle: "core", name: "implementation" }],
      }),
    );
    await settle();
    await vi.advanceTimersByTimeAsync(10);

    expect(matchingRun).toHaveBeenCalledWith(
      new Set(["run"]),
      "refresh",
      expect.any(Array),
    );
    expect(matchingWorkflow).toHaveBeenCalledWith(
      new Set(["run", "workflow"]),
      "refresh",
      expect.any(Array),
    );
    expect(unrelatedRun).not.toHaveBeenCalled();
    expect(unrelatedWorkflow).not.toHaveBeenCalled();

    controller.stop();
  });

  // #1711 defect 1: a dead-but-open stream showed as "connected" forever.
  //
  // The daemon emits a heartbeat every 15s precisely so a client can detect
  // this. The client parsed heartbeats and threw them away, and had no liveness
  // deadline — its only timeout is cleared as soon as response HEADERS arrive,
  // so the body read loop had none at all. On a NAT rebind or an idle-timeout
  // proxy, `reader.read()` neither resolves nor rejects: freshness stayed
  // "connected", no reconnect was scheduled, and the polling fallback never
  // engaged. A visible, untouched dashboard showed arbitrarily stale state
  // behind a green "live" indicator — the wall-monitor case, which the
  // visibilitychange/online handlers do not cover.
  it("reconnects when the stream goes silent past the idle deadline", async () => {
    const first = new ControlledEventStream();
    const second = new ControlledEventStream();
    const client = new ScriptedClient([
      () => Promise.resolve(first),
      () => Promise.resolve(second),
    ]);
    const controller = new LiveDataController(client, {
      ...testConfig,
      streamIdleTimeoutMs: 1_000,
    });
    controller.start();
    await settle();
    expect(client.requests).toHaveLength(1);

    // The socket stays open and completely silent — no data, no heartbeat, no
    // error. This is exactly what a silently-dropped TCP connection looks like.
    await vi.advanceTimersByTimeAsync(900);
    expect(client.requests).toHaveLength(1);

    await vi.advanceTimersByTimeAsync(200);
    await settle();
    expect(client.requests).toHaveLength(2);

    controller.stop();
  });

  // The other half of the same mechanism: a heartbeat carries no data and is
  // still discarded by applyEvent, but it must keep the connection alive. If
  // the watchdog only re-armed on data, an idle-but-healthy instance would
  // reconnect every 45 seconds forever.
  it("treats a heartbeat as proof of life without applying it", async () => {
    const stream = new ControlledEventStream();
    const client = new ScriptedClient([() => Promise.resolve(stream)]);
    const controller = new LiveDataController(client, {
      ...testConfig,
      streamIdleTimeoutMs: 1_000,
    });
    const listener = vi.fn();
    controller.subscribe(["run"], listener);
    controller.start();
    await settle();
    listener.mockClear();

    for (let beat = 0; beat < 3; beat += 1) {
      await vi.advanceTimersByTimeAsync(800);
      stream.push({ id: `heartbeat:${beat}`, type: "heartbeat" } as never);
      await settle();
    }

    // 2.4s elapsed against a 1s deadline, and still one connection.
    expect(client.requests).toHaveLength(1);
    // And the heartbeats did not masquerade as data.
    expect(listener).not.toHaveBeenCalled();

    controller.stop();
  });

  // #1711 defect 2: reconnect backoff reset on the first byte.
  //
  // A buffering reverse proxy, or a daemon in a restart loop, accepts the SSE
  // request, flushes the initial snapshot, then closes. Receiving ANY event
  // reset failureCount to 0, so handleDisconnect set it back to 1, the delay
  // stayed at the 250ms base, and the client reconnected four times a second
  // indefinitely — never reaching failuresBeforePolling, never backing off, and
  // driving an invalidation flush from the replayed snapshot every cycle.
  //
  // Time connected, not bytes received, is what distinguishes a working stream
  // from one that dies on arrival.
  it("does not reset backoff for a stream that dies immediately after its first event", async () => {
    const streams = [
      new ControlledEventStream(),
      new ControlledEventStream(),
      new ControlledEventStream(),
    ];
    let index = 0;
    const client = new ScriptedClient(
      streams.map((stream) => () => {
        index += 1;
        return Promise.resolve(stream);
      }),
    );
    const controller = new LiveDataController(client, {
      ...testConfig,
      reconnectBaseDelayMs: 100,
      reconnectMaxDelayMs: 10_000,
      connectionSettledMs: 5_000,
    });
    controller.start();
    await settle();
    expect(client.requests).toHaveLength(1);

    // Flush one event, then die — well inside connectionSettledMs.
    streams[0]?.push(update("session:1", ["run"]));
    await settle();
    streams[0]?.end();
    await settle();

    // First retry at the base delay.
    await vi.advanceTimersByTimeAsync(100);
    await settle();
    expect(client.requests).toHaveLength(2);

    streams[1]?.push(update("session:2", ["run"]));
    await settle();
    streams[1]?.end();
    await settle();

    // The second retry must be LONGER than the base. Under the old behaviour
    // the event reset the count and this fired at 100ms again, forever.
    await vi.advanceTimersByTimeAsync(100);
    expect(client.requests).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(150);
    await settle();
    expect(client.requests).toHaveLength(3);

    controller.stop();
  });

  // #1930 / §8.2: a differing epoch must force a SNAPSHOT, not a quiet cursor
  // swap.
  //
  // The store was rebuilt, so the client's view predates a generation it cannot
  // reason about — its sequence numbers came from a different AUTOINCREMENT.
  // Adopting the new cursor and carrying on leaves it following the new feed
  // while holding pre-rebuild data, and nothing ever corrects that: every
  // subsequent event applies cleanly, so the staleness is permanent and
  // invisible.
  it("forces a full refresh when the cursor epoch changes mid-stream", async () => {
    const stream = new ControlledEventStream();
    const client = new ScriptedClient([() => Promise.resolve(stream)]);
    const controller = new LiveDataController(client, testConfig);
    const listener = vi.fn();
    // Scoped to one run: if the epoch change only refreshed what the event
    // named, this listener would not fire, because the event names a different
    // run entirely.
    controller.subscribe(["run"], listener, { runId: "run-unrelated" });
    controller.start();
    await settle();

    stream.push(update("1:epoch-a:5", ["run"], { runIds: ["run-a"] }));
    await settle();
    await vi.advanceTimersByTimeAsync(20);
    listener.mockClear();

    // Same schema, NEW epoch — a rebuild, delivered on the established stream
    // without the connection dropping.
    stream.push(update("1:epoch-b:1", ["run"], { runIds: ["run-a"] }));
    await settle();
    await vi.advanceTimersByTimeAsync(20);

    expect(listener).toHaveBeenCalled();

    controller.stop();
  });

  // The counterpart: within ONE epoch, an event that names other entities must
  // not refresh an unrelated subscriber. Otherwise the epoch fix would have
  // bought correctness by making every event a full refresh.
  it("does not refresh unrelated subscribers within the same epoch", async () => {
    const stream = new ControlledEventStream();
    const client = new ScriptedClient([() => Promise.resolve(stream)]);
    const controller = new LiveDataController(client, testConfig);
    const listener = vi.fn();
    controller.subscribe(["run"], listener, { runId: "run-unrelated" });
    controller.start();
    await settle();

    stream.push(update("1:epoch-a:5", ["run"], { runIds: ["run-a"] }));
    await settle();
    await vi.advanceTimersByTimeAsync(20);
    listener.mockClear();

    stream.push(update("1:epoch-a:6", ["run"], { runIds: ["run-a"] }));
    await settle();
    await vi.advanceTimersByTimeAsync(20);

    expect(listener).not.toHaveBeenCalled();

    controller.stop();
  });

  it("reconnects with the last applied event ID", async () => {
    const first = new ControlledEventStream();
    const second = new ControlledEventStream();
    const client = new ScriptedClient([
      () => Promise.resolve(first),
      () => Promise.resolve(second),
    ]);
    const controller = new LiveDataController(client, testConfig);

    controller.start();
    await settle();
    first.push(update("session:4", ["run"]));
    await settle();
    first.end();
    await settle();
    expect(controller.freshness).toBe("reconnecting");

    await vi.advanceTimersByTimeAsync(100);
    await settle();
    expect(client.requests[1]?.cursor).toBe("session:4");
    expect(controller.freshness).toBe("connected");

    controller.stop();
  });

  it("resumes after visibility changes without a blanket refresh", async () => {
    const first = new ControlledEventStream();
    const second = new ControlledEventStream();
    first.push(snapshot("session:0"));
    const client = new ScriptedClient([
      () => Promise.resolve(first),
      () => Promise.resolve(second),
    ]);
    const controller = new LiveDataController(client, testConfig);
    const refresh = vi.fn();
    controller.subscribe(["instance", "run", "workflow"], refresh);
    refresh.mockClear();

    controller.start();
    await settle();
    await vi.advanceTimersByTimeAsync(10);
    expect(refresh).toHaveBeenCalledOnce();
    refresh.mockClear();

    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      value: "hidden",
    });
    document.dispatchEvent(new Event("visibilitychange"));
    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      value: "visible",
    });
    document.dispatchEvent(new Event("visibilitychange"));
    await settle();
    await vi.advanceTimersByTimeAsync(10);

    expect(client.requests[1]).toEqual({ cursor: "session:0" });
    expect(refresh).not.toHaveBeenCalled();
    expect(controller.freshness).toBe("connected");

    controller.stop();
  });

  it("preserves queued invalidations across a visibility reconnect", async () => {
    const first = new ControlledEventStream();
    const second = new ControlledEventStream();
    const client = new ScriptedClient([
      () => Promise.resolve(first),
      () => Promise.resolve(second),
    ]);
    const controller = new LiveDataController(client, testConfig);
    const refresh = vi.fn();
    controller.subscribe(["run"], refresh, { runId: "run-a" });
    refresh.mockClear();

    controller.start();
    await settle();
    refresh.mockClear();
    first.push(update("session:1", ["run"], { runIds: ["run-a"] }));
    await settle();

    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      value: "hidden",
    });
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(10);
    expect(refresh).not.toHaveBeenCalled();

    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      value: "visible",
    });
    document.dispatchEvent(new Event("visibilitychange"));
    await settle();
    await vi.advanceTimersByTimeAsync(0);

    expect(client.requests[1]).toEqual({ cursor: "session:1" });
    expect(refresh).toHaveBeenCalledWith(new Set(["run"]), "refresh", expect.any(Array));

    controller.stop();
  });

  it("retries an unsuccessful in-flight invalidation after refocus", async () => {
    window.sessionStorage.setItem("goobers-live-event-cursor", "session:0");
    const first = new ControlledEventStream();
    const second = new ControlledEventStream();
    const client = new ScriptedClient([
      () => Promise.resolve(first),
      () => Promise.resolve(second),
    ]);
    const controller = new LiveDataController(client, testConfig);
    const firstRefresh = deferred<boolean>();
    const refresh = vi.fn();
    controller.subscribe(["run"], refresh, { runId: "run-a" });
    refresh.mockReset();
    refresh.mockReturnValueOnce(firstRefresh.promise).mockResolvedValue(true);

    controller.start();
    await settle();
    first.push(update("session:1", ["run"], { runIds: ["run-a"] }));
    await settle();
    await vi.advanceTimersByTimeAsync(10);
    expect(refresh).toHaveBeenCalledOnce();

    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      value: "hidden",
    });
    document.dispatchEvent(new Event("visibilitychange"));
    firstRefresh.resolve(false);
    await settle();
    await vi.advanceTimersByTimeAsync(200);
    expect(refresh).toHaveBeenCalledOnce();

    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      value: "visible",
    });
    document.dispatchEvent(new Event("visibilitychange"));
    await settle();
    await vi.advanceTimersByTimeAsync(0);

    expect(client.requests[1]).toEqual({ cursor: "session:1" });
    expect(refresh).toHaveBeenCalledTimes(2);
    expect(refresh).toHaveBeenLastCalledWith(new Set(["run"]), "refresh", expect.any(Array));

    controller.stop();
  });

  it("reports SSE causes across retries and visibility changes", async () => {
    const first = new ControlledEventStream();
    const second = new ControlledEventStream();
    const third = new ControlledEventStream();
    const client = new ScriptedClient([
      () => Promise.resolve(first),
      () => Promise.resolve(second),
      () => Promise.resolve(third),
    ]);
    const recordSSE = vi.fn();
    const diagnostics: PortalDiagnostics = {
      startRequest: () => ({ finish: () => undefined }),
      recordSSE,
    };
    const controller = new LiveDataController(client, testConfig, { diagnostics });

    controller.start();
    await settle();
    first.end();
    await settle();
    await vi.advanceTimersByTimeAsync(100);
    await settle();

    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      value: "hidden",
    });
    document.dispatchEvent(new Event("visibilitychange"));
    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      value: "visible",
    });
    document.dispatchEvent(new Event("visibilitychange"));
    await settle();

    expect(recordSSE.mock.calls.map((call) => call[0])).toEqual([
      { event: "connect", cause: "initial" },
      { event: "disconnect", cause: "stream-ended" },
      { event: "reconnect", cause: "stream-ended", delayMs: 100 },
      { event: "connect", cause: "stream-ended" },
      { event: "disconnect", cause: "visibility-hidden" },
      { event: "reconnect", cause: "visibility-visible", delayMs: undefined },
      { event: "connect", cause: "visibility-visible" },
    ]);

    controller.stop();
  });

  // A refresh fails exactly when the daemon is slow or erroring. Retrying it at a
  // flat pollingIntervalMs meant the portal re-issued its whole snapshot every 5s
  // forever against a backend that could not answer, which is what produced the
  // observed proxy-error flood (#1710). Successive failures must space out.
  it("backs off exponentially while refreshes keep failing, and resets on success", async () => {
    const stream = new ControlledEventStream();
    const client = new ScriptedClient([() => Promise.resolve(stream)]);
    const controller = new LiveDataController(client, testConfig);
    const refresh = vi.fn();
    controller.subscribe(["run"], refresh);
    refresh.mockReset();
    refresh.mockResolvedValue(false);

    controller.start();
    await settle();
    stream.push(update("session:1", ["run"]));
    await settle();
    await vi.advanceTimersByTimeAsync(10);
    expect(refresh).toHaveBeenCalledOnce();

    // First retry is one polling interval out.
    await vi.advanceTimersByTimeAsync(200);
    expect(refresh).toHaveBeenCalledTimes(2);

    // Second retry must be 400ms, not another 200ms.
    await vi.advanceTimersByTimeAsync(200);
    expect(refresh).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(200);
    expect(refresh).toHaveBeenCalledTimes(3);

    // Third retry must be 800ms.
    await vi.advanceTimersByTimeAsync(400);
    expect(refresh).toHaveBeenCalledTimes(3);
    await vi.advanceTimersByTimeAsync(400);
    expect(refresh).toHaveBeenCalledTimes(4);

    controller.stop();
  });

  // The counterpart to the escalation above: a recovered daemon must not stay
  // penalised by the backoff it earned while it was failing.
  it("resets refresh backoff after a success so a later failure retries at the base interval", async () => {
    const stream = new ControlledEventStream();
    const client = new ScriptedClient([() => Promise.resolve(stream)]);
    const controller = new LiveDataController(client, testConfig);
    const refresh = vi.fn();
    controller.subscribe(["run"], refresh);
    refresh.mockReset();
    // Fail once (escalating the delay to 400ms for the retry after next), then
    // recover on the retry.
    refresh.mockResolvedValueOnce(false).mockResolvedValue(true);

    controller.start();
    await settle();
    stream.push(update("session:1", ["run"]));
    await settle();
    await vi.advanceTimersByTimeAsync(10);
    expect(refresh).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(200);
    expect(refresh).toHaveBeenCalledTimes(2); // succeeded -> backoff reset

    // A fresh failure now must retry at the base interval, not the escalated one.
    refresh.mockResolvedValue(false);
    stream.push(update("session:2", ["run"]));
    await settle();
    await vi.advanceTimersByTimeAsync(10);
    expect(refresh).toHaveBeenCalledTimes(3);
    await vi.advanceTimersByTimeAsync(200);
    expect(refresh).toHaveBeenCalledTimes(4);

    controller.stop();
  });

  it("clears an expired cursor, requests a full snapshot, and reconnects cleanly", async () => {
    window.sessionStorage.setItem("goobers-live-event-cursor", "expired:9");
    const recovered = new ControlledEventStream();
    recovered.push(snapshot("current:0"));
    const client = new ScriptedClient([
      () => Promise.reject(new DaemonApiError(409, "stale_cursor", "expired")),
      () => Promise.resolve(recovered),
    ]);
    const controller = new LiveDataController(client, testConfig);
    const states: LiveFreshness[] = [];
    const refresh = vi.fn();
    controller.subscribe(["instance", "run", "workflow"], refresh);
    controller.subscribeState((state) => states.push(state));
    refresh.mockClear();

    controller.start();
    await settle();
    expect(states).toContain("stale");
    expect(window.sessionStorage.getItem("goobers-live-event-cursor")).toBeNull();

    await vi.advanceTimersByTimeAsync(0);
    await settle();
    expect(client.requests).toEqual([{ cursor: "expired:9" }, undefined]);
    await vi.advanceTimersByTimeAsync(10);
    expect(refresh).toHaveBeenCalledOnce();
    expect(refresh.mock.calls[0]?.[0]).toEqual(new Set(["instance", "run", "workflow"]));

    controller.stop();
  });

  it("retries a failed post-connect snapshot until it succeeds", async () => {
    const stream = new ControlledEventStream();
    stream.push(snapshot("session:0"));
    const client = new ScriptedClient([() => Promise.resolve(stream)]);
    const controller = new LiveDataController(client, testConfig);
    const refresh = vi.fn();
    controller.subscribe(["instance", "run", "workflow"], refresh);
    refresh.mockReset();
    refresh.mockResolvedValueOnce(false).mockResolvedValueOnce(true);

    controller.start();
    await settle();
    await vi.advanceTimersByTimeAsync(10);
    expect(refresh).toHaveBeenCalledOnce();
    expect(controller.freshness).toBe("stale");

    await vi.advanceTimersByTimeAsync(189);
    expect(refresh).toHaveBeenCalledOnce();

    await vi.advanceTimersByTimeAsync(1);
    expect(refresh).toHaveBeenCalledTimes(2);
    expect(controller.freshness).toBe("connected");

    controller.stop();
  });

  it("collapses invalidation windows during a snapshot into one follow-up refresh", async () => {
    const stream = new ControlledEventStream();
    stream.push(snapshot("session:0"));
    const client = new ScriptedClient([() => Promise.resolve(stream)]);
    const controller = new LiveDataController(client, testConfig);
    const states: LiveFreshness[] = [];
    const initial = deferred<boolean>();
    const replay = deferred<boolean>();
    const refresh = vi.fn();
    controller.subscribe(["instance", "run", "workflow"], refresh);
    controller.subscribeState((state) => states.push(state));
    refresh.mockReset();
    refresh.mockReturnValueOnce(initial.promise).mockReturnValueOnce(replay.promise);

    controller.start();
    await settle();
    await vi.advanceTimersByTimeAsync(10);
    stream.push(update("session:1", ["run"]));
    await settle();
    await vi.advanceTimersByTimeAsync(10);
    stream.push(update("session:2", ["workflow"]));
    await settle();
    await vi.advanceTimersByTimeAsync(10);

    initial.resolve(true);
    await settle();
    expect(refresh).toHaveBeenCalledTimes(2);
    expect(states).not.toContain("connected");

    replay.resolve(true);
    await settle();
    expect(refresh).toHaveBeenCalledTimes(2);
    expect(controller.freshness).toBe("connected");

    controller.stop();
  });

  it("hands off an immediate full refresh queued as a flush completes", async () => {
    const stream = new ControlledEventStream();
    stream.push(snapshot("session:0"));
    const client = new ScriptedClient([() => Promise.resolve(stream)]);
    const controller = new LiveDataController(client, {
      ...testConfig,
      invalidationWindowMs: 0,
    });
    const refresh = vi.fn().mockResolvedValue(true);
    controller.subscribe(["instance", "run", "workflow"], refresh);
    refresh.mockClear();

    const internals = controller as unknown as {
      drainInvalidations: () => Promise<void>;
    };
    const drainInvalidations = internals.drainInvalidations.bind(controller);
    let queueAtCompletion = true;
    vi.spyOn(internals, "drainInvalidations").mockImplementation(async () => {
      await drainInvalidations();
      if (queueAtCompletion) {
        queueAtCompletion = false;
        controller.refresh();
      }
    });

    controller.start();
    await settle();
    await settle();
    expect(refresh).toHaveBeenCalledTimes(2);

    controller.stop();
  });

  it("polls the same models while SSE is unavailable", async () => {
    const unavailable = () => Promise.reject(new DaemonUnavailableError());
    const client = new ScriptedClient([unavailable, unavailable, unavailable]);
    const controller = new LiveDataController(client, testConfig);
    const refresh = vi.fn();
    controller.subscribe(["instance", "run", "workflow"], refresh);
    refresh.mockClear();

    controller.start();
    await settle();
    await vi.advanceTimersByTimeAsync(100);
    await settle();
    expect(controller.freshness).toBe("polling-fallback");

    await vi.advanceTimersByTimeAsync(10);
    expect(refresh).toHaveBeenCalledOnce();
    expect(refresh.mock.calls[0]?.[0]).toEqual(new Set(["instance", "run", "workflow"]));

    await vi.advanceTimersByTimeAsync(210);
    expect(refresh.mock.calls.length).toBeGreaterThan(1);

    controller.stop();
  });

  it("waits for each polling refresh before scheduling the next", async () => {
    const unavailable = () => Promise.reject(new DaemonUnavailableError());
    const client = new ScriptedClient([unavailable, unavailable, unavailable]);
    const controller = new LiveDataController(client, testConfig);
    const firstPoll = deferred<boolean>();
    const refresh = vi.fn();
    controller.subscribe(["instance", "run", "workflow"], refresh);
    refresh.mockReset();
    refresh.mockReturnValueOnce(firstPoll.promise).mockResolvedValue(true);

    controller.start();
    await settle();
    await vi.advanceTimersByTimeAsync(100);
    await settle();
    expect(controller.freshness).toBe("polling-fallback");
    expect(refresh).toHaveBeenCalledOnce();

    await vi.advanceTimersByTimeAsync(400);
    expect(refresh).toHaveBeenCalledOnce();

    firstPoll.resolve(true);
    await settle();
    await vi.advanceTimersByTimeAsync(199);
    expect(refresh).toHaveBeenCalledOnce();

    await vi.advanceTimersByTimeAsync(1);
    expect(refresh).toHaveBeenCalledTimes(2);

    controller.stop();
  });

  it("closes streams and timers across network, visibility, and teardown changes", async () => {
    const streams = [
      new ControlledEventStream(),
      new ControlledEventStream(),
      new ControlledEventStream(),
    ];
    const client = new ScriptedClient(streams.map((stream) => () => Promise.resolve(stream)));
    const controller = new LiveDataController(client, testConfig);

    controller.start();
    await settle();
    window.dispatchEvent(new Event("offline"));
    expect(controller.freshness).toBe("offline");
    expect(streams[0]?.close).toHaveBeenCalledOnce();

    Object.defineProperty(window.navigator, "onLine", { configurable: true, value: true });
    window.dispatchEvent(new Event("online"));
    await settle();
    expect(client.requests).toHaveLength(2);

    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      value: "hidden",
    });
    document.dispatchEvent(new Event("visibilitychange"));
    expect(controller.freshness).toBe("stale");
    expect(streams[1]?.close).toHaveBeenCalledOnce();

    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      value: "visible",
    });
    document.dispatchEvent(new Event("visibilitychange"));
    await settle();
    expect(client.requests).toHaveLength(3);

    controller.stop();
    expect(streams[2]?.close).toHaveBeenCalledOnce();
    expect(vi.getTimerCount()).toBe(0);
  });
});

describe("live page integration", () => {
  it("refreshes terminal run detail when the run model is invalidated", async () => {
    vi.useRealTimers();
    window.location.hash = "#/run/01JZ402DASHBOARD";
    const client = new MutableFixtureClient();
    const getRun = vi.spyOn(client, "getRun");
    render(<App client={client} />);

    expect(
      await screen.findByRole("heading", { name: "Run 01JZ402DASHBOARD" }),
    ).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Live updates connected"));
    const initialReads = getRun.mock.calls.length;

    client.stream.push(
      update("fixture:1", ["run"], { runIds: ["01JZ441DAEMONAPI"] }),
    );
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(getRun).toHaveBeenCalledTimes(initialReads);

    client.stream.push(
      update("fixture:2", ["run"], { runIds: ["01JZ402DASHBOARD"] }),
    );

    await waitFor(() => expect(getRun.mock.calls.length).toBeGreaterThan(initialReads));
  });

  it("stops active run detail refreshes while hidden and after unmount", async () => {
    window.location.hash = "#/run/01JZ441DAEMONAPI";
    const client = new MutableFixtureClient();
    const getRun = vi.spyOn(client, "getRun");
    const { unmount } = render(<App client={client} />);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
      await settle();
    });
    expect(
      screen.getByRole("heading", { name: "Run 01JZ441DAEMONAPI" }),
    ).toBeInTheDocument();
    const visibleReads = getRun.mock.calls.length;

    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      value: "hidden",
    });
    act(() => document.dispatchEvent(new Event("visibilitychange")));
    await act(async () => vi.advanceTimersByTimeAsync(10_000));
    expect(getRun).toHaveBeenCalledTimes(visibleReads);

    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      value: "visible",
    });
    await act(async () => {
      document.dispatchEvent(new Event("visibilitychange"));
      await vi.advanceTimersByTimeAsync(0);
      await settle();
    });
    expect(getRun).toHaveBeenCalledTimes(visibleReads);

    client.stream.push(
      update("fixture:1", ["run"], { runIds: ["01JZ441DAEMONAPI"] }),
    );
    await act(async () => {
      await vi.advanceTimersByTimeAsync(50);
      await settle();
    });
    expect(getRun.mock.calls.length).toBeGreaterThan(visibleReads);

    const readsBeforeUnmount = getRun.mock.calls.length;
    unmount();
    await act(async () => vi.advanceTimersByTimeAsync(10_000));
    expect(getRun).toHaveBeenCalledTimes(readsBeforeUnmount);
  });

  it("refreshes configuration warnings for their instance and workflow models", async () => {
    vi.useRealTimers();
    const client = new MutableFixtureClient();
    let instanceWarnings: ValidationWarning[] = [];
    let workflowWarnings: ValidationWarning[] = [];
    const warningClient = {
      getInstance: vi.fn(async () => ({ warnings: instanceWarnings })),
      getWorkflow: vi.fn(async () => ({ warnings: workflowWarnings })),
    };
    render(<App client={client} warningClient={warningClient} />);

    expect(await screen.findByText("No active configuration warnings.")).toBeInTheDocument();
    instanceWarnings = [warning("VER001", "Instance/live")];
    client.stream.push(update("fixture:1", ["instance"]));
    expect(await screen.findByText("VER001")).toBeInTheDocument();

    act(() => {
      window.location.hash = "#/workflow/core/implementation";
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    expect(
      await screen.findByText("No active configuration warnings for this workflow."),
    ).toBeInTheDocument();
    workflowWarnings = [warning("VER002", "Workflow/implementation")];
    client.stream.push(update("fixture:2", ["workflow"]));
    expect(await screen.findByText("VER002")).toBeInTheDocument();
  });

  it("refreshes runs without reloading inventory for run-journal invalidations", async () => {
    vi.useRealTimers();
    window.location.hash = "#/workflows";
    const client = new MutableFixtureClient();
    const listGaggles = vi.spyOn(client, "listGaggles");
    const listGoobers = vi.spyOn(client, "listGoobers");
    const listWorkflows = vi.spyOn(client, "listWorkflows");
    const listRuns = vi.spyOn(client, "listRuns");
    render(<App client={client} />);

    expect(await screen.findByRole("heading", { name: "Workflows" })).toBeInTheDocument();
    expect(screen.getByText("1 active / 2 max")).toBeInTheDocument();
    const coreSection = screen
      .getByRole("heading", { name: "Core product" })
      .closest<HTMLElement>(".gaggle-section");
    if (!coreSection) {
      throw new Error("Core product inventory section was not rendered.");
    }
    expect(within(coreSection).getByText("Active runs").nextElementSibling).toHaveTextContent("1");
    await waitFor(() =>
      expect(screen.getByRole("status")).toHaveTextContent("Live updates connected"),
    );
    const inventoryGaggleReads = listGaggles.mock.calls.length;
    const inventoryGooberReads = listGoobers.mock.calls.length;
    const inventoryWorkflowReads = listWorkflows.mock.calls.length;
    const runReads = listRuns.mock.calls.length;

    client.setActiveRuns("core", "implementation", 2);
    await act(async () => {
      for (let sequence = 1; sequence <= 8; sequence += 1) {
        client.stream.push(
          update(`fixture:${sequence}`, ["instance", "run", "workflow"], {
            runIds: ["01JZ441DAEMONAPI"],
            workflows: [{ gaggle: "core", name: "implementation" }],
          }),
        );
      }
      await new Promise((resolve) => setTimeout(resolve, 150));
    });

    expect(screen.getByText("2 active / 2 max")).toBeInTheDocument();
    expect(within(coreSection).getByText("Active runs").nextElementSibling).toHaveTextContent("2");
    expect(listGaggles).toHaveBeenCalledTimes(inventoryGaggleReads);
    expect(listGoobers).toHaveBeenCalledTimes(inventoryGooberReads);
    expect(listWorkflows).toHaveBeenCalledTimes(inventoryWorkflowReads);
    expect(listRuns).toHaveBeenCalledTimes(runReads + 1);
  });

  it("shares inventory across page mounts and refetches it after a definition event", async () => {
    vi.useRealTimers();
    const client = new MutableFixtureClient();
    const listGaggles = vi.spyOn(client, "listGaggles");
    const listGoobers = vi.spyOn(client, "listGoobers");
    const listWorkflows = vi.spyOn(client, "listWorkflows");
    render(<App client={client} />);

    expect(
      await screen.findByRole("heading", { name: "2 runs need attention." }),
    ).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByRole("status")).toHaveTextContent("Live updates connected"),
    );
    const inventoryReads = {
      gaggles: listGaggles.mock.calls.length,
      goobers: listGoobers.mock.calls.length,
      workflows: listWorkflows.mock.calls.length,
    };

    act(() => {
      window.location.hash = "#/workflows";
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    expect(await screen.findByRole("heading", { name: "Workflows" })).toBeInTheDocument();
    expect(listGaggles).toHaveBeenCalledTimes(inventoryReads.gaggles);
    expect(listGoobers.mock.calls.length).toBeGreaterThan(inventoryReads.goobers);
    expect(listWorkflows).toHaveBeenCalledTimes(inventoryReads.workflows);

    const populatedInventoryReads = {
      gaggles: listGaggles.mock.calls.length,
      goobers: listGoobers.mock.calls.length,
      workflows: listWorkflows.mock.calls.length,
    };
    act(() => {
      window.location.hash = "#/overview";
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    expect(
      await screen.findByRole("heading", { name: "2 runs need attention." }),
    ).toBeInTheDocument();
    act(() => {
      window.location.hash = "#/workflows";
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    expect(await screen.findByRole("heading", { name: "Workflows" })).toBeInTheDocument();
    expect(listGaggles).toHaveBeenCalledTimes(populatedInventoryReads.gaggles);
    expect(listGoobers).toHaveBeenCalledTimes(populatedInventoryReads.goobers);
    expect(listWorkflows).toHaveBeenCalledTimes(populatedInventoryReads.workflows);

    client.addWorkflow();
    act(() => client.stream.push(update("fixture:1", ["instance", "workflow"])));

    expect(await screen.findByText("Inventory refresh")).toBeInTheDocument();
    expect(listGaggles.mock.calls.length).toBeGreaterThan(populatedInventoryReads.gaggles);
    expect(listGoobers.mock.calls.length).toBeGreaterThan(populatedInventoryReads.goobers);
    expect(listWorkflows.mock.calls.length).toBeGreaterThan(populatedInventoryReads.workflows);
  });

  it("meets the local p95 update target and stays stale on disconnect", async () => {
    vi.useRealTimers();
    const client = new MutableFixtureClient();
    render(<App client={client} />);

    expect(
      await screen.findByRole("heading", { name: "2 runs need attention." }),
    ).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("Live updates connected");

    const latencies: number[] = [];
    for (let sequence = 1; sequence <= 20; sequence += 1) {
      client.instanceName = `refreshed-instance-${sequence}`;
      const started = performance.now();
      client.stream.push(update(`fixture:${sequence}`, ["instance"]));
      await waitFor(
        () => expect(screen.getByText(client.instanceName)).toBeInTheDocument(),
        { timeout: 900 },
      );
      latencies.push(performance.now() - started);
    }
    const sortedLatencies = [...latencies].sort((left, right) => left - right);
    const p95Index = Math.ceil(sortedLatencies.length * 0.95) - 1;
    expect(sortedLatencies[p95Index]).toBeLessThan(1_000);

    client.stream.end();
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Reconnecting"));
    expect(screen.getByText("refreshed-instance-20")).toBeInTheDocument();
    expect(screen.getByRole("status")).not.toHaveTextContent("Live updates connected");
  }, 25_000);
});

class ScriptedClient extends FixtureDaemonClient {
  readonly requests: (EventStreamRequest | undefined)[] = [];

  constructor(
    private readonly connections: ((
      request: EventStreamRequest | undefined,
      options: RequestOptions | undefined,
    ) => Promise<DaemonEventStream>)[],
  ) {
    super(populatedDaemonFixtures());
  }

  override connectEvents(
    request?: EventStreamRequest,
    options?: RequestOptions,
  ): Promise<DaemonEventStream> {
    this.requests.push(request);
    const connection = this.connections.shift();
    if (!connection) {
      return Promise.reject(new DaemonUnavailableError());
    }
    return connection(request, options);
  }
}

class MutableFixtureClient extends FixtureDaemonClient {
  private currentStream = new ControlledEventStream();
  private readonly mutableFixtures: DaemonFixtures;
  instanceName = "goobers-dev";

  constructor() {
    const fixtures = populatedDaemonFixtures();
    super(fixtures);
    this.mutableFixtures = fixtures;
  }

  get stream(): ControlledEventStream {
    return this.currentStream;
  }

  override connectEvents(request?: EventStreamRequest): Promise<DaemonEventStream> {
    this.currentStream = new ControlledEventStream();
    if (!request?.cursor) {
      this.currentStream.push(snapshot("fixture:0"));
    }
    return Promise.resolve(this.currentStream);
  }

  override async getInstance(options?: RequestOptions): Promise<Instance> {
    const instance = await super.getInstance(options);
    return { ...instance, name: this.instanceName };
  }

  addWorkflow(): void {
    const workflows = this.mutableFixtures.workflows?.core;
    const template = workflows?.items[0];
    const gaggle = this.mutableFixtures.gaggles.items.find((item) => item.name === "core");
    if (!workflows || !template || !gaggle) {
      throw new Error("Populated fixtures must include the core workflow inventory.");
    }
    workflows.items.push({
      ...template,
      identity: { gaggle: "core", name: "inventory-refresh" },
      displayName: "Inventory refresh",
    });
    workflows.page.total = workflows.items.length;
    gaggle.workflowCount = workflows.items.length;
  }

  setActiveRuns(gaggleName: string, workflowName: string, activeRuns: number): void {
    const gaggle = this.mutableFixtures.gaggles.items.find((item) => item.name === gaggleName);
    const workflow = this.mutableFixtures.workflows?.[gaggleName]?.items.find(
      (item) => item.identity.name === workflowName,
    );
    if (!gaggle || !workflow) {
      throw new Error("Populated fixtures must include the requested workflow inventory.");
    }
    gaggle.activeRunCount = activeRuns;
    workflow.concurrency.activeRuns = activeRuns;
  }
}

class ControlledEventStream implements DaemonEventStream {
  private ended = false;
  private readonly queue: DaemonUpdateEvent[] = [];
  private readonly readers: ((result: IteratorResult<DaemonUpdateEvent>) => void)[] = [];
  readonly close = vi.fn(() => this.end());

  push(event: DaemonUpdateEvent): void {
    if (this.ended) {
      throw new Error("Cannot push to a closed event stream.");
    }
    const reader = this.readers.shift();
    if (reader) {
      reader({ done: false, value: event });
    } else {
      this.queue.push(event);
    }
  }

  end(): void {
    if (this.ended) {
      return;
    }
    this.ended = true;
    for (const reader of this.readers.splice(0)) {
      reader({ done: true, value: undefined });
    }
  }

  [Symbol.asyncIterator](): AsyncIterator<DaemonUpdateEvent> {
    return {
      next: () => {
        const event = this.queue.shift();
        if (event) {
          return Promise.resolve({ done: false, value: event });
        }
        if (this.ended) {
          return Promise.resolve({ done: true, value: undefined });
        }
        return new Promise((resolve) => this.readers.push(resolve));
      },
    };
  }
}

function update(
  id: string,
  models: ("instance" | "run" | "workflow")[],
  targets: Pick<ModelInvalidation, "runIds" | "workflows"> = {},
): DaemonUpdateEvent {
  return {
    id,
    type: "invalidate",
    data: { cursor: id, models, ...targets },
  };
}

function snapshot(id: string): DaemonUpdateEvent {
  return {
    id,
    type: "snapshot",
    data: { cursor: id, models: ["instance", "run", "workflow"] },
  };
}

function warning(code: ValidationWarning["code"], scope: string): ValidationWarning {
  return {
    code,
    severity: "warning",
    scope,
    explanation: `${scope} changed.`,
  };
}

async function settle(): Promise<void> {
  for (let turn = 0; turn < 5; turn += 1) {
    await Promise.resolve();
  }
}

function deferred<T>(): {
  promise: Promise<T>;
  resolve: (value: T) => void;
} {
  let resolve: (value: T) => void = () => undefined;
  const promise = new Promise<T>((promiseResolve) => {
    resolve = promiseResolve;
  });
  return { promise, resolve };
}
