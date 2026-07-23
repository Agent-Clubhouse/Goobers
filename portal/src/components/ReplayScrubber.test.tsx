import { act, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { RunEvent } from "../api/types";
import { ReplayScrubber } from "./ReplayScrubber";

function ev(seq: number, time: string): RunEvent {
  return { schema: "v1", seq, type: "stage.started", branch: 0, time, knownSchema: true } as RunEvent;
}

// Harness feeds each seek back as the selected sequence, exactly as the run
// page does, so the play loop can actually advance.
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
  it("plays forward on a timer, driving the selected sequence", () => {
    vi.useFakeTimers();
    const onSeek = vi.fn();
    render(
      <Harness
        events={[ev(1, "2026-01-01T00:00:00Z"), ev(2, "2026-01-01T00:00:01Z")]}
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
    expect(screen.getByText("Event 2 of 2")).toBeInTheDocument();
  });

  it("scrubs directly to an event via the range input", () => {
    const onSeek = vi.fn();
    render(
      <Harness
        events={[ev(1, "2026-01-01T00:00:00Z"), ev(2, "2026-01-01T00:00:01Z"), ev(3, "2026-01-01T00:00:02Z")]}
        initial={1}
        onSeek={onSeek}
        terminal
      />,
    );
    fireEvent.change(screen.getByRole("slider", { name: "Scrub to event" }), { target: { value: "2" } });
    expect(onSeek).toHaveBeenCalledWith(3);
  });

  it("does not stall at a live run's end — it resumes when new events arrive", () => {
    vi.useFakeTimers();
    const onSeek = vi.fn();
    const events = [ev(1, "2026-01-01T00:00:00Z"), ev(2, "2026-01-01T00:00:01Z")];
    const { rerender } = render(<Harness events={events} initial={2} onSeek={onSeek} terminal={false} />);

    fireEvent.click(screen.getByRole("button", { name: "Play replay" }));
    act(() => {
      vi.advanceTimersByTime(10_000);
    });
    // Waiting at the current end of a live run: no advance, and still playing
    // (a stall would have left it stuck; here it simply waits for more).
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
        events={[ev(1, "2026-01-01T00:00:00Z"), ev(2, "2026-01-01T00:00:01Z")]}
        initial={1}
        terminal
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Play replay" }));
    act(() => {
      vi.advanceTimersByTime(10_000);
    });
    // Advanced to the terminal end, then paused on its own.
    expect(screen.getByText("Event 2 of 2")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Play replay" })).toBeInTheDocument();
  });
});
