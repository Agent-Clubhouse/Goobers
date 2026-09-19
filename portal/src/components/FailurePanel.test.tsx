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
    expect(screen.getByText(message, { selector: "pre" })).toBeInTheDocument();
    expect(screen.getAllByText("run_failed")).toHaveLength(1);
    expect(screen.getByText("push-branch", { selector: "dd" })).toBeInTheDocument();
    expect(screen.getByText("1", { selector: "dd" })).toBeInTheDocument();

    const chain = screen.getByRole("list", { name: "Failure cause chain" });
    const causes = within(chain).getAllByRole("listitem");
    expect(causes).toHaveLength(8);
    expect(causes[1]).toHaveTextContent('execute stage "push-branch"');
    expect(causes[5]).toHaveTextContent(
      "occupant Q:\\GitHub\\Goobers\\workcopies\\run",
    );
    expect(causes[7]).toHaveTextContent("128 of 128 slots used");

    const rawDetails = screen.getByText("Show raw failure details");
    expect(rawDetails).toBeInTheDocument();
    expect(rawDetails.closest("details")).not.toHaveAttribute("open");
    expect(
      within(rawDetails.closest("details") as HTMLElement).getByRole("button", {
        name: "Copy raw details",
      }),
    ).toBeInTheDocument();
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
    expect(
      screen.getByText("The daemon stopped before recording a result.", { selector: "p" }),
    ).toBeInTheDocument();
  });
});
