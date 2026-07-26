import { act, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { DaemonApiError, DaemonUnavailableError } from "./api/errors";
import { FixtureDaemonClient } from "./api/fixtureClient";
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
  failuresBeforePolling: 2,
  pollingIntervalMs: 200,
  refreshMaxDelayMs: 1_000,
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

    expect(matchingRun).toHaveBeenCalledWith(new Set(["run"]), "refresh");
    expect(matchingWorkflow).toHaveBeenCalledWith(new Set(["run", "workflow"]), "refresh");
    expect(unrelatedRun).not.toHaveBeenCalled();
    expect(unrelatedWorkflow).not.toHaveBeenCalled();

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
    expect(refresh).toHaveBeenCalledWith(new Set(["run"]), "refresh");

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
    expect(refresh).toHaveBeenLastCalledWith(new Set(["run"]), "refresh");

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

  it("does not re-page gaggle/workflow inventory during a burst of run invalidations", async () => {
    vi.useRealTimers();
    const client = new MutableFixtureClient();
    const listGaggles = vi.spyOn(client, "listGaggles");
    const listWorkflows = vi.spyOn(client, "listWorkflows");
    render(<App client={client} />);

    expect(
      await screen.findByRole("heading", { name: "2 runs need attention." }),
    ).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByRole("status")).toHaveTextContent("Live updates connected"),
    );
    const inventoryGaggleReads = listGaggles.mock.calls.length;
    const inventoryWorkflowReads = listWorkflows.mock.calls.length;

    for (let sequence = 1; sequence <= 8; sequence += 1) {
      client.stream.push(update(`fixture:${sequence}`, ["run"]));
    }
    await new Promise((resolve) => setTimeout(resolve, 150));

    // Run-only invalidations rebuild the bounded run groups but never re-page
    // the gaggle/workflow inventory.
    expect(listGaggles.mock.calls.length).toBe(inventoryGaggleReads);
    expect(listWorkflows.mock.calls.length).toBe(inventoryWorkflowReads);
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
  });
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
  instanceName = "goobers-dev";

  constructor() {
    super(populatedDaemonFixtures());
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
