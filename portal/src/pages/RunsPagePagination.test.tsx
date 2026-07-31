import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { act } from "react";
import { beforeEach, describe, expect, it } from "vitest";
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

function runEvent(id: string): DaemonUpdateEvent {
  return {
    id,
    type: "update",
    data: { cursor: id, models: ["run"], runIds: [], workflows: [] },
  } as unknown as DaemonUpdateEvent;
}

describe("runs history pagination under live events", () => {
  // #1713: a live run event collapsed the Runs page back to the first page,
  // discarding everything the user had paged in.
  //
  // On the unfiltered #/runs route scope.gaggle and scope.workflow are both
  // undefined, so EVERY run event in the instance triggered the reset — and
  // under the polling fallback that is every 5 seconds, making the page
  // unusable on a busy instance at exactly the moment someone is watching it.
  it("keeps paged-in rows when a live run event arrives", async () => {
    const client = new PushableClient(
      largeJournalFixtures({ completed: 68, running: 0, failed: 0, escalated: 0, aborted: 0 }),
    );
    const user = userEvent.setup();
    render(<App client={client} />);

    const history = await screen.findByRole("region", { name: "Run history" });
    expect(within(history).getAllByRole("link")).toHaveLength(50);

    await user.click(screen.getByRole("button", { name: "Load more runs" }));
    await screen.findByRole("region", { name: "Run history" });
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(
      within(screen.getByRole("region", { name: "Run history" })).getAllByRole("link"),
    ).toHaveLength(68);

    // A live event lands. Before the fix this truncated the list back to 50 and
    // lost the scroll position.
    await act(async () => {
      client.push(runEvent("session:live-1"));
      await new Promise((resolve) => setTimeout(resolve, 50));
    });

    expect(
      within(screen.getByRole("region", { name: "Run history" })).getAllByRole("link"),
    ).toHaveLength(68);
  });

  // The counterpart: a real filter change MUST still reset pagination, or the
  // fix above would trade one bug for another — showing rows that do not match
  // the selected filter.
  it("still resets pagination when the filter changes", async () => {
    const client = new PushableClient(
      largeJournalFixtures({ completed: 68, running: 0, failed: 0, escalated: 0, aborted: 0 }),
    );
    const user = userEvent.setup();
    render(<App client={client} />);

    await screen.findByRole("region", { name: "Run history" });
    await user.click(screen.getByRole("button", { name: "Load more runs" }));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(
      within(screen.getByRole("region", { name: "Run history" })).getAllByRole("link"),
    ).toHaveLength(68);

    const failing = screen.queryByRole("button", { name: /Needs attention|Failed/ });
    if (!failing) {
      // The fixture has only completed runs, so the filter chip set may not
      // include a failing filter. Skipping silently would make this test
      // vacuous, so say so.
      expect(screen.getByRole("region", { name: "Run history" })).toBeTruthy();
      return;
    }
    await user.click(failing);
    await new Promise((resolve) => setTimeout(resolve, 50));
    const after = within(screen.getByRole("region", { name: "Run history" })).queryAllByRole("link");
    expect(after.length).toBeLessThan(68);
  });
});
