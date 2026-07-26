import { act, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { RunEvent } from "../api/types";
import { ReplayScrubber } from "./ReplayScrubber";

function ev(
  seq: number,
  time: string,
  fields: Partial<RunEvent> = {},
): RunEvent {
  return {
    schema: "v1",
    seq,
    type: "stage.started",
    branch: 0,
    time,
    knownSchema: true,
    category: "transition",
    replayChapter: true,
    ...fields,
  };
}

function Harness({
  events,
  initial,
  terminal,
  onSeek,
}: {
  events: RunEvent[];
  initial: number;
  terminal: boolean;
  onSeek?: (seq: number) => void;
}) {
  const [seq, setSeq] = useState(initial);
  return (
    <ReplayScrubber
      events={events}
      onSeek={(next) => {
        onSeek?.(next);
        setSeq(next);
      }}
      selectedSeq={seq}
      terminal={terminal}
    />
  );
}

afterEach(() => {
  vi.useRealTimers();
});

describe("replay scrubber", () => {
  it("plays every durable event forward on the compressed timeline", () => {
    vi.useFakeTimers();
    const onSeek = vi.fn();
    render(
      <Harness
        events={[
          ev(1, "2026-01-01T00:00:00Z"),
          ev(2, "2026-01-01T00:00:01Z", {
            type: "artifact.recorded",
            category: "evidence",
            replayChapter: false,
          }),
        ]}
        initial={1}
        onSeek={onSeek}
        terminal
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Play replay" }));
    act(() => {
      vi.advanceTimersByTime(2_000);
    });
    expect(onSeek).toHaveBeenCalledWith(2);
    expect(screen.getByText(/Raw event 2 of 2 · Sequence 2/)).toBeInTheDocument();
  });

  it("scrubs by compressed elapsed time and supports keyboard event stepping", () => {
    const onSeek = vi.fn();
    render(
      <Harness
        events={[
          ev(1, "2026-01-01T00:00:00Z"),
          ev(2, "2026-01-01T00:00:01Z"),
          ev(3, "2026-01-01T00:00:02Z"),
        ]}
        initial={1}
        onSeek={onSeek}
        terminal
      />,
    );
    const timeline = screen.getByRole("slider", { name: "Scrub replay timeline" });
    fireEvent.change(timeline, { target: { value: "2000" } });
    expect(onSeek).toHaveBeenLastCalledWith(3);

    fireEvent.keyDown(timeline, { key: "ArrowLeft" });
    expect(onSeek).toHaveBeenLastCalledWith(2);
  });

  it("skips evidence and liveness noise between chapters while retaining raw steps", () => {
    const onSeek = vi.fn();
    render(
      <Harness
        events={[
          ev(1, "2026-01-01T00:00:00Z", { type: "run.started" }),
          ev(2, "2026-01-01T00:00:01Z", {
            type: "artifact.recorded",
            category: "evidence",
            replayChapter: false,
          }),
          ev(3, "2026-01-01T00:00:02Z", {
            type: "stage.heartbeat",
            category: "liveness",
            replayChapter: false,
          }),
          ev(4, "2026-01-01T00:00:03Z", {
            type: "gate.evaluated",
            category: "decision",
            gate: "review",
            verdict: "approve",
          }),
        ]}
        initial={1}
        onSeek={onSeek}
        terminal
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Next chapter" }));
    expect(onSeek).toHaveBeenLastCalledWith(4);
    fireEvent.click(screen.getByRole("button", { name: "Previous raw event" }));
    expect(onSeek).toHaveBeenLastCalledWith(3);
    fireEvent.click(screen.getByRole("button", { name: "Previous chapter" }));
    expect(onSeek).toHaveBeenLastCalledWith(1);
  });

  it("makes chapter landmarks keyboard reachable and identifies their shape in text", () => {
    render(
      <Harness
        events={[
          ev(1, "2026-01-01T00:00:00Z", {
            type: "gate.evaluated",
            category: "decision",
            gate: "review",
            verdict: "approve",
          }),
        ]}
        initial={1}
        terminal
      />,
    );
    const chapter = screen.getByRole("button", {
      name: /Go to Gate decision chapter/,
    });
    chapter.focus();
    expect(chapter).toHaveFocus();
    expect(chapter).toHaveAttribute("aria-current", "step");
  });

  it("discloses and exposes each compressed idle period for inspection", () => {
    render(
      <Harness
        events={[
          ev(1, "2026-01-01T00:00:00Z"),
          ev(2, "2026-01-01T00:01:00Z"),
        ]}
        initial={1}
        terminal
      />,
    );
    expect(screen.getByText("Idle compression on")).toBeInTheDocument();
    expect(
      screen.getByRole("note", {
        name: /Compressed idle gap between sequences 1 and 2: 1m shown as 1.5s/,
      }),
    ).toBeInTheDocument();
  });

  it.each([
    {
      name: "first-pass run",
      events: [
        ev(1, "2026-01-01T00:00:00Z", { type: "run.started" }),
        ev(2, "2026-01-01T00:00:01Z", {
          type: "ref.touched",
          category: "result",
          externalRef: { provider: "github", kind: "pr", id: "42" },
        }),
        ev(3, "2026-01-01T00:00:02Z", {
          type: "gate.evaluated",
          category: "decision",
          gate: "review",
          verdict: "approve",
        }),
        ev(4, "2026-01-01T00:00:03Z", {
          type: "run.finished",
          status: "completed",
        }),
      ],
      landmarks: ["External result", "Gate decision", "Terminal outcome"],
    },
    {
      name: "reviewer repass",
      events: [
        ev(1, "2026-01-01T00:00:00Z", { type: "run.started" }),
        ev(2, "2026-01-01T00:00:01Z", {
          type: "gate.evaluated",
          category: "decision",
          gate: "review",
          verdict: "needs-changes",
          target: "implement",
        }),
        ev(3, "2026-01-01T00:00:02Z", {
          type: "stage.started",
          stage: "implement",
          attempt: 2,
          attemptClass: "policy",
        }),
        ev(4, "2026-01-01T00:00:03Z", {
          type: "run.finished",
          status: "completed",
        }),
      ],
      landmarks: ["Gate decision", "Workflow transition", "Terminal outcome"],
    },
    {
      name: "failure",
      events: [
        ev(1, "2026-01-01T00:00:00Z", {
          type: "stage.finished",
          stage: "implement",
          status: "failure",
        }),
        ev(2, "2026-01-01T00:00:01Z", {
          type: "run.finished",
          status: "failed",
        }),
      ],
      landmarks: ["Failure", "Terminal outcome"],
    },
    {
      name: "escalation",
      events: [
        ev(1, "2026-01-01T00:00:00Z", {
          type: "gate.evaluated",
          category: "decision",
          gate: "review",
          verdict: "fail",
          target: "@escalate",
          escalated: true,
        }),
        ev(2, "2026-01-01T00:00:01Z", {
          type: "run.finished",
          status: "escalated",
        }),
      ],
      landmarks: ["Escalation", "Terminal outcome"],
    },
    {
      name: "no-work run",
      events: [
        ev(1, "2026-01-01T00:00:00Z", { type: "run.started" }),
        ev(2, "2026-01-01T00:00:01Z", {
          type: "stage.finished",
          stage: "query",
          status: "no-work",
        }),
        ev(3, "2026-01-01T00:00:02Z", {
          type: "run.finished",
          status: "completed",
        }),
      ],
      landmarks: ["Workflow transition", "Terminal outcome"],
    },
  ])("renders semantic landmarks for a $name", ({ events, landmarks }) => {
    render(<Harness events={events} initial={events[0].seq} terminal />);
    for (const landmark of landmarks) {
      expect(
        screen.getAllByRole("button", {
          name: new RegExp(`Go to ${landmark} chapter`),
        }).length,
      ).toBeGreaterThan(0);
    }
  });

  it("does not stall at a live run's end and resumes when events arrive", () => {
    vi.useFakeTimers();
    const onSeek = vi.fn();
    const events = [
      ev(1, "2026-01-01T00:00:00Z"),
      ev(2, "2026-01-01T00:00:01Z"),
    ];
    const { rerender } = render(
      <Harness events={events} initial={2} onSeek={onSeek} terminal={false} />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Play replay" }));
    act(() => {
      vi.advanceTimersByTime(10_000);
    });
    expect(onSeek).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Pause replay" })).toBeInTheDocument();

    rerender(
      <Harness
        events={[...events, ev(3, "2026-01-01T00:00:02Z")]}
        initial={2}
        onSeek={onSeek}
        terminal={false}
      />,
    );
    act(() => {
      vi.advanceTimersByTime(2_000);
    });
    expect(onSeek).toHaveBeenCalledWith(3);
  });

  it("stops cleanly at a finished run's end", () => {
    vi.useFakeTimers();
    render(
      <Harness
        events={[
          ev(1, "2026-01-01T00:00:00Z"),
          ev(2, "2026-01-01T00:00:01Z"),
        ]}
        initial={1}
        terminal
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Play replay" }));
    act(() => {
      vi.advanceTimersByTime(10_000);
    });
    expect(screen.getByText(/Sequence 2/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Play replay" })).toBeInTheDocument();
  });
});
