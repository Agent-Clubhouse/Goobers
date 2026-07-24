import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "./api/fixtureClient";
import type { RequestOptions, RunList, RunListOptions } from "./api/types";
import { LiveDataProvider } from "./liveData";
import { useOperationalOverview, useOperationalSnapshot } from "./operationalData";
import { populatedDaemonFixtures } from "./test/daemonFixtures";

// A client whose listRuns records the AbortSignal it was handed and stays
// pending until released, so a refresh can be observed mid-flight.
class GatedRunsClient extends FixtureDaemonClient {
  readonly signals: (AbortSignal | undefined)[] = [];
  private open = false;
  private readonly waiters: (() => void)[] = [];

  override async listRuns(request?: RunListOptions, options?: RequestOptions): Promise<RunList> {
    this.signals.push(options?.signal);
    if (!this.open) {
      await new Promise<void>((resolve) => this.waiters.push(resolve));
    }
    return super.listRuns(request, options);
  }

  release(): void {
    this.open = true;
    for (const resolve of this.waiters.splice(0)) {
      resolve();
    }
  }
}

function wrapper(client: GatedRunsClient) {
  return ({ children }: { children: ReactNode }) => (
    <LiveDataProvider client={client} config={{ invalidationWindowMs: 0 }}>
      {children}
    </LiveDataProvider>
  );
}

describe("operational hooks do not abort in-flight reads on refresh (#1367)", () => {
  beforeEach(() => {
    Object.defineProperty(document, "visibilityState", { configurable: true, value: "visible" });
    Object.defineProperty(window.navigator, "onLine", { configurable: true, value: true });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("keeps the overview's in-flight request alive when a new refresh starts", async () => {
    const client = new GatedRunsClient(populatedDaemonFixtures());
    const { result, unmount } = renderHook(() => useOperationalOverview(client), {
      wrapper: wrapper(client),
    });

    // First load is in flight (its listRuns are gated open).
    await waitFor(() => expect(client.signals.length).toBeGreaterThanOrEqual(1));
    const firstSignal = client.signals[0];
    expect(firstSignal?.aborted).toBe(false);

    // A second refresh must NOT cancel the first request mid-flight.
    const before = client.signals.length;
    act(() => {
      result.current.retry();
    });
    await waitFor(() => expect(client.signals.length).toBeGreaterThan(before));
    expect(firstSignal?.aborted).toBe(false);

    // Unmount is the one place a still-pending request is aborted — and it
    // cancels only the latest controller, never resurrects an abort of the
    // earlier request that was allowed to finish on its own.
    const latestSignal = client.signals[client.signals.length - 1];
    unmount();
    expect(latestSignal?.aborted).toBe(true);
    expect(firstSignal?.aborted).toBe(false);
    client.release();
  });

  it("keeps the snapshot's in-flight request alive when a new refresh starts", async () => {
    const client = new GatedRunsClient(populatedDaemonFixtures());
    const { result, unmount } = renderHook(() => useOperationalSnapshot(client), {
      wrapper: wrapper(client),
    });

    await waitFor(() => expect(client.signals.length).toBeGreaterThanOrEqual(1));
    const firstSignal = client.signals[0];
    expect(firstSignal?.aborted).toBe(false);

    const before = client.signals.length;
    act(() => {
      result.current.retry();
    });
    await waitFor(() => expect(client.signals.length).toBeGreaterThan(before));
    expect(firstSignal?.aborted).toBe(false);

    const latestSignal = client.signals[client.signals.length - 1];
    unmount();
    expect(latestSignal?.aborted).toBe(true);
    expect(firstSignal?.aborted).toBe(false);
    client.release();
  });
});
