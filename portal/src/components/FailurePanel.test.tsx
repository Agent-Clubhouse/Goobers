import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { FailurePanel } from "./FailurePanel";

describe("FailurePanel", () => {
  it("shows structured metadata once and formats a wrapped error as a causal chain", () => {
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
    expect(screen.queryByText(message)).not.toBeInTheDocument();
    expect(screen.getByText("Error Code:").parentElement).toHaveTextContent(
      /Error Code:\s*run_failed/,
    );
    expect(screen.getByText("push-branch", { selector: "dd" })).toBeInTheDocument();
    expect(screen.getByText("1", { selector: "dd" })).toBeInTheDocument();

    const chain = screen.getByRole("list", { name: "Failure cause chain" });
    const causes = within(chain).getAllByRole("listitem");
    expect(causes).toHaveLength(7);
    expect(causes[0]).toHaveTextContent('runner: execute stage "push-branch"');
    expect(causes[4]).toHaveTextContent(
      "occupant Q:\\GitHub\\Goobers\\workcopies\\run",
    );
    expect(causes[6]).toHaveTextContent("128 of 128 slots used");
  });

  it("keeps the runner operation together as one causal layer", () => {
    render(
      <FailurePanel
        failure={{
          message:
            'runner: prepare gate "review": create read-only workspace: a rebound branch requires a writable repo workspace',
        }}
        phase="failed"
      />,
    );

    const causes = within(
      screen.getByRole("list", { name: "Failure cause chain" }),
    ).getAllByRole("listitem");
    expect(causes).toHaveLength(3);
    expect(causes[0]).toHaveTextContent('runner: prepare gate "review"');
    expect(causes[1]).toHaveTextContent("create read-only workspace");
    expect(causes[2]).toHaveTextContent(
      "a rebound branch requires a writable repo workspace",
    );
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
