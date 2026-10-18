import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { insightScopeKey, insightScopeOption } from "../insightScope";
import { InsightScopePicker } from "./InsightScopePicker";

const scopes = [
  insightScopeOption({ kind: "instance" }),
  insightScopeOption({ kind: "gaggle", gaggle: "core" }),
  insightScopeOption({ kind: "workflow", gaggle: "core", workflow: "implementation" }),
  insightScopeOption({ kind: "stage", gaggle: "core", workflow: "implementation", stage: "review" }),
  insightScopeOption({ kind: "gaggle", gaggle: "tools" }),
];

describe("InsightScopePicker", () => {
  it("makes the gaggle name selectable independently of expanding workflows", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<InsightScopePicker onChange={onChange} scopes={scopes} value={insightScopeKey({ kind: "instance" })} />);
    await user.click(screen.getByLabelText("Scope"));
    expect(screen.queryByRole("button", { name: "Workflow · core / implementation" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Expand Gaggle · core" }));
    expect(screen.getByRole("button", { name: "Workflow · core / implementation" })).toBeInTheDocument();
    expect(onChange).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Gaggle · core" }));
    expect(onChange).toHaveBeenCalledWith(insightScopeKey({ kind: "gaggle", gaggle: "core" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByLabelText("Scope")).toHaveFocus();
  });

  it("nests stages under their workflow and preserves ancestor context when searching", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<InsightScopePicker onChange={onChange} scopes={scopes} value={insightScopeKey({ kind: "instance" })} />);
    await user.click(screen.getByLabelText("Scope"));
    await user.type(screen.getByLabelText("Find scope"), "review");
    const workflow = screen.getByRole("button", { name: "Workflow · core / implementation" }).closest("li");
    if (!workflow) throw new Error("Expected a workflow node.");
    expect(within(workflow).getByRole("button", { name: "Stage · core / implementation / review" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Gaggle · tools" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Stage · core / implementation / review" }));
    expect(onChange).toHaveBeenCalledWith(insightScopeKey({ kind: "stage", gaggle: "core", workflow: "implementation", stage: "review" }));
  });

  it("opens the selected ancestors and dismisses with Escape or outside focus", async () => {
    const user = userEvent.setup();
    render(
      <>
        <InsightScopePicker onChange={vi.fn()} scopes={scopes} value={scopes[3].key} />
        <button type="button">Outside</button>
      </>,
    );
    await user.click(screen.getByLabelText("Scope"));
    expect(screen.getByRole("button", { name: scopes[3].label })).toHaveAttribute("aria-current", "true");
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByLabelText("Scope")).toHaveFocus();
    await user.click(screen.getByLabelText("Scope"));
    await user.click(screen.getByRole("button", { name: "Outside" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("synthesizes missing parent scopes and handles duplicate options", async () => {
    const user = userEvent.setup();
    render(<InsightScopePicker onChange={vi.fn()} scopes={[scopes[3], scopes[3]]} value={scopes[0].key} />);
    await user.click(screen.getByLabelText("Scope"));
    await user.type(screen.getByLabelText("Find scope"), "review");
    expect(screen.getAllByRole("button", { name: scopes[1].label })).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: scopes[2].label })).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: scopes[3].label })).toHaveLength(1);
  });
});
