import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { Goober, InteractiveCapabilities, InteractiveSession, SessionMessage } from "../api/types";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { SharedSessionDetail } from "./SharedSessionDetail";
import { SharedSessionsPanel } from "./SharedSessionsPanel";

const at = "2026-10-04T12:00:00Z";
const session: InteractiveSession = { id: "session-one", gaggle: "team", title: "Scope feature", goober: "planner", configGeneration: "pinned", gooberDigest: "digest", state: "running", createdBy: { issuer: "https://identity.example", subject: "alice" }, createdAt: at, updatedAt: at, nextSequence: 3 };
const access: InteractiveCapabilities = { gaggle: "team", policyConfigured: true, viewer: true, operator: true, sourceWriteMode: "pull-request", actions: ["session.create", "session.message"].map((action) => ({ action: action as "session.create" | "session.message", authorized: true, credentialConfigured: true, available: true, reasonCode: "" })) };
const human: SessionMessage = { id: "message-one", sessionId: session.id, sequence: 1, actorKind: "human", actor: session.createdBy, text: "Outline the feature", createdAt: at, turnId: "queued-turn" };
const agent: SessionMessage = { id: "message-two", sessionId: session.id, sequence: 2, actorKind: "agent", text: "Start with child workflows.", createdAt: at, runId: "verified-run" };
const goobers = [{ name: "planner", displayName: "Planner" } as Goober];
function setup() {
  const client = new FixtureDaemonClient(populatedDaemonFixtures());
  vi.spyOn(client, "getInteractiveCapabilities").mockResolvedValue(access);
  vi.spyOn(client, "listSessions").mockResolvedValue({ items: [session] });
  vi.spyOn(client, "getSession").mockResolvedValue(session);
  vi.spyOn(client, "getSessionMessages").mockResolvedValue({ items: [human, agent] });
  render(<SharedSessionsPanel client={client} gaggle="team" goobers={goobers} />);
  return client;
}
async function select() { fireEvent.click(await screen.findByRole("button", { name: /Open session: Scope feature/ })); await screen.findByText(human.text); }

describe("SharedSessionsPanel", () => {
  it("keeps queued messages distinct from verified executions and shows each author's identity", async () => {
    setup(); await select();
    expect(screen.getByText("alice")).toBeInTheDocument();
    expect(screen.getByText("https://identity.example")).toBeInTheDocument();
    expect(screen.getByText("Agent", { selector: "strong" })).toBeInTheDocument();
    expect(screen.getAllByRole("link", { name: "Open turn run" })).toHaveLength(1);
    expect(screen.getByRole("link", { name: "Open turn run" })).toHaveAttribute("href", "#/run/verified-run");
  });
  it("retries uncertain messages unchanged across refresh and prevents switching away", async () => {
    const client = setup();
    const send = vi.spyOn(client, "sendSessionMessage").mockRejectedValueOnce(new Error("lost response")).mockResolvedValueOnce({ session: { ...session, state: "queued" }, duplicate: true });
    await select();
    fireEvent.change(screen.getByLabelText("Message to agent"), { target: { value: "Also include permissions." } });
    fireEvent.click(screen.getByRole("button", { name: "Send message" }));
    await screen.findByRole("button", { name: "Retry same message" });
    expect(screen.getByLabelText("Message to agent")).toBeDisabled();
    expect(screen.getByRole("button", { name: /Open session: Scope feature/ })).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "Refresh sessions" }));
    fireEvent.click(await screen.findByRole("button", { name: "Retry same message" }));
    expect(await screen.findByText("Message accepted. The agent will process it in order.")).toBeInTheDocument();
    expect(send.mock.calls[1]).toEqual(send.mock.calls[0]);
    expect(send.mock.calls[0].slice(0, 2)).toEqual(["team", session.id]);
    expect(send.mock.calls[0][3]).toEqual({ text: "Also include permissions." });
  });
  it("inspects a human-selected PR and retains that exact target when a message reply is lost", async () => {
    const client = setup();
    vi.mocked(client.getInteractiveCapabilities).mockResolvedValue({ ...access, actions: [...access.actions, ...(["pr.repair", "repository.read"] as const).map((action) => ({ action, authorized: true, credentialConfigured: true, available: true, reasonCode: "" as const }))] });
    vi.spyOn(client, "listWorkbenchSources").mockResolvedValue({ generation: "current", items: [{ bindingId: "code", kind: "documents", provider: "github", owner: "org", repository: "repo" }] });
    const target = { sourceBindingId: "code", repository: { provider: "github" as const, owner: "org", name: "repo" }, repositorySourceId: "100", id: "12", sourceId: "900", expectedHeadSha: "a".repeat(40) };
    const inspect = vi.spyOn(client, "inspectPullRequest").mockResolvedValue({ target, headSha: target.expectedHeadSha, baseSha: "b".repeat(40), head: "fix", base: "main", title: "Fix a problem", description: "", url: "https://github.com/org/repo/pull/12", open: true, draft: false });
    const send = vi.spyOn(client, "sendSessionMessage").mockRejectedValueOnce(new Error("lost reply")).mockResolvedValueOnce({ session, duplicate: true });
    await select();
    await screen.findByRole("option", { name: "org/repo · code" });
    fireEvent.change(screen.getByLabelText("Repository source"), { target: { value: "code" } });
    fireEvent.change(screen.getByLabelText("PR number"), { target: { value: "12" } });
    fireEvent.click(screen.getByRole("button", { name: "Inspect PR" }));
    fireEvent.click(await screen.findByRole("button", { name: "Use this PR for the message" }));
    fireEvent.change(screen.getByLabelText("Message to agent"), { target: { value: "Fix the failing case" } });
    fireEvent.click(screen.getByRole("button", { name: "Send message with PR" }));
    await screen.findByRole("button", { name: "Retry same message" });
    expect(screen.getByLabelText("Repository source")).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "Refresh conversation" }));
    fireEvent.click(await screen.findByRole("button", { name: "Retry same message" }));
    await screen.findByText("Message accepted. The agent will process it in order.");
    expect(inspect).toHaveBeenCalledTimes(1);
    expect(send.mock.calls[0][3]).toEqual({ text: "Fix the failing case", repairTarget: target });
    expect(send.mock.calls[1]).toEqual(send.mock.calls[0]);
  });
  it("clears an unsubmitted PR selection when the conversation is refreshed", async () => {
    const client = setup();
    vi.mocked(client.getInteractiveCapabilities).mockResolvedValue({ ...access, actions: [...access.actions, ...(["pr.repair", "repository.read"] as const).map((action) => ({ action, authorized: true, credentialConfigured: true, available: true, reasonCode: "" as const }))] });
    const sources = vi.spyOn(client, "listWorkbenchSources").mockResolvedValue({ generation: "before", items: [{ bindingId: "code", kind: "documents", provider: "github", owner: "org", repository: "repo" }] });
    const target = { sourceBindingId: "code", repository: { provider: "github" as const, owner: "org", name: "repo" }, repositorySourceId: "100", id: "12", sourceId: "900", expectedHeadSha: "a".repeat(40) };
    vi.spyOn(client, "inspectPullRequest").mockResolvedValue({ target, headSha: target.expectedHeadSha, baseSha: "b".repeat(40), head: "fix", base: "main", title: "Fix a problem", description: "", url: "https://github.com/org/repo/pull/12", open: true, draft: false });
    await select();
    await screen.findByRole("option", { name: "org/repo · code" });
    fireEvent.change(screen.getByLabelText("Repository source"), { target: { value: "code" } });
    fireEvent.change(screen.getByLabelText("PR number"), { target: { value: "12" } });
    fireEvent.click(screen.getByRole("button", { name: "Inspect PR" }));
    fireEvent.click(await screen.findByRole("button", { name: "Use this PR for the message" }));
    expect(screen.getByRole("button", { name: "Send message with PR" })).toBeInTheDocument();
    sources.mockResolvedValue({ generation: "after", items: [] });
    fireEvent.click(screen.getByRole("button", { name: "Refresh conversation" }));
    await screen.findByRole("button", { name: "Send message" });
    expect(screen.queryByText(/Selected PR #12 at/)).not.toBeInTheDocument();
  });
  it("never retries an uncertain message through a replacement client", async () => {
    function connected() {
      const client = new FixtureDaemonClient(populatedDaemonFixtures());
      vi.spyOn(client, "getInteractiveCapabilities").mockResolvedValue(access);
      vi.spyOn(client, "getSession").mockResolvedValue(session);
      vi.spyOn(client, "getSessionMessages").mockResolvedValue({ items: [human] });
      return client;
    }
    const first = connected(), replacement = connected();
    const originalSend = vi.spyOn(first, "sendSessionMessage").mockRejectedValueOnce(new Error("lost response")).mockResolvedValueOnce({ session, duplicate: true });
    const replacementSend = vi.spyOn(replacement, "sendSessionMessage");
    const pendingChanged = vi.fn();
    const view = render(<SharedSessionDetail client={first} gaggle="team" id={session.id} parentRevision={0} pendingChanged={pendingChanged} />);
    fireEvent.change(await screen.findByLabelText("Message to agent"), { target: { value: "Original connection" } });
    fireEvent.click(screen.getByRole("button", { name: "Send message" }));
    await screen.findByRole("button", { name: "Retry same message" });
    view.rerender(<SharedSessionDetail client={replacement} gaggle="team" id={session.id} parentRevision={0} pendingChanged={pendingChanged} />);
    expect(await screen.findByRole("button", { name: "Retry same message" })).toBeDisabled();
    expect(replacementSend).not.toHaveBeenCalled();
    view.rerender(<SharedSessionDetail client={first} gaggle="team" id={session.id} parentRevision={0} pendingChanged={pendingChanged} />);
    fireEvent.click(await screen.findByRole("button", { name: "Retry same message" }));
    await screen.findByText("Message accepted. The agent will process it in order.");
    expect(originalSend.mock.calls[1]).toEqual(originalSend.mock.calls[0]);
    expect(replacementSend).not.toHaveBeenCalled();
  });
  it("creates from an existing configured agent without sending execution pins", async () => {
    const client = setup();
    const create = vi.spyOn(client, "createSession").mockResolvedValue({ session, duplicate: false });
    fireEvent.change(await screen.findByLabelText("Session title"), { target: { value: "Scope feature" } });
    fireEvent.change(screen.getByLabelText("Agent"), { target: { value: "planner" } });
    fireEvent.click(screen.getByRole("button", { name: "Create session" }));
    await waitFor(() => expect(create).toHaveBeenCalledWith("team", expect.any(String), { title: "Scope feature", goober: "planner" }));
    expect(await screen.findByText(human.text)).toBeInTheDocument();
  });
  it("reports a close request separately from a confirmed stop", async () => {
    const client = setup();
    const close = vi.spyOn(client, "closeSession").mockResolvedValue({ session: { ...session, state: "cancel-requested" }, duplicate: false });
    await select();
    vi.mocked(client.getSession).mockResolvedValue({ ...session, state: "cancel-requested" });
    fireEvent.click(screen.getByText("Close session", { selector: "summary" }));
    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "Scope complete" } });
    fireEvent.click(screen.getByRole("button", { name: "Close session" }));
    expect(await screen.findByText("Close accepted. Any active work still needs to stop.")).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByLabelText("Message to agent")).not.toBeInTheDocument());
    expect(close).toHaveBeenCalledWith("team", session.id, expect.any(String), { reason: "Scope complete" });
  });
  it("clears a conversation after access revocation and keeps configured read-only sessions visible", async () => {
    const client = setup(); await select();
    vi.mocked(client.getInteractiveCapabilities).mockResolvedValue({ ...access, policyConfigured: false, actions: [] });
    fireEvent.click(screen.getByRole("button", { name: "Refresh conversation" }));
    expect(await screen.findByText("Messages are read-only with your current permissions and configuration.")).toBeInTheDocument();
    expect(screen.getByText(human.text)).toBeInTheDocument();
    expect(screen.queryByLabelText("Message to agent")).not.toBeInTheDocument();
    vi.mocked(client.getSessionMessages).mockRejectedValue(new Error("revoked"));
    fireEvent.click(screen.getByRole("button", { name: "Refresh conversation" }));
    expect(await screen.findByText("This session is unavailable. Check your access or refresh.")).toBeInTheDocument();
    expect(within(screen.getByRole("region", { name: "Session conversation" })).queryByText(human.text)).not.toBeInTheDocument();
  });
});
