import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { PermissionCard, QuestionCard, type PermissionRequest, type QuestionRequest } from "./ApprovalDock";

const permission: PermissionRequest = {kind: "permission", requestId: "p1", turnId: "t1", sequence: 1, title: "Run tests", preview: ["npm test"], options: [{optionId: "allow_once", label: "Allow once"}, {optionId: "reject_once", label: "Reject"}]};
const question: QuestionRequest = {kind: "question", requestId: "q1", turnId: "t1", sequence: 2, questions: [{id: "choice", prompt: "Which checks?", multiSelect: true, allowFreeText: true, options: [{id: "unit", label: "Unit"}, {id: "browser", label: "Browser"}]}]};

describe("approval dock", () => {
  it("requires a concrete preview and an explicit choice before approving", async () => {
    const onAnswer = vi.fn().mockResolvedValue(true);
    const view = render(<PermissionCard request={{...permission, preview: []}} disabled={false} onAnswer={onAnswer} />);
    expect(screen.getByRole("button", {name: "Allow once"})).toBeDisabled();
    fireEvent.keyDown(screen.getByRole("region", {name: "Permission request"}), {key: "1"});
    expect(onAnswer).not.toHaveBeenCalled();
    view.rerender(<PermissionCard request={permission} disabled={false} onAnswer={onAnswer} />);
    fireEvent.keyDown(screen.getByRole("region", {name: "Permission request"}), {key: "Enter"});
    expect(onAnswer).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", {name: "Allow once"}));
    await waitFor(() => expect(onAnswer).toHaveBeenCalledWith({type: "permission.respond", requestId: "p1", optionId: "allow_once"}));
    expect(await screen.findByText(/Response accepted/)).toBeVisible();
    expect(screen.getByRole("button", {name: "Reject"})).toBeDisabled();
  });
  it("distinguishes sending from accepted and permits retry after failure", async () => {
    let finish!: (accepted: boolean) => void;
    const onAnswer = vi.fn(() => new Promise<boolean>(resolve => { finish = resolve; }));
    render(<PermissionCard request={permission} disabled={false} onAnswer={onAnswer} />);
    fireEvent.click(screen.getByRole("button", {name: "Reject"}));
    expect(screen.getByText("Sending response…")).toBeVisible();
    expect(screen.queryByText(/Response accepted/)).not.toBeInTheDocument();
    finish(false);
    await waitFor(() => expect(screen.getByRole("button", {name: "Reject"})).toBeEnabled());
  });
  it("unlocks repeated rejections even when replay batches identical failure messages", async () => {
    const onAnswer = vi.fn().mockResolvedValue(true);
    const view = render(<PermissionCard request={permission} disabled={false} onAnswer={onAnswer} />);
    for (const responseSequence of [2, 4]) {
      fireEvent.click(screen.getByRole("button", {name: "Reject"}));
      await screen.findByText(/Response accepted/);
      view.rerender(<PermissionCard request={{...permission, failure: "Driver unavailable", responseSequence}} disabled={false} onAnswer={onAnswer} />);
      await waitFor(() => expect(screen.getByRole("button", {name: "Reject"})).toBeEnabled());
    }
    expect(onAnswer).toHaveBeenCalledTimes(2);
  });
  it("sends all selected answers, with explicit written answers taking precedence", async () => {
    const onAnswer = vi.fn().mockResolvedValue(true);
    render(<QuestionCard request={question} disabled={false} onAnswer={onAnswer} />);
    expect(screen.getByRole("button", {name: "Send answer"})).toBeDisabled();
    fireEvent.click(screen.getByRole("button", {name: "Unit"}));
    fireEvent.click(screen.getByRole("button", {name: "Browser"}));
    fireEvent.change(screen.getByRole("textbox", {name: "Answer: Which checks?"}), {target: {value: "Only integration"}});
    fireEvent.keyDown(screen.getByRole("textbox", {name: "Answer: Which checks?"}), {key: "1"});
    expect(onAnswer).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", {name: "Send answer"}));
    await waitFor(() => expect(onAnswer).toHaveBeenCalledWith({type: "question.answer", requestId: "q1", answers: {choice: ["Only integration"]}}));
  });
});
