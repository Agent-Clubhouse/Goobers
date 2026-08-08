import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { ArtifactContent, DaemonClient, RunEvent } from "../api/types";
import { KeyMomentsDigest } from "./KeyMomentsDigest";

function event(seq: number, type: RunEvent["type"], fields: Partial<RunEvent>): RunEvent {
  return {
    schema: "v1",
    seq,
    type,
    branch: 0,
    time: `2026-07-18T00:00:${String(seq).padStart(2, "0")}Z`,
    knownSchema: true,
    ...fields,
  };
}

function stubClient(artifact?: ArtifactContent): DaemonClient {
  return {
    getArtifact: vi.fn(async (): Promise<ArtifactContent> => {
      if (!artifact) {
        throw new Error("no artifact");
      }
      return artifact;
    }),
  } as unknown as DaemonClient;
}

const events: RunEvent[] = [
  event(1, "run.started", { category: "transition" }),
  event(2, "gate.started", { category: "bookkeeping", gate: "review" }),
  event(3, "artifact.recorded", {
    category: "evidence",
    artifact: {
      name: "verdict/review-1.json",
      digest: "sha256:verdict-1",
      size: 40,
      mediaType: "application/json",
      stage: "review",
    },
  }),
  event(4, "gate.evaluated", {
    category: "decision",
    gate: "review",
    verdict: "needs-changes",
    target: "implement",
  }),
  event(5, "stage.finished", { category: "transition", stage: "implement", escalated: true }),
];

describe("KeyMomentsDigest", () => {
  it("lists only the significant moments, most significant first", () => {
    render(
      <KeyMomentsDigest
        client={stubClient()}
        events={events}
        onSelect={vi.fn()}
        runId="run-1"
        runStartedAt="2026-07-18T00:00:00Z"
        selectedSeq={4}
      />,
    );

    const rows = screen.getAllByRole("button", { name: /key moment sequence/i });
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveAccessibleName(/key moment sequence 5:/i);
    expect(rows[1]).toHaveAccessibleName(/key moment sequence 4:/i);
  });

  it("reports the run's empty state when nothing significant happened", () => {
    render(
      <KeyMomentsDigest
        client={stubClient()}
        events={[event(1, "run.started", { category: "transition" })]}
        onSelect={vi.fn()}
        runId="run-1"
        runStartedAt="2026-07-18T00:00:00Z"
        selectedSeq={1}
      />,
    );

    expect(
      screen.getByText("No decisions, gate evaluations, or handoffs recorded yet"),
    ).toBeInTheDocument();
  });

  it("selects the underlying event and expands its payload inline on click, without a graph lookup", async () => {
    const onSelect = vi.fn();
    const artifact: ArtifactContent = {
      digest: "sha256:verdict-1",
      mediaType: "application/json",
      size: 28,
      etag: null,
      bytes: new TextEncoder().encode('{"verdict":"needs-changes"}').buffer as ArrayBuffer,
    };
    render(
      <KeyMomentsDigest
        client={stubClient(artifact)}
        events={events}
        onSelect={onSelect}
        runId="run-1"
        runStartedAt="2026-07-18T00:00:00Z"
        selectedSeq={1}
      />,
    );

    const decisionRow = screen.getByRole("button", { name: /key moment sequence 4:/i });
    fireEvent.click(decisionRow);

    expect(onSelect).toHaveBeenCalledWith(events[3]);
    expect(decisionRow).toHaveAttribute("aria-expanded", "true");

    fireEvent.click(screen.getByRole("button", { name: "View content" }));
    expect(await screen.findByText('{"verdict":"needs-changes"}')).toBeInTheDocument();

    fireEvent.click(decisionRow);
    expect(decisionRow).toHaveAttribute("aria-expanded", "false");
  });
});
