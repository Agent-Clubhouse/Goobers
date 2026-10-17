import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { InteractivePage } from "./InteractivePage";
import { InteractiveError, writePending, type Command, type InteractiveClient, type InteractiveEvent, type RuntimeStatus, type Session, type StreamState, type Surface } from "../interactive/client";

const surfaces: Surface[] = [
  { id: "helper", name: "Gaggle helper", description: "Scope work using configured workflows", mode: "helper", enabled: true, maxChildren: 1 },
  { id: "workbench", name: "Agent workbench", description: "Investigate, edit and test", mode: "workbench", enabled: true, maxChildren: 2 },
];
const runtime: RuntimeStatus = { defaultMode: "codex-local", executionModes: [{id: "codex-local", label: "Agent · Codex", available: false, reason: "Host must opt in with --trusted-local-model-login", liveQualified: false}, {id: "local-tools", label: "Deterministic demo", available: true, liveQualified: false}], providerMode: "controlled-local", executionMode: "local-tools", identity: { name: "Mason", subject: "local:mason", issuer: "loopback-demo" }, connections: [{id: "ado", provider: "ado", label: "ADO demo", status: "active", resources: ["demo/project"]}], limits: { maxTurnSeconds: 120, maxChildren: 2 }, capabilities: { liveModel: false, liveDelegatedAuth: false } };
function makeSession(overrides: Partial<Session> = {}): Session { return { id: "s-1", owner: "local:mason", surfaceId: "workbench", mode: "workbench", executionMode: "local-tools", title: "Inspect application", status: "idle", sequence: 1, fence: 1, createdAt: "2026-10-17T09:00:00Z", updatedAt: "2026-10-17T09:00:00Z", turns: [], children: [], changes: [], ...overrides }; }
function fixture(initial = makeSession()) {
  let session = initial;
  let onEvent: (event: InteractiveEvent) => void = () => {};
  let onState: (state: StreamState) => void = () => {};
  const close = vi.fn();
  const client: InteractiveClient = {
    selectIdentity: vi.fn().mockResolvedValue({}), status: vi.fn().mockResolvedValue(runtime),
    connectionAction: vi.fn().mockResolvedValue({}), surfaces: vi.fn().mockResolvedValue(surfaces),
    sessions: vi.fn().mockImplementation(async () => [session]), create: vi.fn().mockResolvedValue(session),
    session: vi.fn().mockImplementation(async () => session), events: vi.fn().mockResolvedValue([]),
    changes: vi.fn().mockResolvedValue({changes: [{path: "app.js", status: "modified", additions: 2, deletions: 1}], diff: "-broken\n+fixed", workspacePath: "/isolated/session"}),
    command: vi.fn().mockImplementation(async (_id: string, input: Command) => ({commandId: input.commandId, sessionId: session.id, type: input.type, status: "accepted", sequence: ++session.sequence})),
    subscribe: vi.fn().mockImplementation((_id, _after, event, state) => { onEvent = event; onState = state; return {close}; }),
  };
  return {client, close, setSession: (next: Session) => {session = next;}, event: (event: InteractiveEvent) => onEvent(event), stream: (state: StreamState) => onState(state)};
}
afterEach(cleanup);

describe("interactive workspace", () => {
  it("offers configured optional surfaces and opens the selected experience", async () => {
    const {client} = fixture(); const navigate = vi.fn();
    render(<InteractivePage client={client} navigate={navigate} />);
    await screen.findByRole("radio", {name: /Agent workbench/});
    fireEvent.click(screen.getByRole("radio", {name: /Agent workbench/}));
    fireEvent.click(screen.getByRole("radio", {name: "Deterministic demo"}));
    fireEvent.change(screen.getByLabelText(/Session name/), {target: {value: "Fix task"}});
    fireEvent.click(screen.getByRole("button", {name: "Start session"}));
    await waitFor(() => expect(client.create).toHaveBeenCalledWith(expect.objectContaining({surfaceId: "workbench", executionMode: "local-tools", title: "Fix task"})));
    expect(navigate).toHaveBeenCalledWith({page: "interactive", sessionId: "s-1"});
    expect(screen.getByText(/Controlled local provider · no live Azure authentication/)).toBeInTheDocument();
  });
  it("requires an explicit demo choice when the default agent lacks host opt-in", async () => {
    const fx = fixture(); render(<InteractivePage client={fx.client} navigate={vi.fn()} />);
    const agent = await screen.findByRole("radio", {name: "Agent (Codex)"});
    expect(agent).toBeChecked(); expect(agent).toBeDisabled();
    expect(screen.getByRole("radio", {name: "Deterministic demo"})).not.toBeChecked();
    expect(screen.getByRole("button", {name: "Start session"})).toBeDisabled();
    expect(screen.getByText("Not live-qualified")).toBeInTheDocument();
    expect(fx.client.create).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("radio", {name: "Deterministic demo"}));
    fireEvent.click(screen.getByRole("button", {name: "Start session"}));
    await waitFor(() => expect(fx.client.create).toHaveBeenCalledWith(expect.objectContaining({executionMode: "local-tools"})));
  });
  it("supports an explicit agent choice for either surface without running a model in the UI", async () => {
    for (const [surfaceName, surfaceId] of [[/Gaggle helper/, "helper"], [/Agent workbench/, "workbench"]] as const) {
      const fx = fixture();
      vi.mocked(fx.client.status).mockResolvedValue({...runtime, executionModes: runtime.executionModes!.map(option => ({...option, available: true}))});
      const view = render(<InteractivePage client={fx.client} navigate={vi.fn()} />);
      fireEvent.click(await screen.findByRole("radio", {name: surfaceName}));
      await waitFor(() => expect(screen.getByRole("button", {name: "Start session"})).toBeEnabled());
      fireEvent.click(screen.getByRole("button", {name: "Start session"}));
      await waitFor(() => expect(fx.client.create).toHaveBeenCalledWith(expect.objectContaining({surfaceId, executionMode: "codex-local"})));
      expect(fx.client.command).not.toHaveBeenCalled();
      view.unmount();
    }
  });
  it("preserves an explicit agent choice after rejection without falling back to the demo", async () => {
    const fx = fixture();
    vi.mocked(fx.client.status).mockResolvedValue({...runtime, executionModes: runtime.executionModes!.map(option => ({...option, available: true}))});
    vi.mocked(fx.client.create).mockRejectedValue(new InteractiveError("Model unavailable", "unavailable", 503));
    render(<InteractivePage client={fx.client} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getByRole("button", {name: "Start session"})).toBeEnabled());
    fireEvent.click(screen.getByRole("button", {name: "Start session"}));
    expect(await screen.findByText("Model unavailable")).toBeInTheDocument();
    expect(screen.getByRole("radio", {name: "Agent (Codex)"})).toBeChecked();
    expect(fx.client.create).toHaveBeenCalledTimes(1);
    expect(fx.client.create).toHaveBeenCalledWith(expect.objectContaining({executionMode: "codex-local"}));
  });
  it("respects surface execution restrictions and displays its resolved DSL policy", async () => {
    const fx = fixture();
    vi.mocked(fx.client.status).mockResolvedValue({...runtime, executionModes: runtime.executionModes!.map(option => ({...option, available: true}))});
    vi.mocked(fx.client.surfaces).mockResolvedValue([{...surfaces[0], configRevision: "resolved-revision-one", gaggle: "finance", goober: "review-helper", model: "configured-model", executionModes: ["local-tools"], defaultExecutionMode: "local-tools", maxTurnSeconds: 90, maxActivePerOwner: 1}]);
    render(<InteractivePage client={fx.client} navigate={vi.fn()} />);
    expect(await screen.findByRole("radio", {name: "Agent (Codex)"})).toBeDisabled();
    expect(screen.getByRole("radio", {name: "Deterministic demo"})).toBeChecked();
    expect(screen.getByText("review-helper")).toBeInTheDocument();
    expect(screen.getByText(/Gaggle: finance · Model: configured-model/)).toBeInTheDocument();
    expect(screen.getByText(/90s per turn · 1 active per owner/)).toBeInTheDocument();
    expect(screen.getByText(/not allowed by the selected surface configuration/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", {name: "Start session"}));
    await waitFor(() => expect(fx.client.create).toHaveBeenCalledWith(expect.objectContaining({executionMode: "local-tools"})));
  });
  it("keeps an unavailable configured agent default instead of falling back", async () => {
    const fx = fixture();
    vi.mocked(fx.client.surfaces).mockResolvedValue([{...surfaces[0], configRevision: "revision-one", executionModes: ["codex-local"], defaultExecutionMode: "codex-local"}]);
    render(<InteractivePage client={fx.client} navigate={vi.fn()} />);
    expect(await screen.findByRole("radio", {name: "Agent (Codex)"})).toBeChecked();
    expect(screen.getByRole("radio", {name: "Deterministic demo"})).toBeDisabled();
    expect(screen.getByRole("button", {name: "Start session"})).toBeDisabled();
    expect(fx.client.create).not.toHaveBeenCalled();
  });
  it("refreshes configuration without rebinding an old session and preserves stop and review", async () => {
    const fx = fixture(makeSession({configRevision: "old", status: "recovery_required", children: [{id: "c-1", prompt: "Review", status: "proposed", requestedAt: "2026-10-17T09:00:00Z"}]}));
    vi.mocked(fx.client.surfaces).mockResolvedValue([{...surfaces[1], configRevision: "old", maxChildren: 1}]);
    render(<InteractivePage client={fx.client} navigate={vi.fn()} sessionId="s-1" />);
    await screen.findByRole("button", {name: "Stop session"});
    vi.mocked(fx.client.surfaces).mockResolvedValue([{...surfaces[1], configRevision: "new", maxChildren: 0}]);
    fireEvent.click(screen.getByRole("button", {name: "Refresh configuration"}));
    expect(await screen.findByText(/This session is bound to a retired configuration/)).toBeInTheDocument();
    expect(screen.getByRole("button", {name: "Resume session"})).toBeDisabled();
    expect(screen.getByRole("button", {name: "Stop session"})).toBeEnabled();
    fireEvent.click(screen.getByRole("tab", {name: /Children/}));
    expect(screen.getByRole("button", {name: "Propose child"})).toBeDisabled();
    expect(screen.getByRole("button", {name: "Run child"})).toBeDisabled();
    expect(screen.getByRole("button", {name: "Deny child"})).toBeEnabled();
  });
  it("reconciles an uncertain create with the same mode and ID after its surface is removed", async () => {
    const fx = fixture();
    const original = {commandId: "original-create", surfaceId: "removed-surface", executionMode: "codex-local" as const, title: "Saved request"};
    writePending("create.local:mason", original);
    vi.mocked(fx.client.surfaces).mockResolvedValue([]);
    render(<InteractivePage client={fx.client} navigate={vi.fn()} />);
    fireEvent.click(await screen.findByRole("button", {name: "Retry opening session"}));
    await waitFor(() => expect(fx.client.create).toHaveBeenCalledWith(original));
  });
  it("reports a failed configuration refresh and retains the last confirmed choices", async () => {
    const fx = fixture(); render(<InteractivePage client={fx.client} navigate={vi.fn()} />);
    await screen.findByRole("radio", {name: /Gaggle helper/});
    vi.mocked(fx.client.surfaces).mockRejectedValue(new Error("Configuration unavailable"));
    fireEvent.click(screen.getByRole("button", {name: "Refresh configuration"}));
    expect(await screen.findByText(/Configuration refresh failed: Configuration unavailable/)).toBeInTheDocument();
    expect(screen.getByRole("radio", {name: /Gaggle helper/})).toBeInTheDocument();
  });
  it("pins session copy to the saved execution mode instead of the server default", async () => {
    const fx = fixture(); render(<InteractivePage client={fx.client} navigate={vi.fn()} sessionId="s-1" />);
    expect(await screen.findByText("Try the sample investigation.")).toBeInTheDocument();
    expect(screen.getByText(/It does not reason about arbitrary requests/)).toBeInTheDocument();
    expect(screen.queryByRole("radio", {name: "Agent (Codex)"})).not.toBeInTheDocument();
    expect(screen.getByRole("tab", {name: "Actions"})).toBeInTheDocument();
  });
  it("uses free-text agent suggestions and distinguishes availability from qualification", async () => {
    const fx = fixture(makeSession({executionMode: "codex-local"}));
    vi.mocked(fx.client.status).mockResolvedValue({...runtime, executionModes: runtime.executionModes!.map(option => ({...option, available: true}))});
    render(<InteractivePage client={fx.client} navigate={vi.fn()} sessionId="s-1" />);
    expect(await screen.findByText(/Agent mode is not live-qualified/)).toBeInTheDocument();
    expect(screen.getByRole("button", {name: /Investigate this repository, choose a small improvement/})).toBeInTheDocument();
    expect(screen.queryByText("Try the sample investigation.")).not.toBeInTheDocument();
    expect(screen.getByLabelText("Message")).toHaveAttribute("placeholder", "Describe the work you want the agent to do…");
  });
  it("shows the exact immutable child definition and sends only a human review decision", async () => {
    const definition = "apiVersion: goobers/v1\nkind: Workflow\nmetadata:\n  name: model-review\n";
    const fx = fixture(makeSession({executionMode: "codex-local", children: [{id: "child-dynamic", prompt: "Review the implementation", workflowYaml: definition, status: "proposed", requestedAt: "2026-10-17T09:00:00Z"}]}));
    render(<InteractivePage client={fx.client} navigate={vi.fn()} sessionId="s-1" />);
    fireEvent.click(await screen.findByRole("tab", {name: /Children/}));
    expect(screen.getByLabelText("Workflow definition for child-dynamic").textContent).toBe(definition);
    expect(screen.queryByRole("textbox", {name: /Workflow definition/})).not.toBeInTheDocument();
    expect(fx.client.command).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", {name: "Run child"}));
    await waitFor(() => expect(fx.client.command).toHaveBeenCalledWith("s-1", expect.objectContaining({type: "child.review", childId: "child-dynamic", decision: "approve"})));
    expect(vi.mocked(fx.client.command).mock.calls[0][1]).not.toHaveProperty("workflowYaml");
  });
  it("shows an actionable unavailable state on an ordinary daemon without interactive routes", async () => {
    const {client} = fixture(); vi.mocked(client.surfaces).mockRejectedValue(new InteractiveError("missing", "unavailable", 404));
    render(<InteractivePage client={client} navigate={vi.fn()} />);
    expect(await screen.findByRole("heading", {name: "Interactive workspace unavailable"})).toBeInTheDocument();
    expect(screen.getByRole("link", {name: "View configured workflows"})).toHaveAttribute("href", "#/workflows");
  });
  it("targets active steering and does not claim delivery until its durable event arrives", async () => {
    const turn = { id: "t1", text: "Investigate", status: "running", attempt: 1, fence: 1, startedAt: "2026-10-17T09:00:00Z" };
    const fx = fixture(makeSession({status: "running", activeTurnId: "t1", turns: [turn]}));
    render(<InteractivePage client={fx.client} navigate={vi.fn()} sessionId="s-1" />);
    fireEvent.change(await screen.findByLabelText("Steer this turn"), {target: {value: "Focus on tests"}});
    fireEvent.click(screen.getByRole("button", {name: "Send steering"}));
    await waitFor(() => expect(fx.client.command).toHaveBeenCalledWith("s-1", expect.objectContaining({type: "steer", turnId: "t1", expectedFence: 1, text: "Focus on tests"})));
    expect(await screen.findByText("Steering accepted · awaiting delivery")).toBeInTheDocument();
    const command = vi.mocked(fx.client.command).mock.calls[0][1];
    act(() => fx.event({sequence: 3, sessionId: "s-1", turnId: "t1", commandId: command.commandId, type: "steering.delivered", time: turn.startedAt, data: {}}));
    expect(screen.getByText("Steering delivered")).toBeInTheDocument();
  });
  it("refreshes only a definitely rejected sequence while preserving the steering intent and fence", async () => {
    const turn = { id: "t1", text: "Wait", status: "running", attempt: 1, fence: 1, startedAt: "2026-10-17T09:00:00Z" };
    const fx = fixture(makeSession({status: "running", activeTurnId: "t1", turns: [turn]}));
    vi.mocked(fx.client.command).mockImplementationOnce(async () => {
      fx.setSession(makeSession({status: "running", activeTurnId: "t1", turns: [turn], sequence: 12}));
      throw new InteractiveError("refresh session", "stale_sequence", 409);
    });
    render(<InteractivePage client={fx.client} navigate={vi.fn()} sessionId="s-1" />);
    fireEvent.change(await screen.findByLabelText("Steer this turn"), {target: {value: "Focus on tests"}});
    fireEvent.click(screen.getByRole("button", {name: "Send steering"}));
    await waitFor(() => expect(fx.client.command).toHaveBeenCalledTimes(2));
    const [first, second] = vi.mocked(fx.client.command).mock.calls.map(call => call[1]);
    expect(second).toEqual({...first, expectedSequence: 12});
    expect(second).toMatchObject({turnId: "t1", expectedFence: 1, text: "Focus on tests"});
  });
  it("does not redirect rejected steering when the active turn changes", async () => {
    const turn = { id: "t1", text: "Wait", status: "running", attempt: 1, fence: 1, startedAt: "2026-10-17T09:00:00Z" };
    const fx = fixture(makeSession({status: "running", activeTurnId: "t1", turns: [turn]}));
    vi.mocked(fx.client.command).mockImplementationOnce(async () => {
      fx.setSession(makeSession({status: "running", activeTurnId: "t2", turns: [turn], sequence: 12}));
      throw new InteractiveError("refresh session", "stale_sequence", 409);
    });
    render(<InteractivePage client={fx.client} navigate={vi.fn()} sessionId="s-1" />);
    fireEvent.change(await screen.findByLabelText("Steer this turn"), {target: {value: "Focus on tests"}});
    fireEvent.click(screen.getByRole("button", {name: "Send steering"}));
    expect(await screen.findByText(/Your steering was not sent to another turn/)).toBeInTheDocument();
    expect(fx.client.command).toHaveBeenCalledTimes(1);
  });
  it("does not automatically retry an uncertain command response", async () => {
    const fx = fixture(); vi.mocked(fx.client.command).mockRejectedValue(new InteractiveError("Lost response", "network", 0));
    render(<InteractivePage client={fx.client} navigate={vi.fn()} sessionId="s-1" />);
    fireEvent.change(await screen.findByLabelText("Message"), {target: {value: "Investigate"}});
    fireEvent.click(screen.getByRole("button", {name: "Send message"}));
    expect(await screen.findByRole("button", {name: "Retry same command"})).toBeInTheDocument();
    expect(fx.client.command).toHaveBeenCalledTimes(1);
  });
  it("disconnects and replays without dispatching new work", async () => {
    const fx = fixture(); render(<InteractivePage client={fx.client} navigate={vi.fn()} sessionId="s-1" />);
    fireEvent.click(await screen.findByRole("button", {name: "Disconnect stream"}));
    expect(fx.close).toHaveBeenCalled();
    expect(screen.getByLabelText("Message")).toBeDisabled();
    fireEvent.click(screen.getByRole("button", {name: "Reconnect"}));
    await waitFor(() => expect(fx.client.subscribe).toHaveBeenCalledTimes(2));
    expect(fx.client.command).not.toHaveBeenCalled();
  });
  it("restores a pending command after reload and retries its exact identity and payload", async () => {
    const command: Command = {commandId: "lost-command", type: "message", text: "Fix application", expectedSequence: 1, expectedFence: 1};
    writePending("pending.s-1", {sessionId: "s-1", command});
    const fx = fixture(); render(<InteractivePage client={fx.client} navigate={vi.fn()} sessionId="s-1" />);
    fireEvent.click(await screen.findByRole("button", {name: "Retry same command"}));
    await waitFor(() => expect(fx.client.command).toHaveBeenCalledWith("s-1", command));
  });
  it("reviews actual cumulative changes and requires a child decision", async () => {
    const fx = fixture(makeSession({children: [{id: "child-1", prompt: "Review tests", status: "proposed", requestedAt: "2026-10-17T09:00:00Z"}]}));
    render(<InteractivePage client={fx.client} navigate={vi.fn()} sessionId="s-1" />);
    fireEvent.click(await screen.findByRole("button", {name: "Review changes"}));
    expect(await screen.findByLabelText("Cumulative diff")).toHaveTextContent("+fixed");
    fireEvent.click(screen.getByRole("tab", {name: /Children/}));
    fireEvent.click(screen.getByRole("button", {name: "Run child"}));
    await waitFor(() => expect(fx.client.command).toHaveBeenCalledWith("s-1", expect.objectContaining({type: "child.review", childId: "child-1", decision: "approve"})));
  });
  it("keeps stop pending until the server confirms custody release", async () => {
    const fx = fixture(); render(<InteractivePage client={fx.client} navigate={vi.fn()} sessionId="s-1" />);
    fireEvent.click(await screen.findByRole("button", {name: "Stop session"}));
    expect(await screen.findByText(/Stop request accepted/)).toBeInTheDocument();
    expect(screen.queryByText(/Session stopped. History/)).not.toBeInTheDocument();
    fx.setSession(makeSession({status: "stopping"}));
    act(() => fx.event({sequence: 4, sessionId: "s-1", type: "session.stopping", time: "2026-10-17T09:00:00Z", data: {}}));
    expect(await screen.findByText(/Waiting for the executor to confirm/)).toBeInTheDocument();
  });
  it("labels controlled auth and waits for the server before confirming a revoke", async () => {
    const fx = fixture(); let resolve!: (value: unknown) => void;
    vi.mocked(fx.client.connectionAction).mockReturnValue(new Promise(done => {resolve = done;}));
    render(<InteractivePage client={fx.client} navigate={vi.fn()} sessionId="s-1" />);
    fireEvent.click(await screen.findByRole("tab", {name: "Access"}));
    fireEvent.click(screen.getByRole("button", {name: "Revoke demo connection"}));
    expect(screen.getByText(/Connection change pending server confirmation/)).toBeInTheDocument();
    expect(screen.queryByText("Revoke confirmed by the server.")).not.toBeInTheDocument();
    await act(async () => resolve({}));
    expect(await screen.findByText("Revoke confirmed by the server.")).toBeInTheDocument();
    expect(fx.client.connectionAction).toHaveBeenCalledWith("ado", "revoke");
  });
});
