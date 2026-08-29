import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it } from "vitest";
import { FixtureDaemonClient, type DaemonFixtures } from "./api/fixtureClient";
import type {
  DaemonEventStream,
  DaemonUpdateEvent,
  RequestOptions,
  RunList,
  RunListOptions,
} from "./api/types";
import { LiveDataProvider } from "./liveData";
import { useRunsHistory, type RunsFilter } from "./runsHistory";
import { populatedDaemonFixtures } from "./test/daemonFixtures";

// #3656: a background refresh started for one filter used a controller nothing
// else held, so a slow response could land after the operator had already
// switched filters and merge the old scope's runs into the new one's list.
describe("useRunsHistory across a scope change", () => {
  beforeEach(() => {
    window.sessionStorage.clear();
  });

  it("aborts and rejects a refresh that outlives the filter it was started for", async () => {
    const client = new DeferredRunsClient();
    const { result, rerender, unmount } = renderHook(
      ({ filter }: { filter: RunsFilter }) => useRunsHistory(client, filter),
      { initialProps: { filter: "all" as RunsFilter }, wrapper: liveWrapper(client) },
    );

    await waitFor(() => expect(client.pending(anyRequest)).toHaveLength(1));
    await act(async () => client.release(anyRequest));
    await waitFor(() => expect(result.current.state.status).toBe("ready"));
    expect(runIds(result.current.state)).toContain("01JZ400FAILED");

    // A live event starts a background refresh for the "all" filter, which
    // then takes its time.
    act(() => client.stream.push(invalidation("run-refresh")));
    await waitFor(() => expect(client.pending(unfiltered)).toHaveLength(1));
    const stale = client.pending(unfiltered)[0];

    // The operator switches filters while that refresh is still in flight.
    rerender({ filter: "complete" });
    await waitFor(() => expect(client.pending(completed)).toHaveLength(1));
    expect(stale.signal?.aborted).toBe(true);

    await act(async () => client.release(completed));
    await waitFor(() => expect(runIds(result.current.state)).toEqual(["01JZ455ESCALATE"]));

    // The stale response lands anyway — an abort cannot unschedule a promise
    // that had already resolved — and must be dropped, not merged.
    await act(async () => {
      client.release(unfiltered);
      await Promise.resolve();
    });
    await settle();

    expect(runIds(result.current.state)).toEqual(["01JZ455ESCALATE"]);
    expect(result.current.state.status).toBe("ready");

    unmount();
  });

  it("still merges a refresh that completes within its own scope", async () => {
    const client = new DeferredRunsClient();
    const { result, unmount } = renderHook(
      ({ filter }: { filter: RunsFilter }) => useRunsHistory(client, filter),
      { initialProps: { filter: "complete" as RunsFilter }, wrapper: liveWrapper(client) },
    );

    await waitFor(() => expect(client.pending(completed)).toHaveLength(1));
    await act(async () => client.release(completed));
    await waitFor(() => expect(runIds(result.current.state)).toEqual(["01JZ455ESCALATE"]));

    client.addRun("01JZ999NEWCOMPLETE", "2026-07-18T07:00:00Z");
    act(() => client.stream.push(invalidation("run-refresh")));
    await waitFor(() => expect(client.pending(completed)).toHaveLength(1));
    await act(async () => client.release(completed));

    await waitFor(() =>
      expect(runIds(result.current.state)).toEqual(["01JZ999NEWCOMPLETE", "01JZ455ESCALATE"]),
    );

    unmount();
  });
});

const anyRequest = () => true;
const unfiltered = (request?: RunListOptions) => request?.phase === undefined;
const completed = (request?: RunListOptions) => request?.phase === "completed";

function runIds(state: { status: string; data?: { runs: { id: string }[] } }): string[] {
  return state.data?.runs.map((run) => run.id) ?? [];
}

async function settle(): Promise<void> {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 10));
  });
}

/**
 * A client whose run listings are released by the test, one predicate at a
 * time, so a response for an abandoned scope can be delivered AFTER the
 * response for the current one. The release deliberately ignores the request's
 * abort signal: the defect is about a response that had already resolved when
 * the scope changed, which no abort can call back.
 */
class DeferredRunsClient extends FixtureDaemonClient {
  readonly stream = new ControlledEventStream();
  private readonly gates: {
    request: RunListOptions | undefined;
    signal: AbortSignal | undefined;
    resolve: () => void;
  }[] = [];

  private readonly journal: DaemonFixtures;

  constructor(journal: DaemonFixtures = populatedDaemonFixtures()) {
    super(journal);
    this.journal = journal;
  }

  override connectEvents(): Promise<DaemonEventStream> {
    return Promise.resolve(this.stream);
  }

  override async listRuns(
    request?: RunListOptions,
    options?: RequestOptions,
  ): Promise<RunList> {
    await new Promise<void>((resolve) => {
      this.gates.push({ request, resolve, signal: options?.signal });
    });
    return super.listRuns(request);
  }

  addRun(id: string, startedAt: string): void {
    this.journal.runs.runs.push({
      ...this.journal.runs.runs[0],
      id,
      lastSeq: 99,
      phase: "completed",
      startedAt,
    });
  }

  pending(match: (request?: RunListOptions) => boolean): {
    signal: AbortSignal | undefined;
  }[] {
    return this.gates.filter((gate) => match(gate.request));
  }

  release(match: (request?: RunListOptions) => boolean): void {
    for (const gate of this.gates.filter((candidate) => match(candidate.request))) {
      this.gates.splice(this.gates.indexOf(gate), 1);
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

function invalidation(id: string): DaemonUpdateEvent {
  return { id, type: "invalidate", data: { cursor: id, models: ["run"] } };
}

function liveWrapper(client: DeferredRunsClient) {
  return ({ children }: { children: ReactNode }) => (
    <LiveDataProvider client={client} config={{ invalidationWindowMs: 0 }}>
      {children}
    </LiveDataProvider>
  );
}
