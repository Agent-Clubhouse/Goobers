import {act, fireEvent, render, screen, waitFor, cleanup} from "@testing-library/react";
import {afterEach, expect, it, vi} from "vitest";
import {PermissionCard, QuestionCard} from "./ApprovalDock";
afterEach(cleanup);
it("late HTTP acceptance cannot override an already observed permission rejection", async () => {
  let finish!: (ok: boolean) => void;
  const onAnswer = vi.fn(() => new Promise<boolean>(resolve => { finish = resolve; }));
  const request = {kind: "permission" as const, requestId:"p",turnId:"t",sequence:1,title:"Run",preview:["npm test"], options:[{optionId:"allow_once" as const,label:"Allow once"},{optionId:"reject_once" as const,label:"Deny"}]};
  const view = render(<PermissionCard request={request} disabled={false} onAnswer={onAnswer}/>);
  fireEvent.click(screen.getByRole("button", {name:"Allow once"}));
  expect(screen.getByText("Sending response…")).toBeVisible();
  view.rerender(<PermissionCard request={{...request, pending:false, responseSequence:2, failure:"Driver rejected this response"}} disabled={false} onAnswer={onAnswer}/>);
  await waitFor(() => expect(screen.getByRole("button",{name:"Allow once"})).toBeEnabled());
  await act(async () => { finish(true); });
  expect(screen.getByRole("button",{name:"Allow once"})).toBeEnabled();
  expect(screen.queryByText(/Response accepted/)).not.toBeInTheDocument();
});
it("late HTTP acceptance cannot override an already observed question rejection", async () => {
  let finish!: (ok: boolean) => void;
  const onAnswer = vi.fn(() => new Promise<boolean>(resolve => { finish = resolve; }));
  const request = {kind:"question" as const, requestId:"q",turnId:"t",sequence:1,questions:[{id:"one",prompt:"Which?",options:[{id:"a",label:"A"}]}]};
  const view=render(<QuestionCard request={request} disabled={false} onAnswer={onAnswer}/>);
  fireEvent.click(screen.getByRole("button",{name:"A"}));
  fireEvent.click(screen.getByRole("button",{name:"Send answer"}));
  view.rerender(<QuestionCard request={{...request,pending:false,responseSequence:2,failure:"Driver rejected this response"}} disabled={false} onAnswer={onAnswer}/>);
  await waitFor(() => expect(screen.getByRole("button",{name:"Send answer"})).toBeEnabled());
  await act(async () => { finish(true); });
  expect(screen.getByRole("button",{name:"Send answer"})).toBeEnabled();
});
