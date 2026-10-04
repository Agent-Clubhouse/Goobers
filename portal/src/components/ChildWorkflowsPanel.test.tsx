import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { ChildWorkflowPage, ChildWorkflowSummary } from "../api/types";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { ChildWorkflowsPanel } from "./ChildWorkflowsPanel";

const child: ChildWorkflowSummary = { childId: "child-one", runId: "reserved-run", runAvailable: false, stage: "plan", workflow: "inspect", invocationKey: "inspect-one", sequence: 1, state: "queued", cancellationRequested: false, acknowledged: false, expired: false, acceptedAt: "2026-10-04T12:00:00Z", updatedAt: "2026-10-04T12:00:00Z" };
const page: ChildWorkflowPage = { runId: "parent", gaggle: "own", children: [child] };

describe("ChildWorkflowsPanel", () => {
  it("separates queue custody, cancellation and actual run links across pages", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const read = vi.spyOn(client, "getChildWorkflows").mockResolvedValueOnce({ ...page, nextCursor: "cursor-one" }).mockResolvedValueOnce({ ...page, parent: { runId: "upstream", workflow: "plan", invocationKey: "inspect" }, children: [{ ...child, state: "completed", runAvailable: true, cancellationRequested: true }] });
    render(<ChildWorkflowsPanel client={client} runId="parent" />);
    expect(await screen.findByText("Queued")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Open child run" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Next children" }));
    expect(await screen.findByText("Completed")).toBeInTheDocument();
    expect(screen.getByText("Returned result awaits parent acknowledgement.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open child run" })).toHaveAttribute("href", "#/run/reserved-run");
    expect(screen.getByRole("link", { name: "plan · parent run" })).toHaveAttribute("href", "#/run/upstream");
    expect(read).toHaveBeenLastCalledWith("parent", "cursor-one", expect.objectContaining({ signal: expect.any(AbortSignal) }));
  });

  it("clears previously visible family data when a refresh is refused", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const read = vi.spyOn(client, "getChildWorkflows").mockResolvedValueOnce({ ...page, children: [{ ...child, runAvailable: true }] }).mockRejectedValueOnce(new Error("revoked"));
    render(<ChildWorkflowsPanel client={client} runId="parent" />);
    await screen.findByRole("link", { name: "Open child run" });
    fireEvent.click(screen.getByRole("button", { name: "Refresh children" }));
    await screen.findByText("Child workflow information is unavailable. Check your access or try again.");
    expect(screen.queryByRole("link", { name: "Open child run" })).not.toBeInTheDocument();
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
  });
});
