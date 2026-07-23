import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { EscalationCause, RunEvent } from "../api/types";
import { EscalationPanel } from "./EscalationPanel";

const cause: EscalationCause = {
  selector: { kind: "gate", name: "review" },
  selectedBranch: "@escalate",
  repassCount: 2,
  retryCount: 1,
  terminalReason: "repass budget exhausted",
  causalEventSeq: 12,
};

const causalEvent: RunEvent = {
  schema: "v1",
  seq: 12,
  type: "gate.evaluated",
  branch: 0,
  time: "2026-01-01T00:00:12Z",
  knownSchema: true,
  gate: "review",
  verdict: "needs-changes",
};

describe("escalation panel", () => {
  it("surfaces the authoritative cause: reason, gate, branch, and budget", () => {
    render(<EscalationPanel escalation={cause} />);
    expect(screen.getByText("authoritative cause", { exact: false })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "repass budget exhausted" })).toBeInTheDocument();
    expect(screen.getByText("review")).toBeInTheDocument();
    expect(screen.getByText("@escalate")).toBeInTheDocument();
    expect(screen.getByText("2 repass · 1 retry")).toBeInTheDocument();
  });

  it("composes a summary from the selector when there is no terminal reason", () => {
    render(<EscalationPanel escalation={{ ...cause, terminalReason: undefined }} />);
    expect(
      screen.getByRole("heading", { name: "Gate review escalated the run via @escalate." }),
    ).toBeInTheDocument();
  });

  it("links to the causal event and focuses it on click", () => {
    const onFocus = vi.fn();
    render(
      <EscalationPanel causalEvent={causalEvent} escalation={cause} onFocusCausalEvent={onFocus} />,
    );
    fireEvent.click(screen.getByRole("button", { name: /Causal event/ }));
    expect(onFocus).toHaveBeenCalled();
    expect(screen.getByText(/Seq 12/)).toBeInTheDocument();
  });

  it("marks the causal event unavailable when it cannot be resolved", () => {
    render(<EscalationPanel escalation={cause} />);
    expect(screen.getByText(/Seq 12 · Unavailable/)).toBeInTheDocument();
  });
});
