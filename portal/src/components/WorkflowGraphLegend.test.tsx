import { fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { WorkflowGraphLegend } from "./WorkflowGraphLegend";

describe("WorkflowGraphLegend (#1431)", () => {
  it("is collapsed by default and expands via its native disclosure", () => {
    render(<WorkflowGraphLegend />);
    const details = screen.getByText("Graph legend").closest("details");
    expect(details).not.toBeNull();
    expect(details).not.toHaveAttribute("open");

    fireEvent.click(screen.getByText("Graph legend"));
    expect(details).toHaveAttribute("open");
  });

  it("explains line style (repass) independently from execution emphasis (traversed)", () => {
    render(<WorkflowGraphLegend />);
    const edgeKey = screen.getByRole("list", { name: "Edge state key" });
    // Declared/dim and traversed/emphasized are named as the execution axis.
    expect(within(edgeKey).getByText(/not yet traversed/i)).toBeInTheDocument();
    expect(within(edgeKey).getByText(/^Traversed as of the selected sequence/i)).toBeInTheDocument();
    // Repass/back-edge is named as a SEPARATE, declared-shape axis: dashed
    // regardless of whether it was taken — the exact distinction #1430 fixed
    // rendering for and #1431 exists to explain in words.
    const repassItem = within(edgeKey).getByText(/Repass \/ back-edge/i);
    expect(repassItem.textContent).toMatch(/independent of whether it was ever taken/i);
    expect(repassItem.textContent).toMatch(/untaken repass stays dashed and dim/i);
  });

  it("explains every node run-state, including pending and skipped", () => {
    render(<WorkflowGraphLegend />);
    const nodeKey = screen.getByRole("list", { name: "Node state key" });
    for (const label of [
      "Pending",
      "Running",
      "Completed",
      "Failed",
      "Blocked",
      "Aborted",
      "Escalated",
    ]) {
      expect(within(nodeKey).getByText(label)).toBeInTheDocument();
    }
    const skipped = within(nodeKey).getByText(/^Skipped/);
    expect(skipped.textContent).toMatch(/no-work node the run ended without visiting/);
  });

  it("pairs every sample with a non-color text label — no swatch stands alone", () => {
    render(<WorkflowGraphLegend />);
    const edgeItems = within(screen.getByRole("list", { name: "Edge state key" })).getAllByRole(
      "listitem",
    );
    const nodeItems = within(screen.getByRole("list", { name: "Node state key" })).getAllByRole(
      "listitem",
    );
    const branchItems = within(
      screen.getByRole("list", { name: "Branch state key" }),
    ).getAllByRole("listitem");
    expect(edgeItems).toHaveLength(3);
    expect(nodeItems).toHaveLength(8);
    expect(branchItems).toHaveLength(6);
    for (const item of [...edgeItems, ...nodeItems, ...branchItems]) {
      // Every sample is aria-hidden (a decorative shape/style cue) and is
      // always accompanied by real text content in the same list item.
      const sample = item.querySelector("[aria-hidden='true']");
      expect(sample).not.toBeNull();
      expect(item.textContent?.trim().length).toBeGreaterThan(0);
    }
  });

  it("explains every declared-branch state (#1567), distinguishing cancelled from failed by more than color", () => {
    render(<WorkflowGraphLegend />);
    const branchKey = screen.getByRole("list", { name: "Branch state key" });
    for (const label of [
      "Running",
      "Succeeded",
      "Failed",
      "Timed out",
      "Cancelled",
      "No output",
    ]) {
      expect(within(branchKey).getByText(label)).toBeInTheDocument();
    }
    const failedSample = within(branchKey)
      .getByText("Failed")
      .closest("li")
      ?.querySelector("path");
    const cancelledSample = within(branchKey)
      .getByText("Cancelled")
      .closest("li")
      ?.querySelector("path");
    expect(failedSample?.getAttribute("class")).not.toBe(cancelledSample?.getAttribute("class"));
  });

  it("is keyboard reachable: the disclosure is a native, focusable summary", () => {
    render(<WorkflowGraphLegend />);
    const summary = screen.getByText("Graph legend");
    expect(summary.tagName).toBe("SUMMARY");
    summary.focus();
    expect(summary).toHaveFocus();
  });
});
