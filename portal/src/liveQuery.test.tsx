import { act, renderHook, waitFor } from "@testing-library/react";
import { useEffect, useState, type ReactNode } from "react";
import { beforeEach, describe, expect, it } from "vitest";
import { FixtureDaemonClient } from "./api/fixtureClient";
import type {
  DaemonEventStream,
  DaemonUpdateEvent,
  ModelInvalidation,
} from "./api/types";
import { LiveDataProvider, useLiveData } from "./liveData";
import { useLiveQuery, type LiveQueryOptions } from "./liveQuery";
import { populatedDaemonFixtures } from "./test/daemonFixtures";

interface TestSnapshot {
  value: number;
}

const ERROR_MESSAGE = "Unable to read the test snapshot.";

describe("useLiveQuery", () => {
  beforeEach(() => {
    window.sessionStorage.clear();
    Object.defineProperty(document, "visibilityState", { configurable: true, value: "visible" });
    Object.defineProperty(window.navigator, "onLine", { configurable: true, value: true });
  });

  it("keeps a useful in-flight read and completes a fresh one under repeated invalidations", async () => {
    const loader = new DeferredLoader();
    const client = new StreamingClient();
    const { result, unmount } = mount(client, loader);

    await waitFor(() => expect(loader.calls).toBe(1));
    act(() => {
      for (let index = 1; index <= 5; index += 1) {
        client.stream.push(invalidation(`evt-${index}`));
      }
    });
    // The controller batches events into notification passes, so the count is
    // a lower bound; what matters is that every one of them reached the query.
    await waitFor(() => expect(result.current.invalidations).toBeGreaterThanOrEqual(2));
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 10));
    });

    // The abort loop this hook exists to end: five events during one read used
    // to cancel and restart it five times, so nothing ever completed.
    expect(loader.calls).toBe(1);
    expect(loader.signals[0]?.aborted).toBe(false);

    act(() => loader.release(1));
    await waitFor(() => expect(loader.calls).toBe(2));
    act(() => loader.release(1));
    await waitFor(() =>
      expect(result.current.query.state).toEqual({ status: "ready", data: { value: 2 } }),
    );

    // Five invalidations collapse into exactly one follow-up pass.
    expect(loader.calls).toBe(2);
    expect(loader.signals.some((signal) => signal.aborted)).toBe(false);

    unmount();
  });

  it("normalizes a non-Error rejection and recovers on retry", async () => {
    const loader = new DeferredLoader();
    const client = new StreamingClient();
    const { result, unmount } = mount(client, loader);

    await waitFor(() => expect(loader.calls).toBe(1));
    loader.failure = "the daemon fell over";
    act(() => loader.release(1));
    await waitFor(() =>
      expect(result.current.query.state).toEqual({
        status: "error",
        error: new Error(ERROR_MESSAGE),
      }),
    );

    loader.failure = undefined;
    act(() => result.current.query.retry());
    await waitFor(() => expect(loader.calls).toBe(2));
    act(() => loader.release(1));
    await waitFor(() =>
      expect(result.current.query.state).toEqual({ status: "ready", data: { value: 1 } }),
    );

    unmount();
  });

  it("keeps the held snapshot visible while a retry refetches", async () => {
    const loader = new DeferredLoader();
    const client = new StreamingClient();
    const { result, unmount } = mount(client, loader);

    await waitFor(() => expect(loader.calls).toBe(1));
    act(() => loader.release(1));
    await waitFor(() => expect(result.current.query.state.status).toBe("ready"));

    act(() => result.current.query.retry());
    await waitFor(() => expect(loader.calls).toBe(2));
    expect(result.current.query.state).toEqual({ status: "stale", data: { value: 1 } });

    act(() => loader.release(1));
    await waitFor(() =>
      expect(result.current.query.state).toEqual({ status: "ready", data: { value: 2 } }),
    );

    unmount();
  });

  it("aborts the in-flight read on teardown", async () => {
    const loader = new DeferredLoader();
    const client = new StreamingClient();
    const { unmount } = mount(client, loader);

    await waitFor(() => expect(loader.calls).toBe(1));
    unmount();

    expect(loader.signals[0]?.aborted).toBe(true);
    await act(async () => {
      loader.release(1);
      await Promise.resolve();
    });
    expect(loader.calls).toBe(1);
  });

  it("only reloads for invalidations inside the subscription scope", async () => {
    const loader = new DeferredLoader();
    const client = new StreamingClient();
    const { result, unmount } = mount(client, loader, {
      scope: { gaggle: "core", workflow: "implementation" },
    });

    await waitFor(() => expect(loader.calls).toBe(1));
    act(() => loader.release(1));
    await waitFor(() => expect(result.current.query.state.status).toBe("ready"));

    act(() =>
      client.stream.push(
        invalidation("evt-other", { workflows: [{ gaggle: "other", name: "other" }] }),
      ),
    );
    await waitFor(() => expect(result.current.invalidations).toBe(1));
    expect(loader.calls).toBe(1);

    act(() =>
      client.stream.push(
        invalidation("evt-scoped", {
          workflows: [{ gaggle: "core", name: "implementation" }],
        }),
      ),
    );
    await waitFor(() => expect(loader.calls).toBe(2));

    unmount();
  });
});

function mount(
  client: StreamingClient,
  loader: DeferredLoader,
  overrides: Partial<LiveQueryOptions<TestSnapshot>> = {},
) {
  return renderHook(
    () => ({
      query: useLiveQuery<TestSnapshot>({
        cacheKey: "live-query-test",
        dependencies: [{ model: "run" }],
        models: ["run"],
        load: loader.load,
        errorMessage: ERROR_MESSAGE,
        ...overrides,
      }),
      invalidations: useInvalidationCount(),
    }),
    { wrapper: liveWrapper(client) },
  );
}

/** Counts unscoped refresh notifications, so a test can wait for an event to
 * have reached the subscribers before asserting on what the query did. */
function useInvalidationCount(): number {
  const { subscribe } = useLiveData();
  const [count, setCount] = useState(0);
  useEffect(
    () =>
      subscribe(["run"], (_models, reason) => {
        if (reason === "refresh") {
          setCount((current) => current + 1);
        }
        return true;
      }),
    [subscribe],
  );
  return count;
}

class DeferredLoader {
  readonly signals: AbortSignal[] = [];
  failure: unknown;
  private value = 0;
  private readonly gates: Deferred[] = [];

  readonly load = (signal: AbortSignal): Promise<TestSnapshot> => {
    this.signals.push(signal);
    const gate = deferred();
    this.gates.push(gate);
    return gate.promise.then(() => {
      if (this.failure !== undefined) {
        throw this.failure;
      }
      this.value += 1;
      return { value: this.value };
    });
  };

  get calls(): number {
    return this.signals.length;
  }

  release(count: number): void {
    for (const gate of this.gates.splice(0, count)) {
      gate.resolve();
    }
  }
}

class StreamingClient extends FixtureDaemonClient {
  readonly stream = new ControlledEventStream();

  constructor() {
    super(populatedDaemonFixtures());
  }

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

function invalidation(id: string, extra: Partial<ModelInvalidation> = {}): DaemonUpdateEvent {
  return { id, type: "invalidate", data: { cursor: id, models: ["run"], ...extra } };
}

function liveWrapper(client: StreamingClient) {
  return ({ children }: { children: ReactNode }) => (
    <LiveDataProvider client={client} config={{ invalidationWindowMs: 0 }}>
      {children}
    </LiveDataProvider>
  );
}
