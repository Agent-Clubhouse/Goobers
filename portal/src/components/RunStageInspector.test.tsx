import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type {
  AttemptList,
  ArtifactContent,
  DaemonClient,
  StageAttempt,
  WorkflowGraphNode,
} from "../api/types";
import { RunStageInspector } from "./RunStageInspector";

const reviewNode: WorkflowGraphNode = { id: "review", kind: "gate", evaluator: "agentic" };

function attempt(overrides: Partial<StageAttempt>): StageAttempt {
  return {
    number: 1,
    class: "initial",
    status: "success",
    startedSeq: 1,
    finishedSeq: 2,
    durationMillis: 1500,
    artifacts: [],
    ...overrides,
  };
}

function stubClient(
  attempts: StageAttempt[],
  artifact?: ArtifactContent,
): DaemonClient {
  return {
    listStageAttempts: vi.fn(
      async (): Promise<AttemptList> => ({ runId: "run-1", stage: "review", attempts }),
    ),
    getArtifact: vi.fn(async (): Promise<ArtifactContent> => {
      if (!artifact) {
        throw new Error("no artifact");
      }
      return artifact;
    }),
  } as unknown as DaemonClient;
}

describe("run stage inspector", () => {
  it("prompts to select a node when none is chosen", () => {
    render(
      <RunStageInspector client={stubClient([])} node={undefined} runId="run-1" selectedSeq={9} />,
    );
    expect(screen.getByText("Select a node")).toBeInTheDocument();
  });

  it("loads and shows the current attempt's status, output, and artifact metadata", async () => {
    const client = stubClient([
      attempt({
        number: 1,
        status: "success",
        outputs: { verdict: "approve" },
        artifacts: [{ name: "rationale.md", digest: "sha256:abc", size: 42, mediaType: "text/markdown", recordedSeq: 2 }],
      }),
    ]);
    render(<RunStageInspector client={client} node={reviewNode} runId="run-1" selectedSeq={9} />);

    expect(await screen.findByText("success")).toBeInTheDocument();
    expect(screen.getByText("approve")).toBeInTheDocument();
    expect(screen.getByText("rationale.md")).toBeInTheDocument();
    expect(screen.getByText("sha256:abc")).toBeInTheDocument();
    expect(client.listStageAttempts).toHaveBeenCalledWith("run-1", "review", expect.anything());
  });

  it("only shows attempts started by the selected sequence", async () => {
    const client = stubClient([
      attempt({ number: 1, startedSeq: 1, finishedSeq: 2 }),
      attempt({ number: 2, startedSeq: 8, finishedSeq: 9 }),
    ]);
    render(<RunStageInspector client={client} node={reviewNode} runId="run-1" selectedSeq={5} />);

    // Attempt 2 started at seq 8, after the playhead at 5 — it must not appear.
    await waitFor(() => expect(screen.queryByText("Attempt 2")).not.toBeInTheDocument());
    // With a single visible attempt the switcher is not rendered at all.
    expect(screen.queryByText("Attempt 1")).not.toBeInTheDocument();
  });

  it("fetches and previews a textual artifact body on demand", async () => {
    const bytes = new TextEncoder().encode("# Rationale\nApproved.").buffer;
    const client = stubClient(
      [
        attempt({
          artifacts: [{ name: "rationale.md", digest: "sha256:abc", size: 20, mediaType: "text/markdown" }],
        }),
      ],
      { digest: "sha256:abc", mediaType: "text/markdown", size: 20, etag: null, bytes },
    );
    render(<RunStageInspector client={client} node={reviewNode} runId="run-1" selectedSeq={9} />);

    fireEvent.click(await screen.findByRole("button", { name: "View content" }));
    expect(await screen.findByText(/Approved\./)).toBeInTheDocument();
    expect(client.getArtifact).toHaveBeenCalledWith("run-1", "sha256:abc");
  });
});
