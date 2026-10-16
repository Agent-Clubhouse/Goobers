import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ChildActivityPanel } from "./ChildActivityPanel";
import type { ChildActivity } from "../api/types";

const waiting: ChildActivity = {
  status: "recorded", parked: false,
  waits: [{ runId: "child-run", stage: "inspect", branch: 1, action: "wait", since: "2026-10-09T15:00:00Z", sequence: 7 }],
};

describe("recorded child activity", () => {
  it("distinguishes a waiting stage from a fully parked run and follows its link", () => {
    const navigate = vi.fn();
    const view = render(<ChildActivityPanel activity={waiting} navigate={navigate} />);
    expect(screen.getByText("These stages are waiting on child workflows.")).toBeInTheDocument();
    expect(screen.getByText(/Awaiting child outcome/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Open child child-run" }));
    expect(navigate).toHaveBeenCalledWith({ page: "run", id: "child-run" });
    view.rerender(<ChildActivityPanel activity={{ ...waiting, parked: true }} navigate={navigate} />);
    expect(screen.getByText("This run is waiting on child workflows.")).toBeInTheDocument();
    view.rerender(<ChildActivityPanel navigate={navigate} />);
    expect(screen.queryByRole("heading", { name: "Child workflows" })).not.toBeInTheDocument();
  });

  it("links a recorded child back to its parent", () => {
    const navigate = vi.fn();
    render(<ChildActivityPanel activity={{ status: "recorded", parked: false, waits: [], parent: { runId: "parent-run", workflow: "Implementation", stageOccurrence: "plan/branch0/visit1" } }} navigate={navigate} />);
    fireEvent.click(screen.getByRole("button", { name: "Implementation" }));
    expect(navigate).toHaveBeenCalledWith({ page: "run", id: "parent-run" });
    expect(screen.queryByText(/This run is waiting/)).not.toBeInTheDocument();
  });

  it("shows unavailable evidence without presenting stale links", () => {
    render(<ChildActivityPanel activity={{ ...waiting, status: "unavailable" }} navigate={vi.fn()} />);
    expect(screen.getByRole("status")).toHaveTextContent("Recorded child relationships are unavailable");
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });
});
