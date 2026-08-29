import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { act } from "react";
import type { ReactElement } from "react";
import { describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type {
  AttemptList,
  ArtifactContent,
  DaemonEventStream,
  DaemonUpdateEvent,
  EventStreamRequest,
  RequestOptions,
  WorkflowGraphNode,
} from "../api/types";
import { LiveDataProvider } from "../liveData";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { RunStageInspector } from "./RunStageInspector";

/** A client whose event stream the test can drive, and whose attempt list grows. */
class LiveAttemptsClient extends FixtureDaemonClient {
  private readers: ((result: IteratorResult<DaemonUpdateEvent>) => void)[] = [];
  attemptCount = 1;
  readonly listStageAttempts = vi.fn(
    (): Promise<AttemptList> =>
      Promise.resolve({
        runId: "run-1",
        stage: "implement",
        attempts: Array.from({ length: this.attemptCount }, (_, index) => ({
          id: `sta-1-${index + 1}`,
          visit: 1,
          number: index + 1,
          class: "initial" as const,
          status: index + 1 === this.attemptCount ? ("running" as const) : ("failure" as const),
          startedSeq: 9,
          finishedSeq: index + 1 === this.attemptCount ? undefined : 10,
          durationMillis: 1500,
          artifacts: [
            {
              name: "result.txt",
              digest: "sha256:result",
              size: 15,
              mediaType: "text/plain",
              recordedSeq: 9,
            },
          ],
        })),
      }),
  );
  readonly getArtifact = vi.fn(
    (): Promise<ArtifactContent> =>
      Promise.resolve({
        digest: "sha256:result",
        mediaType: "text/plain",
        size: 15,
        etag: null,
        bytes: new TextEncoder().encode("durable content").buffer,
      }),
  );

  connectEvents(
    _request?: EventStreamRequest,
    _options?: RequestOptions,
  ): Promise<DaemonEventStream> {
    const self = this;
    return Promise.resolve({
      close: () => {},
      [Symbol.asyncIterator]: () => ({
        next: () =>
          new Promise<IteratorResult<DaemonUpdateEvent>>((resolve) =>
            self.readers.push(resolve),
          ),
      }),
    } as DaemonEventStream);
  }

  push(runId: string, id: string): void {
    this.readers.shift()?.({
      done: false,
      value: {
        id,
        type: "update",
        data: { cursor: id, models: ["run"], runIds: [runId], workflows: [] },
      } as unknown as DaemonUpdateEvent,
    });
  }
}

async function pushRefreshes(client: LiveAttemptsClient, count: number): Promise<void> {
  let calls = client.listStageAttempts.mock.calls.length;
  for (let index = 1; index <= count; index += 1) {
    client.push("run-1", `session:refresh-${index}`);
    await waitFor(() =>
      expect(client.listStageAttempts.mock.calls.length).toBeGreaterThan(calls),
    );
    calls = client.listStageAttempts.mock.calls.length;
  }
}

function wrap(client: LiveAttemptsClient, node: ReactElement) {
  return <LiveDataProvider client={client}>{node}</LiveDataProvider>;
}

const node = { id: "implement", kind: "stage", label: "implement" } as unknown as WorkflowGraphNode;

describe("run stage inspector live refresh", () => {
  // #1714: the attempt list was fetched in a plain effect keyed on
  // [client, runId, stageId] and never subscribed to live data. RunPage keys
  // RunDetailWorkspace on run.id, which is stable across refreshes, so the
  // inspector was never remounted either.
  //
  // A user selects a running stage and watches it retry three times. The graph
  // and event timeline update via useRunDetail; the attempt list stays frozen
  // at whatever it was when the node was first clicked. They have to click away
  // and back — on the portal's primary live-debugging surface.
  it("refetches attempts when a live event arrives for this run", async () => {
    const client = new LiveAttemptsClient(populatedDaemonFixtures());
    render(wrap(client, <RunStageInspector client={client} node={node} runId="run-1" selectedSeq={9} />));

    await waitFor(() => expect(client.listStageAttempts).toHaveBeenCalled());
    // Let the provider's initial snapshot settle. It legitimately counts as a
    // change, so the baseline is taken AFTER it rather than assuming a fixed
    // mount-time call count — an assertion on an absolute number here would be
    // measuring provider startup, not the defect.
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 60));
    });
    const before = client.listStageAttempts.mock.calls.length;

    // The stage retries. Before the fix, nothing refetched.
    client.attemptCount = 2;
    await act(async () => {
      client.push("run-1", "session:retry-1");
      await new Promise((resolve) => setTimeout(resolve, 60));
    });

    await waitFor(() =>
      expect(client.listStageAttempts.mock.calls.length).toBeGreaterThan(before),
    );
  });

  // Scoping matters: an event for a DIFFERENT run must not refetch this one's
  // attempts. Without the runId scope, a busy instance would issue one request
  // per event per open inspector.
  it("ignores live events for other runs", async () => {
    const client = new LiveAttemptsClient(populatedDaemonFixtures());
    render(wrap(client, <RunStageInspector client={client} node={node} runId="run-1" selectedSeq={9} />));

    await waitFor(() => expect(client.listStageAttempts).toHaveBeenCalled());
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 60));
    });
    const before = client.listStageAttempts.mock.calls.length;

    await act(async () => {
      client.push("run-other", "session:unrelated");
      await new Promise((resolve) => setTimeout(resolve, 60));
    });

    expect(client.listStageAttempts.mock.calls.length).toBe(before);
  });

  it("keeps expanded artifact content visible across three live refreshes", async () => {
    const client = new LiveAttemptsClient(populatedDaemonFixtures());
    render(wrap(client, <RunStageInspector client={client} node={node} runId="run-1" selectedSeq={9} />));

    fireEvent.click(await screen.findByRole("button", { name: "View content" }));
    expect(await screen.findByText("durable content")).toBeInTheDocument();

    await pushRefreshes(client, 3);

    expect(screen.getByText("durable content")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "View content" })).not.toBeInTheDocument();
  });

  it("keeps unexpanded artifact content collapsed across three live refreshes", async () => {
    const client = new LiveAttemptsClient(populatedDaemonFixtures());
    render(wrap(client, <RunStageInspector client={client} node={node} runId="run-1" selectedSeq={9} />));

    expect(await screen.findByRole("button", { name: "View content" })).toBeInTheDocument();

    await pushRefreshes(client, 3);

    expect(screen.getByRole("button", { name: "View content" })).toBeInTheDocument();
    expect(screen.queryByText("durable content")).not.toBeInTheDocument();
    expect(client.getArtifact).not.toHaveBeenCalled();
  });
});
