import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type {
  DaemonEventStream,
  DaemonUpdateEvent,
  EventStreamRequest,
  RequestOptions,
  TelemetryError,
  TelemetryErrorsOptions,
  TelemetryErrorsPage,
} from "../api/types";
import { populatedDaemonFixtures } from "../test/daemonFixtures";

beforeEach(() => {
  window.location.hash = "#/errors";
});

/**
 * A daemon client whose event stream can be driven from the test — needed
 * because the defect is specifically about what a LIVE event does to
 * already-loaded pagination state (mirrors RunsPagePagination.test.tsx's
 * PushableClient for the equivalent Runs-page defect, #1713).
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

// Builds a journal of matching-error entries larger than one page (>50, the
// page's ERRORS_PAGE_SIZE), newest first — the fixture client slices its
// `telemetryErrors.items` array as-is rather than sorting it.
function manyErrors(count: number): TelemetryError[] {
  const start = Date.parse("2026-07-18T04:00:00Z");
  return Array.from({ length: count }, (_, index) => ({
    runId: "01JZ400FAILED",
    workflow: "implementation",
    stage: "implement",
    attempt: 1,
    code: "harness.crash",
    errorClass: "unknown",
    message: `Harness exited before producing a result envelope (${index}).`,
    occurredAt: new Date(start - index * 1_000).toISOString(),
  }));
}

function errorLinks(): HTMLElement[] {
  return within(
    screen.getByRole("region", { name: "Matching error history" }),
  ).getAllByRole("link");
}

describe("errors history pagination under live events", () => {
  // #2308: mirrors #1713 (runsHistory.ts) — a live run event was clobbering
  // paged-in error rows because useErrorHistory's only refresh path reset
  // pagination unconditionally, and that same path was reused for live
  // invalidation.
  it("keeps paged-in rows when a live run event arrives", async () => {
    const fixtures = populatedDaemonFixtures();
    fixtures.telemetryErrors = { items: manyErrors(68) };
    const client = new PushableClient(fixtures);
    const user = userEvent.setup();
    render(<App client={client} />);

    await screen.findByRole("region", { name: "Matching error history" });
    expect(errorLinks()).toHaveLength(50);

    await user.click(screen.getByRole("button", { name: "Load more errors" }));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(errorLinks()).toHaveLength(68);

    // A live event lands. Before the fix this truncated the list back to 50
    // and lost the scroll position, exactly like #1713 did on the Runs page.
    await act(async () => {
      client.push(runEvent("session:live-1"));
      await new Promise((resolve) => setTimeout(resolve, 50));
    });

    expect(errorLinks()).toHaveLength(68);
  });

  // A genuinely new error must still surface — the fix must merge, not
  // ignore, what a live refresh brings back.
  it("surfaces a newly-occurred error from a live refresh", async () => {
    const fixtures = populatedDaemonFixtures();
    const errors = manyErrors(3);
    fixtures.telemetryErrors = { items: errors };
    const client = new PushableClient(fixtures);
    render(<App client={client} />);

    await screen.findByRole("region", { name: "Matching error history" });
    expect(errorLinks()).toHaveLength(3);

    errors.unshift({
      runId: "01JZ400FAILED",
      workflow: "implementation",
      stage: "implement",
      attempt: 3,
      code: "harness.crash",
      errorClass: "unknown",
      message: "A brand new failure just occurred.",
      occurredAt: "2026-07-18T05:00:00Z",
    });

    await act(async () => {
      client.push(runEvent("session:live-1"));
      await new Promise((resolve) => setTimeout(resolve, 50));
    });

    expect(await screen.findByText("A brand new failure just occurred.")).toBeInTheDocument();
    expect(errorLinks()).toHaveLength(4);
  });

  // The counterpart: a real filter change MUST still reset pagination, or the
  // fix above would trade one bug for another — showing paged-in rows that no
  // longer match the selected filter.
  it("still resets pagination when the filter changes", async () => {
    const fixtures = populatedDaemonFixtures();
    fixtures.telemetryErrors = { items: manyErrors(68) };
    const client = new PushableClient(fixtures);
    render(<App client={client} />);

    await screen.findByRole("region", { name: "Matching error history" });
    const loadMore = await screen.findByRole("button", { name: "Load more errors" });
    await act(async () => {
      loadMore.click();
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(errorLinks()).toHaveLength(68);

    // Narrows to the 11 newest entries (occurredAt one second apart, starting
    // at 04:00:00Z) without excluding all of them.
    await act(async () => {
      window.location.hash = `#/errors?since=${encodeURIComponent("2026-07-18T03:59:50Z")}`;
      await new Promise((resolve) => setTimeout(resolve, 0));
    });

    expect(errorLinks()).toHaveLength(11);
  });
});

/**
 * A client whose error listings are released by the test, one signature at a
 * time, so a response for an abandoned scope can be delivered AFTER the
 * response for the current one. The release deliberately ignores the request's
 * abort signal: the defect is about a response that had already resolved when
 * the scope changed, which no abort can call back.
 */
class DeferredErrorsClient extends PushableClient {
  private readonly gates: {
    code: string | undefined;
    resolve: () => void;
    signal: AbortSignal | undefined;
  }[] = [];

  override async listTelemetryErrors(
    request?: TelemetryErrorsOptions,
    options?: RequestOptions,
  ): Promise<TelemetryErrorsPage> {
    await new Promise<void>((resolve) => {
      this.gates.push({ code: request?.code, resolve, signal: options?.signal });
    });
    return super.listTelemetryErrors(request);
  }

  pending(code: string): { signal: AbortSignal | undefined }[] {
    return this.gates.filter((gate) => gate.code === code);
  }

  release(code: string): void {
    for (const gate of this.gates.filter((candidate) => candidate.code === code)) {
      this.gates.splice(this.gates.indexOf(gate), 1);
      gate.resolve();
    }
  }
}

function scopedError(code: string, index: number): TelemetryError {
  return {
    runId: "01JZ400FAILED",
    workflow: "implementation",
    stage: "implement",
    attempt: 1,
    code,
    errorClass: "unknown",
    message: `${code} occurrence ${index}.`,
    occurredAt: new Date(Date.parse("2026-07-18T04:00:00Z") - index * 1_000).toISOString(),
  };
}

async function releasePending(client: DeferredErrorsClient, code: string): Promise<void> {
  await waitFor(() => expect(client.pending(code).length).toBeGreaterThan(0));
  await act(async () => {
    client.release(code);
    await new Promise((resolve) => setTimeout(resolve, 10));
  });
}

function errorMessages(): string[] {
  return Array.from(
    screen.getByRole("region", { name: "Matching error history" }).querySelectorAll("small"),
    (message) => message.textContent ?? "",
  );
}

// #3656: the background refresh held its abort controller privately, so a slow
// response for the previous signature could merge into the list after the
// operator had already drilled into a different one.
describe("errors history across a scope change", () => {
  it("aborts and rejects a refresh that outlives the signature it was started for", async () => {
    const fixtures = populatedDaemonFixtures();
    fixtures.telemetryErrors = {
      items: [scopedError("harness.crash", 0), scopedError("provider.rate_limited", 1)],
    };
    const client = new DeferredErrorsClient(fixtures);
    window.location.hash = "#/errors?code=harness.crash";
    render(<App client={client} />);

    await releasePending(client, "harness.crash");
    await screen.findByText("harness.crash occurrence 0.");
    expect(errorMessages()).toEqual(["harness.crash occurrence 0."]);

    // A live event starts a background refresh for this signature, which then
    // takes its time.
    await act(async () => {
      client.push(runEvent("session:live-1"));
      await new Promise((resolve) => setTimeout(resolve, 100));
    });
    expect(client.pending("harness.crash")).toHaveLength(1);
    const stale = client.pending("harness.crash")[0];

    // The operator drills into a different signature while it is in flight.
    await act(async () => {
      window.location.hash = "#/errors?code=provider.rate_limited";
      await new Promise((resolve) => setTimeout(resolve, 10));
    });
    expect(stale.signal?.aborted).toBe(true);

    await releasePending(client, "provider.rate_limited");
    await screen.findByText("provider.rate_limited occurrence 1.");
    expect(errorMessages()).toEqual(["provider.rate_limited occurrence 1."]);

    // The stale response lands anyway — an abort cannot unschedule a promise
    // that had already resolved — and must be dropped, not merged.
    await act(async () => {
      client.release("harness.crash");
      await new Promise((resolve) => setTimeout(resolve, 10));
    });

    expect(errorMessages()).toEqual(["provider.rate_limited occurrence 1."]);
  });
});
