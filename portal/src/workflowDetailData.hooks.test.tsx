import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "./api/fixtureClient";
import type {
  DaemonEventStream,
  DaemonUpdateEvent,
  RequestOptions,
  RunList,
  RunListOptions,
  WorkflowDetail,
} from "./api/types";
import { LiveDataProvider } from "./liveData";
import { populatedDaemonFixtures } from "./test/daemonFixtures";
import { useWorkflowDetail } from "./workflowDetailData";

const GAGGLE = "core";
const WORKFLOW = "implementation";

describe("useWorkflowDetail", () => {
  beforeEach(() => {
    window.sessionStorage.clear();
    Object.defineProperty(document, "visibilityState", { configurable: true, value: "visible" });
    Object.defineProperty(window.navigator, "onLine", { configurable: true, value: true });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("publishes ready once a successful load lands on a live stream", async () => {
    const client = new DeferredWorkflowDetailClient();
    const { result, unmount } = renderHook(() => useWorkflowDetail(client, GAGGLE, WORKFLOW), {
      wrapper: liveWrapper(client),
    });

    expect(result.current.state).toEqual({ status: "loading" });
    await waitFor(() => expect(client.signals).toHaveLength(2));

    act(() => client.release(2));
    await waitFor(() => expect(result.current.state.status).toBe("ready"));
    if (result.current.state.status !== "ready") {
      throw new Error("Expected workflow detail to be ready.");
    }
    expect(result.current.state.data.workflow.identity.name).toBe(WORKFLOW);

    unmount();
  });

  it("publishes stale when the live stream drops while the request is in flight (#3657)", async () => {
    const client = new DeferredWorkflowDetailClient();
    const { result, unmount } = renderHook(() => useWorkflowDetail(client, GAGGLE, WORKFLOW), {
      wrapper: liveWrapper(client),
    });

    await waitFor(() => expect(client.signals).toHaveLength(2));
    act(() => goOffline());

    act(() => client.release(2));
    await waitFor(() => expect(result.current.state.status).toBe("stale"));
    if (result.current.state.status !== "stale") {
      throw new Error("Expected workflow detail to be stale while disconnected.");
    }
    expect(result.current.state.data.workflow.identity.name).toBe(WORKFLOW);
    expect(result.current.state.error).toBeUndefined();

    unmount();
  });
});

function goOffline(): void {
  Object.defineProperty(window.navigator, "onLine", { configurable: true, value: false });
  window.dispatchEvent(new Event("offline"));
}

class DeferredWorkflowDetailClient extends FixtureDaemonClient {
  readonly signals: (AbortSignal | undefined)[] = [];
  readonly stream = new ControlledEventStream();
  private readonly gates: Deferred[] = [];

  constructor() {
    super(populatedDaemonFixtures());
  }

  override connectEvents(): Promise<DaemonEventStream> {
    return Promise.resolve(this.stream);
  }

  override async getWorkflow(
    gaggle: string,
    workflow: string,
    options?: RequestOptions,
  ): Promise<WorkflowDetail> {
    await this.waitForRelease(options);
    return super.getWorkflow(gaggle, workflow, options);
  }

  override async listRuns(request?: RunListOptions, options?: RequestOptions): Promise<RunList> {
    await this.waitForRelease(options);
    return super.listRuns(request, options);
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

function liveWrapper(client: DeferredWorkflowDetailClient) {
  return ({ children }: { children: ReactNode }) => (
    <LiveDataProvider client={client} config={{ invalidationWindowMs: 0 }}>
      {children}
    </LiveDataProvider>
  );
}
