import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "./api/fixtureClient";
import type {
  DaemonClient,
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

class LiveSnapshotClient extends FixtureDaemonClient {
  readonly stream = new ControlledEventStream();

  override connectEvents(): Promise<DaemonEventStream> {
    return Promise.resolve(this.stream);
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

function wrapper(client: DaemonClient) {
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
    const firstSignals = [...client.signals];

    act(() => {
      result.current.retry();
      result.current.retry();
    });
    await act(async () => Promise.resolve());
    expect(client.signals).toHaveLength(1);
    expect(firstSignals.every((signal) => signal?.aborted === false)).toBe(true);

    act(() => client.release(1));
    await waitFor(() => expect(client.signals).toHaveLength(2));
    const replaySignals = client.signals.slice(1);
    unmount();
    expect(replaySignals.every((signal) => signal?.aborted === true)).toBe(true);
    expect(firstSignals.every((signal) => signal?.aborted === false)).toBe(true);
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
    const detachedSignals = [...firstClient.signals];

    rerender({ client: replacementClient });
    expect(detachedSignals.every((signal) => signal?.aborted === true)).toBe(true);
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

  it.each([
    {
      model: "run" as const,
      expected: { gaggles: 0, goobers: 0, workflows: 0, runs: 1 },
    },
    {
      model: "workflow" as const,
      expected: { gaggles: 1, goobers: 2, workflows: 2, runs: 0 },
    },
    {
      model: "instance" as const,
      expected: { gaggles: 0, goobers: 0, workflows: 0, runs: 0 },
    },
  ])("honors $model invalidation request boundaries", async ({ model, expected }) => {
    const client = new LiveSnapshotClient(populatedDaemonFixtures());
    const getHealth = vi.spyOn(client, "getHealth");
    const getInstance = vi.spyOn(client, "getInstance");
    const listGaggles = vi.spyOn(client, "listGaggles");
    const listGoobers = vi.spyOn(client, "listGoobers");
    const listWorkflows = vi.spyOn(client, "listWorkflows");
    const listRuns = vi.spyOn(client, "listRuns");
    const { result, unmount } = renderHook(() => useOperationalSnapshot(client), {
      wrapper: wrapper(client),
    });

    await waitFor(() => expect(result.current.state.status).toBe("ready"));
    getHealth.mockClear();
    getInstance.mockClear();
    listGaggles.mockClear();
    listGoobers.mockClear();
    listWorkflows.mockClear();
    listRuns.mockClear();

    act(() => {
      client.stream.push({
        id: `session:${model}`,
        type: "invalidate",
        data: { cursor: `session:${model}`, models: [model] },
      });
    });

    await waitFor(() => expect(getHealth).toHaveBeenCalledOnce());
    expect(getInstance).toHaveBeenCalledOnce();
    expect(listGaggles).toHaveBeenCalledTimes(expected.gaggles);
    expect(listGoobers).toHaveBeenCalledTimes(expected.goobers);
    expect(listWorkflows).toHaveBeenCalledTimes(expected.workflows);
    expect(listRuns).toHaveBeenCalledTimes(expected.runs);
    unmount();
  });
});

function invalidation(id: string): DaemonUpdateEvent {
  return { id, type: "invalidate", data: { cursor: id, models: ["run"] } };
}
