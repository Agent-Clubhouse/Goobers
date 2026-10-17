import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { OperationsPanel } from "./OperationsPanel";

const state = { mode: "local", actorId: "mason", sessionId: "s-1", repository: "/fixture/repo", configDigest: "revision-1", instanceRoot: "/fixture", gaggles: [{metadata: {name: "interactive"}, spec: {displayName: "Prototype gaggle"}}], workflows: [{metadata: {name: "inspect"}, spec: {enabled: true}}], runs: [], actions: [], backlog: [], workflowDraft: "apiVersion: goobers/v1\nkind: Workflow\n", capabilities: [], limitations: ["Local native runner"] };
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
describe("helper action controls", () => {
  it("separates a scoped proposal from exact apply and displays the returned preview", async () => {
    const action = {id: "action-1", operationId: "operation-1", kind: "submit_run", status: "proposed", operationStatus: "prepared", summary: "Run inspect", preview: "Admit workflow inspect using the current configuration", updatedAt: "2026-10-17T00:00:00Z", request: {kind: "submit_run"}};
    let proposed = false;
    const fetcher = vi.fn().mockImplementation(async (url: string, init?: RequestInit) => {
      if (url.includes("/proposals")) { proposed = true; return new Response(JSON.stringify(action)); }
      if (url.includes("/apply")) return new Response(JSON.stringify({...action, status: "admitted"}));
      return new Response(JSON.stringify({...state, actions: proposed ? [action] : []}));
    });
    vi.stubGlobal("fetch", fetcher);
    render(<OperationsPanel sessionId="s-1" disabled={false} />);
    fireEvent.click(await screen.findByRole("button", {name: "Preview action"}));
    expect(await screen.findByText(action.preview)).toBeInTheDocument();
    expect(fetcher.mock.calls.filter(([url]) => String(url).includes("/apply"))).toHaveLength(0);
    const proposal = fetcher.mock.calls.find(([url]) => String(url).includes("/proposals"))!;
    expect(proposal[0]).toContain("?sessionId=s-1");
    expect(JSON.parse(proposal[1].body)).toMatchObject({kind: "submit_run", workflow: "inspect", gaggle: "interactive"});
    fireEvent.click(screen.getByRole("button", {name: "Start run"}));
    await waitFor(() => expect(fetcher).toHaveBeenCalledWith("/api/v1/interactive/operations/actions/action-1/apply?sessionId=s-1", expect.objectContaining({method: "POST"})));
  });
  it("edits the server-provided workflow draft before asking for a preview", async () => {
    const fetcher = vi.fn().mockImplementation(async (url: string) => new Response(JSON.stringify(url.includes("/proposals") ? {summary: "Add workflow", status: "proposed"} : state)));
    vi.stubGlobal("fetch", fetcher);
    render(<OperationsPanel sessionId="s-1" disabled={false} />);
    fireEvent.change(await screen.findByLabelText("Action"), {target: {value: "add_workflow"}});
    expect(screen.getByLabelText("Workflow YAML")).toHaveValue(state.workflowDraft);
    fireEvent.change(screen.getByLabelText("Workflow YAML"), {target: {value: "kind: Workflow\nmetadata:\n  name: custom"}});
    fireEvent.click(screen.getByRole("button", {name: "Preview action"}));
    await waitFor(() => expect(fetcher.mock.calls.some(([url, init]) => String(url).includes("/proposals") && JSON.parse(init.body).workflowYaml.includes("custom"))).toBe(true));
  });
});
