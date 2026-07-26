import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "./api/fixtureClient";
import type {
  DaemonEventStream,
  DaemonUpdateEvent,
  EventList,
  RequestOptions,
  RunDetail,
} from "./api/types";
import { LiveDataProvider } from "./liveData";
import { useRunDetail } from "./runDetailData";
import { populatedDaemonFixtures } from "./test/daemonFixtures";

describe("useRunDetail", () => {
  beforeEach(() => {
    window.sessionStorage.clear();
    Object.defineProperty(document, "visibilityState", {
      configurable: true,
      value: "visible",
    });
    Object.defineProperty(window.navigator, "onLine", {
      configurable: true,
      value: true,
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("loads fresh data, retains it while refreshing, and preserves it on refresh error", async () => {
    const runId = "01JZ441DAEMONAPI";
    const client = new DeferredRunDetailClient();
    const { result, unmount } = renderHook(() => useRunDetail(client, runId), {
      wrapper: liveWrapper(client),
    });

    expect(result.current.state).toEqual({ status: "loading" });
    await waitFor(() => expect(client.signals).toHaveLength(2));
    expect(result.current.state).toEqual({ status: "loading" });

    act(() => client.release(2));
    await waitFor(() => expect(result.current.state.status).toBe("ready"));
    if (result.current.state.status !== "ready") {
      throw new Error("Expected run detail to be ready.");
    }
    const retained = result.current.state.data;

    act(() => client.stream.push(invalidation("run-refresh")));
    await waitFor(() => expect(client.signals).toHaveLength(4));
    expect(result.current.state).toEqual({ status: "stale", data: retained });

    const refreshError = new Error("refresh failed");
    client.failure = refreshError;
    act(() => client.release(2));
    await waitFor(() =>
      expect(result.current.state).toEqual({
        status: "stale",
        data: retained,
        error: refreshError,
      }),
    );

    unmount();
  });
});

class DeferredRunDetailClient extends FixtureDaemonClient {
  readonly signals: (AbortSignal | undefined)[] = [];
  readonly stream = new ControlledEventStream();
  failure: Error | undefined;
  private readonly gates: Deferred[] = [];

  constructor() {
    super(populatedDaemonFixtures());
  }

  override connectEvents(): Promise<DaemonEventStream> {
    return Promise.resolve(this.stream);
  }

  override async getRun(runId: string, options?: RequestOptions): Promise<RunDetail> {
    await this.waitForRelease(options);
    return super.getRun(runId, options);
  }

  override async listRunEvents(
    runId: string,
    options?: RequestOptions,
  ): Promise<EventList> {
    await this.waitForRelease(options);
    return super.listRunEvents(runId, options);
  }

  release(count: number): void {
    for (const gate of this.gates.splice(0, count)) {
      gate.resolve();
    }
  }

  private async waitForRelease(options?: RequestOptions): Promise<void> {
    this.signals.push(options?.signal);
    const gate = deferred();
    this.gates.push(gate);
    await gate.promise;
    if (this.failure) {
      throw this.failure;
    }
  }
}

class ControlledEventStream implements DaemonEventStream {
  private closed = false;
  private readonly events: DaemonUpdateEvent[] = [];
  private readonly readers: ((result: IteratorResult<DaemonUpdateEvent>) => void)[] = [];

  push(event: DaemonUpdateEvent): void {
    const reader = this.readers.shift();
    if (reader) {
      reader({ done: false, value: event });
      return;
    }
    this.events.push(event);
  }

  close(): void {
    this.closed = true;
    for (const reader of this.readers.splice(0)) {
      reader({ done: true, value: undefined });
    }
  }

  [Symbol.asyncIterator](): AsyncIterator<DaemonUpdateEvent> {
    return {
      next: () => {
        const event = this.events.shift();
        if (event) {
          return Promise.resolve({ done: false, value: event });
        }
        if (this.closed) {
          return Promise.resolve({ done: true, value: undefined });
        }
        return new Promise((resolve) => this.readers.push(resolve));
      },
    };
  }
}

interface Deferred {
  promise: Promise<void>;
  resolve: () => void;
}

function deferred(): Deferred {
  let resolve: () => void = () => undefined;
  const promise = new Promise<void>((promiseResolve) => {
    resolve = promiseResolve;
  });
  return { promise, resolve };
}

function invalidation(id: string): DaemonUpdateEvent {
  return { id, type: "invalidate", data: { cursor: id, models: ["run"] } };
}

function liveWrapper(client: DeferredRunDetailClient) {
  return ({ children }: { children: ReactNode }) => (
    <LiveDataProvider client={client} config={{ invalidationWindowMs: 0 }}>
      {children}
    </LiveDataProvider>
  );
}
