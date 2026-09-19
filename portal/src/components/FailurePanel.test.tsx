import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { FailurePanel } from "./FailurePanel";

describe("FailurePanel", () => {
  it("shows structured metadata once and preserves the complete failure reason", () => {
    const message =
      'runner: execute stage "push-branch": prepare stage "push-branch": create worktree: reconcile released branch "goobers/implementation/example": occupant Q:\\GitHub\\Goobers\\workcopies\\run: recovery inventory is full: 128 of 128 slots used';

    render(
      <FailurePanel
        failure={{
          attempt: 1,
          code: "run_failed",
          message,
          stage: "push-branch",
        }}
        phase="failed"
      />,
    );

    expect(screen.getByRole("heading", { name: "Run failed" })).toBeInTheDocument();
    expect(screen.queryByText(/Attention · Failure/)).not.toBeInTheDocument();
    expect(screen.getByText(message)).toBeInTheDocument();
    expect(screen.getByText("Error Code:").parentElement).toHaveTextContent(
      /Error Code:\s*run_failed/,
    );
    expect(screen.getByText("push-branch", { selector: "dd" })).toBeInTheDocument();
    expect(screen.getByText("1", { selector: "dd" })).toBeInTheDocument();
  });

  it("renders an unwrapped reason as plain text", () => {
    render(
      <FailurePanel
        failure={{ message: "The daemon stopped before recording a result." }}
        phase="aborted"
      />,
    );

    expect(screen.getByRole("heading", { name: "Run aborted" })).toBeInTheDocument();
    expect(screen.queryByRole("list", { name: "Failure cause chain" })).not.toBeInTheDocument();
    expect(screen.getByText("The daemon stopped before recording a result.")).toBeInTheDocument();
  });
});
