import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { DaemonApiError, DaemonUnavailableError } from "./api/errors";
import { FixtureDaemonClient } from "./api/fixtureClient";
import type {
  DaemonEventStream,
  DaemonUpdateEvent,
  EventStreamRequest,
  Instance,
  RequestOptions,
} from "./api/types";
import {
  LiveDataController,
  type LiveDataConfig,
  type LiveFreshness,
} from "./liveData";
import { populatedDaemonFixtures } from "./test/daemonFixtures";

const testConfig: LiveDataConfig = {
  invalidationWindowMs: 10,
  reconnectBaseDelayMs: 100,
  reconnectMaxDelayMs: 200,
  failuresBeforePolling: 2,
  pollingIntervalMs: 200,
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
  vi.useRealTimers();
});

describe("LiveDataController", () => {
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

  it("clears an expired cursor, requests a full snapshot, and reconnects cleanly", async () => {
    window.sessionStorage.setItem("goobers-live-event-cursor", "expired:9");
    const recovered = new ControlledEventStream();
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
  it("refreshes visible daemon data within the local update target and stays stale on disconnect", async () => {
    vi.useRealTimers();
    const client = new MutableFixtureClient();
    render(<App client={client} />);

    expect(
      await screen.findByRole("heading", { name: "2 runs need attention." }),
    ).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("Live updates connected");

    client.instanceName = "refreshed-instance";
    const started = performance.now();
    client.stream.push(update("fixture:1", ["instance"]));
    await waitFor(() => expect(screen.getByText("refreshed-instance")).toBeInTheDocument(), {
      timeout: 900,
    });
    expect(performance.now() - started).toBeLessThan(1_000);

    client.stream.end();
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Reconnecting"));
    expect(screen.getByText("refreshed-instance")).toBeInTheDocument();
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
  readonly stream = new ControlledEventStream();
  instanceName = "goobers-dev";

  constructor() {
    super(populatedDaemonFixtures());
  }

  override connectEvents(): Promise<DaemonEventStream> {
    return Promise.resolve(this.stream);
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

function update(id: string, models: ("instance" | "run" | "workflow")[]): DaemonUpdateEvent {
  return {
    id,
    type: "invalidate",
    data: { cursor: id, models },
  };
}

async function settle(): Promise<void> {
  await Promise.resolve();
  await Promise.resolve();
}
