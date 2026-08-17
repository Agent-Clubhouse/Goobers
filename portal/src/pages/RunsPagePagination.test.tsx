import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { act } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type {
  DaemonEventStream,
  DaemonUpdateEvent,
  EventStreamRequest,
  RequestOptions,
} from "../api/types";
import { largeJournalFixtures } from "../test/daemonFixtures";

beforeEach(() => {
  window.location.hash = "#/runs";
});

/**
 * A daemon client whose event stream can be driven from the test.
 *
 * The stock FixtureDaemonClient returns a canned stream, which is enough for
 * every test that only needs the page to load. This one is needed because the
 * defect is specifically about what a LIVE event does to already-loaded state.
 */
class PushableClient extends FixtureDaemonClient {
  private readers: ((result: IteratorResult<DaemonUpdateEvent>) => void)[] = [];
  private queued: DaemonUpdateEvent[] = [];

  connectEvents(
    _request?: EventStreamRequest,
    _options?: RequestOptions,
  ): Promise<DaemonEventStream> {
    const self = this;
    const stream: DaemonEventStream = {
      close: () => {},
      [Symbol.asyncIterator]() {
        return {
          next(): Promise<IteratorResult<DaemonUpdateEvent>> {
            const queued = self.queued.shift();
            if (queued) {
              return Promise.resolve({ done: false, value: queued });
            }
            return new Promise((resolve) => self.readers.push(resolve));
          },
        };
      },
    } as DaemonEventStream;
    return Promise.resolve(stream);
  }

  push(event: DaemonUpdateEvent): void {
    const reader = this.readers.shift();
    if (reader) {
      reader({ done: false, value: event });
    } else {
      this.queued.push(event);
    }
  }
}

function runEvent(id: string, runIds: string[] = []): DaemonUpdateEvent {
  return {
    id,
    type: "invalidate",
    data: { cursor: id, models: ["run"], runIds, workflows: [] },
  };
}

function expectGloballyOrderedUniqueRuns(history: HTMLElement): string[] {
  const ids = Array.from(history.querySelectorAll(".row-title"), (row) => row.textContent ?? "");
  const startedAt = Array.from(history.querySelectorAll("time"), (time) =>
    Date.parse(time.dateTime),
  );

  expect(new Set(ids).size).toBe(ids.length);
  expect(startedAt).toEqual([...startedAt].sort((left, right) => right - left));
  return ids;
}

describe("runs history pagination under live events", () => {
  it("paginates attention streams independently until both exhaust", async () => {
    const fixtures = largeJournalFixtures({
      completed: 0,
      running: 0,
      failed: 101,
      escalated: 51,
      aborted: 0,
    });
    const failedRuns = fixtures.runs.runs.filter((run) => run.phase === "failed");
    const escalatedRuns = fixtures.runs.runs.filter((run) => run.phase === "escalated");
    const olderDuplicate = failedRuns.at(-1);
    const newerDuplicate = escalatedRuns[0];
    if (!olderDuplicate || !newerDuplicate) {
      throw new Error("Expected failed and escalated pagination fixtures.");
    }
    newerDuplicate.id = olderDuplicate.id;
    newerDuplicate.lastSeq = olderDuplicate.lastSeq + 1;

    const client = new PushableClient(fixtures);
    const listRuns = vi.spyOn(client, "listRuns");
    const user = userEvent.setup();
    render(<App client={client} />);

    const history = await screen.findByRole("region", { name: "Run history" });
    await user.click(screen.getByRole("button", { name: "attention" }));

    await waitFor(() => expect(history.querySelectorAll("a")).toHaveLength(100));
    expect(expectGloballyOrderedUniqueRuns(history)).toContain(olderDuplicate.id);
    expect(
      within(screen.getByRole("link", { name: `Open run ${olderDuplicate.id}` })).getByText(
        "Failed",
      ),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Load more runs" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Load more runs" }));

    await waitFor(() => expect(history.querySelectorAll("a")).toHaveLength(150));
    expect(expectGloballyOrderedUniqueRuns(history)).toContain(olderDuplicate.id);
    const replacement = screen.getByRole("link", { name: `Open run ${olderDuplicate.id}` });
    expect(within(replacement).getByText("Escalated")).toBeInTheDocument();
    expect(replacement.querySelector("time")).toHaveAttribute("datetime", newerDuplicate.startedAt);
    expect(screen.getByRole("button", { name: "Load more runs" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Load more runs" }));

    await waitFor(() => expect(history.querySelectorAll("a")).toHaveLength(151));
    expect(expectGloballyOrderedUniqueRuns(history)).toContain(olderDuplicate.id);
    expect(screen.queryByRole("button", { name: "Load more runs" })).not.toBeInTheDocument();
    const paginatedPhases = listRuns.mock.calls
      .filter(([request]) => request?.cursor)
      .map(([request]) => request?.phase);
    expect(paginatedPhases).toEqual(["failed", "escalated", "failed"]);
  });

  // #1713: a live run event collapsed the Runs page back to the first page,
  // discarding everything the user had paged in.
  //
  // On the unfiltered #/runs route scope.gaggle and scope.workflow are both
  // undefined, so EVERY run event in the instance triggered the reset — and
  // under the polling fallback that is every 5 seconds, making the page
  // unusable on a busy instance at exactly the moment someone is watching it.
  it("keeps paged-in rows when a live run event arrives", async () => {
    const fixtures = largeJournalFixtures({
      completed: 68,
      running: 0,
      failed: 0,
      escalated: 0,
      aborted: 0,
    });
    const client = new PushableClient(fixtures);
    const user = userEvent.setup();
    render(<App client={client} />);

    const history = await screen.findByRole("region", { name: "Run history" });
    expect(history.querySelectorAll("a")).toHaveLength(50);

    await user.click(screen.getByRole("button", { name: "Load more runs" }));
    await waitFor(() => expect(history.querySelectorAll("a")).toHaveLength(68));
    const retainedRun = fixtures.runs.runs.at(-1);
    if (!retainedRun) {
      throw new Error("Expected a paged-in run fixture.");
    }

    // A live event lands. Before the fix this truncated the list back to 50 and
    // lost the scroll position.
    fixtures.runs.runs.push({
      ...fixtures.runs.runs[0],
      id: "01JZLIVE000001",
      startedAt: "2026-12-31T23:59:59Z",
      lastActivityAt: "2026-12-31T23:59:59Z",
    });
    act(() => {
      client.push(runEvent("session:live-1"));
    });

    await waitFor(() => expect(history.querySelectorAll("a")).toHaveLength(69));
    expect(
      screen.getByRole("link", { name: `Open run ${retainedRun.id}` }),
    ).toBeInTheDocument();
  });

  it("updates every invalidated row outside the refreshed head page", async () => {
    const fixtures = largeJournalFixtures({
      completed: 0,
      running: 68,
      failed: 0,
      escalated: 0,
      aborted: 0,
    });
    const affected = fixtures.runs.runs.slice(0, 2);
    for (const run of affected) {
      run.currentStage = "query-backlog";
    }
    fixtures.runDetails = Object.fromEntries(
      affected.map((run, index) => [
        run.id,
        {
          ...run,
          currentStage: index === 0 ? "implement" : "review",
          lastSeq: run.lastSeq + 1,
          graphStatus: "unavailable",
          transitionsStatus: "unavailable",
        },
      ]),
    );
    const client = new PushableClient(fixtures);
    const getRun = vi.spyOn(client, "getRun");
    const user = userEvent.setup();
    render(<App client={client} />);

    await screen.findByRole("region", { name: "Run history" });
    await user.click(screen.getByRole("button", { name: "Load more runs" }));
    const rows = await Promise.all(
      affected.map((run) => screen.findByRole("link", { name: `Open run ${run.id}` })),
    );
    expect(rows.every((row) => within(row).getByText("query-backlog"))).toBe(true);

    act(() => {
      client.push(runEvent("session:live-subset", affected.map((run) => run.id)));
    });

    await waitFor(() => expect(within(rows[0]).getByText("implement")).toBeInTheDocument());
    expect(within(rows[1]).getByText("review")).toBeInTheDocument();
    expect(getRun).toHaveBeenCalledTimes(2);
  });

  it("keeps refreshing stage updates after selecting the active filter", async () => {
    const fixtures = largeJournalFixtures({
      completed: 1,
      running: 1,
      failed: 0,
      escalated: 0,
      aborted: 0,
    });
    const affected = fixtures.runs.runs.find((run) => run.phase === "running");
    expect(affected).toBeDefined();
    if (!affected) {
      return;
    }
    affected.currentStage = "query-backlog";
    const client = new PushableClient(fixtures);
    const user = userEvent.setup();
    render(<App client={client} />);

    await screen.findByRole("region", { name: "Run history" });
    await user.click(screen.getByRole("button", { name: "active" }));
    const row = await screen.findByRole("link", { name: `Open run ${affected.id}` });
    expect(within(row).getByText("query-backlog")).toBeInTheDocument();

    affected.currentStage = "implement";
    affected.lastSeq += 1;
    act(() => {
      client.push(runEvent("session:active-stage", [affected.id]));
    });

    await waitFor(() => expect(within(row).getByText("implement")).toBeInTheDocument());
  });

  it("refreshes an invalidated paged-in row after an overlapping load-more", async () => {
    const fixtures = largeJournalFixtures({
      completed: 0,
      running: 120,
      failed: 0,
      escalated: 0,
      aborted: 0,
    });
    const affected = fixtures.runs.runs[50];
    affected.currentStage = "query-backlog";
    fixtures.runDetails = {
      [affected.id]: {
        ...affected,
        currentStage: "implement",
        lastSeq: affected.lastSeq + 1,
        graphStatus: "unavailable",
        transitionsStatus: "unavailable",
      },
    };
    const client = new PushableClient(fixtures);
    const originalListRuns = client.listRuns.bind(client);
    let paginationCalls = 0;
    let finishLoadMore: (() => void) | undefined;
    vi.spyOn(client, "listRuns").mockImplementation((request, options) => {
      if (request?.cursor && ++paginationCalls === 2) {
        return new Promise((resolve, reject) => {
          finishLoadMore = () => {
            void originalListRuns(request, options).then(resolve, reject);
          };
        });
      }
      return originalListRuns(request, options);
    });
    const getRun = vi.spyOn(client, "getRun");
    const user = userEvent.setup();
    render(<App client={client} />);

    await screen.findByRole("region", { name: "Run history" });
    await user.click(screen.getByRole("button", { name: "Load more runs" }));
    const row = await screen.findByRole("link", { name: `Open run ${affected.id}` });
    expect(within(row).getByText("query-backlog")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Load more runs" }));
    await screen.findByRole("button", { name: "Loading…" });
    act(() => {
      client.push(runEvent("session:during-pagination", [affected.id]));
    });
    await waitFor(() => expect(finishLoadMore).toBeDefined());
    act(() => finishLoadMore?.());

    await waitFor(() => expect(within(row).getByText("implement")).toBeInTheDocument());
    expect(getRun).toHaveBeenCalledWith(affected.id, expect.anything());
  });

  it("surfaces a targeted refresh failure and retries the retained run id", async () => {
    const fixtures = largeJournalFixtures({
      completed: 0,
      running: 68,
      failed: 0,
      escalated: 0,
      aborted: 0,
    });
    const affected = fixtures.runs.runs[0];
    affected.currentStage = "query-backlog";
    fixtures.runDetails = {
      [affected.id]: {
        ...affected,
        currentStage: "review",
        lastSeq: affected.lastSeq + 1,
        graphStatus: "unavailable",
        transitionsStatus: "unavailable",
      },
    };
    const client = new PushableClient(fixtures);
    const originalGetRun = client.getRun.bind(client);
    const getRun = vi
      .spyOn(client, "getRun")
      .mockRejectedValueOnce(new Error("detail refresh failed"))
      .mockImplementation(originalGetRun);
    const user = userEvent.setup();
    render(<App client={client} />);

    await screen.findByRole("region", { name: "Run history" });
    await user.click(screen.getByRole("button", { name: "Load more runs" }));
    const row = await screen.findByRole("link", { name: `Open run ${affected.id}` });

    act(() => {
      client.push(runEvent("session:failed-target", [affected.id]));
    });
    expect(await screen.findByRole("alert")).toHaveTextContent("detail refresh failed");
    expect(within(row).getByText("query-backlog")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(within(row).getByText("review")).toBeInTheDocument());
    expect(getRun).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  // The counterpart: a real filter change MUST still reset pagination, or the
  // fix above would trade one bug for another — showing rows that do not match
  // the selected filter.
  it("still resets pagination when the filter changes", async () => {
    const fixtures = largeJournalFixtures({
      completed: 68,
      running: 51,
      failed: 0,
      escalated: 0,
      aborted: 0,
    });
    const completedRun = fixtures.runs.runs.filter((run) => run.phase === "completed").at(-1);
    const pagedInActiveRun = fixtures.runs.runs.find((run) => run.phase === "running");
    if (!completedRun || !pagedInActiveRun) {
      throw new Error("Expected completed and active pagination fixtures.");
    }
    const client = new PushableClient(fixtures);
    const listRuns = vi.spyOn(client, "listRuns");
    const user = userEvent.setup();
    render(<App client={client} />);

    const history = await screen.findByRole("region", { name: "Run history" });
    await user.click(screen.getByRole("button", { name: "Load more runs" }));
    await waitFor(() => expect(history.querySelectorAll("a")).toHaveLength(100));
    expect(
      screen.getByRole("link", { name: `Open run ${completedRun.id}` }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: `Open run ${pagedInActiveRun.id}` }),
    ).toBeInTheDocument();

    const callsBeforeFilterChange = listRuns.mock.calls.length;
    await user.click(screen.getByRole("button", { name: "active" }));

    await waitFor(() => expect(history.querySelectorAll("a")).toHaveLength(50));
    expect(
      screen.queryByRole("link", { name: `Open run ${completedRun.id}` }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: `Open run ${pagedInActiveRun.id}` }),
    ).not.toBeInTheDocument();
    expect(listRuns.mock.calls.slice(callsBeforeFilterChange)).toEqual([
      [
        { phase: "running", cursor: undefined, limit: 50, showNoWork: false },
        { signal: expect.any(AbortSignal) },
      ],
    ]);
  });
});
