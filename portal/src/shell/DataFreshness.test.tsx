import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { deriveDataFreshness } from "../liveData";
import type { ReadState } from "../api/types";

function readState(overrides: Partial<ReadState> = {}): ReadState {
  return {
    epoch: "e1",
    appliedSeq: 10,
    observedAt: "2026-08-01T00:00:00Z",
    lagSeconds: 1,
    pendingIntake: 0,
    oldestPendingSourceAge: 0,
    intakeWriteFailures: 0,
    minChangeSeq: 0,
    completeness: "complete",
    degraded: [],
    ...overrides,
  };
}

describe("data freshness derivation", () => {
  // #1928: the existing indicator reports the SSE socket, which is not how
  // current the data is. A stream can be perfectly connected to a projector ten
  // minutes behind, and can be reconnecting over data current to the second.
  it("reports a healthy instance as current despite a non-zero lag", () => {
    // The server's lagSeconds is an upper BOUND that folds in the repair
    // sweep's cycle age, so a perfectly healthy instance always reports a few
    // seconds. Treating that as "lagging" would make the warning permanent and
    // therefore meaningless.
    expect(deriveDataFreshness(readState({ lagSeconds: 4 }))).toEqual({
      kind: "current",
      lagSeconds: 4,
    });
  });

  it("reports lagging past the threshold", () => {
    const state = deriveDataFreshness(readState({ lagSeconds: 120 }));
    expect(state.kind).toBe("lagging");
  });

  // A degraded condition means lagging even at low lag: "no sweep has completed"
  // means nothing bounds the un-watermarked case at all, so a small number is
  // not evidence of health.
  it("reports lagging when a degraded condition is present at low lag", () => {
    const state = deriveDataFreshness(
      readState({ lagSeconds: 1, degraded: ["no_sweep_completed"] }),
    );
    expect(state.kind).toBe("lagging");
    if (state.kind === "lagging") {
      expect(state.degraded).toContain("no_sweep_completed");
    }
  });

  // Partial outranks lagging: a named missing partition is a stronger statement
  // than a number. The user needs to know something is absent before they need
  // to know how old the rest is.
  it("reports partial ahead of lagging", () => {
    const state = deriveDataFreshness(
      readState({
        lagSeconds: 600,
        completeness: "partial",
        missing: [{ name: "analytics", reason: "not projected", expectedBy: "2026-08-01T00:05:00Z" }],
      }),
    );
    expect(state.kind).toBe("partial");
  });

  // "partial" with no named partition is silent omission renamed, so it must
  // not render as partial — there is nothing to tell the user.
  it("does not report partial without a named partition", () => {
    const state = deriveDataFreshness(readState({ completeness: "partial" }));
    expect(state.kind).not.toBe("partial");
  });
});

describe("data freshness rendering", () => {
  // §11A: the state must not be conveyed by colour alone.
  it("carries distinct text for each state, not just a colour", async () => {
    const { PortalShellDataFreshnessProbe } = await import("./DataFreshnessProbe");
    const labels = new Set<string>();
    for (const state of [
      { kind: "current" as const, lagSeconds: 2 },
      { kind: "lagging" as const, lagSeconds: 90, degraded: ["projection_lag"] },
      {
        kind: "partial" as const,
        lagSeconds: 5,
        missing: [{ name: "analytics", reason: "queued", expectedBy: "soon" }],
      },
    ]) {
      const { unmount } = render(<PortalShellDataFreshnessProbe state={state} />);
      const node = screen.getByRole("status");
      expect(node.textContent).toBeTruthy();
      labels.add(node.textContent ?? "");
      // The mark is decorative; a screen reader must announce the words.
      expect(node.querySelector("[aria-hidden='true']")).not.toBeNull();
      unmount();
    }
    expect(labels.size).toBe(3);
  });

  it("renders nothing when no envelope was received", async () => {
    const { PortalShellDataFreshnessProbe } = await import("./DataFreshnessProbe");
    const { container } = render(<PortalShellDataFreshnessProbe state={{ kind: "unknown" }} />);
    // No envelope means a standalone read or a daemon with no read model.
    // Rendering "current" there would be a claim nobody made.
    expect(container.textContent).toBe("");
  });
});
