import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "./api/fixtureClient";
import type {
  DaemonEventStream,
  DaemonUpdateEvent,
  Instance,
  RequestOptions,
  RunList,
  RunListOptions,
} from "./api/types";
import { LiveDataProvider } from "./liveData";
import { useOperationalOverview, useOperationalSnapshot } from "./operationalData";
import { populatedDaemonFixtures } from "./test/daemonFixtures";

class GatedRunsClient extends FixtureDaemonClient {
  readonly signals: (AbortSignal | undefined)[] = [];
  readonly stream = new ControlledEventStream();
  instanceName: string | undefined;
  private readonly gates: { promise: Promise<void>; resolve: () => void }[] = [];

  override connectEvents(): Promise<DaemonEventStream> {
    return Promise.resolve(this.stream);
  }

  override async listRuns(request?: RunListOptions, options?: RequestOptions): Promise<RunList> {
    this.signals.push(options?.signal);
    const gate = deferred();
    this.gates.push(gate);
    await gate.promise;
    return super.listRuns(request, options);
  }

  override async getInstance(options?: RequestOptions): Promise<Instance> {
    const instance = await super.getInstance(options);
    return this.instanceName ? { ...instance, name: this.instanceName } : instance;
  }

  release(count: number): void {
    for (const gate of this.gates.splice(0, count)) {
      gate.resolve();
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

function wrapper(client: GatedRunsClient) {
  return ({ children }: { children: ReactNode }) => (
    <LiveDataProvider client={client} config={{ invalidationWindowMs: 0 }}>
      {children}
    </LiveDataProvider>
  );
}

function deferred(): { promise: Promise<void>; resolve: () => void } {
  let resolve: () => void = () => undefined;
  const promise = new Promise<void>((promiseResolve) => {
    resolve = promiseResolve;
  });
  return { promise, resolve };
}

async function settle(): Promise<void> {
  for (let turn = 0; turn < 5; turn += 1) {
    await Promise.resolve();
  }
}

describe("operational hooks coalesce in-flight refreshes (#1367)", () => {
  beforeEach(() => {
    window.sessionStorage.clear();
    Object.defineProperty(document, "visibilityState", { configurable: true, value: "visible" });
    Object.defineProperty(window.navigator, "onLine", { configurable: true, value: true });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("finishes a slow overview load and collapses a refresh burst into one current replay", async () => {
    const client = new GatedRunsClient(populatedDaemonFixtures());
    const { result, unmount } = renderHook(() => useOperationalOverview(client), {
      wrapper: wrapper(client),
    });

    await waitFor(() => expect(client.signals).toHaveLength(5));
    const initialSignals = [...client.signals];

    act(() => {
      client.stream.push(invalidation("session:1"));
      client.stream.push(invalidation("session:2"));
      client.stream.push(invalidation("session:3"));
    });
    await waitFor(() =>
      expect(window.sessionStorage.getItem("goobers-live-event-cursor")).toBe("session:3"),
    );
    expect(client.signals).toHaveLength(5);
    expect(initialSignals.every((signal) => signal?.aborted === false)).toBe(true);

    client.instanceName = "refreshed-instance";
    act(() => client.release(5));
    await waitFor(() => expect(client.signals).toHaveLength(10));
    expect(initialSignals.every((signal) => signal?.aborted === false)).toBe(true);

    act(() => client.release(5));
    await waitFor(() => {
      expect(result.current.state.status).toBe("ready");
      if (result.current.state.status === "ready") {
        expect(result.current.state.data.instance.name).toBe("refreshed-instance");
      }
    });
    expect(client.signals).toHaveLength(10);
    unmount();
  });

  it("keeps one snapshot request in flight and aborts the active replay on unmount", async () => {
    const client = new GatedRunsClient(populatedDaemonFixtures());
    const { result, unmount } = renderHook(() => useOperationalSnapshot(client), {
      wrapper: wrapper(client),
    });

    await waitFor(() => expect(client.signals).toHaveLength(1));
    const firstSignal = client.signals[0];

    act(() => {
      result.current.retry();
      result.current.retry();
    });
    await act(async () => Promise.resolve());
    expect(client.signals).toHaveLength(1);
    expect(firstSignal?.aborted).toBe(false);

    act(() => client.release(1));
    await waitFor(() => expect(client.signals).toHaveLength(2));
    const replaySignal = client.signals[1];
    unmount();
    expect(replaySignal?.aborted).toBe(true);
    expect(firstSignal?.aborted).toBe(false);
    client.release(1);
  });

  it("keeps replacement-client work out of a detached operation", async () => {
    const firstClient = new GatedRunsClient(populatedDaemonFixtures());
    const replacementClient = new GatedRunsClient(populatedDaemonFixtures());
    const { result, rerender, unmount } = renderHook(
      ({ client }) => useOperationalSnapshot(client),
      {
        initialProps: { client: firstClient },
        wrapper: wrapper(firstClient),
      },
    );

    await waitFor(() => expect(firstClient.signals).toHaveLength(1));
    const detachedSignal = firstClient.signals[0];

    rerender({ client: replacementClient });
    expect(detachedSignal?.aborted).toBe(true);
    act(() => {
      result.current.retry();
      result.current.retry();
    });
    await waitFor(() => expect(replacementClient.signals).toHaveLength(1));

    act(() => firstClient.release(1));
    await settle();
    expect(replacementClient.signals).toHaveLength(1);

    act(() => replacementClient.release(1));
    await waitFor(() => expect(replacementClient.signals).toHaveLength(2));
    unmount();
    replacementClient.release(1);
  });
});

function invalidation(id: string): DaemonUpdateEvent {
  return { id, type: "invalidate", data: { cursor: id, models: ["run"] } };
}
