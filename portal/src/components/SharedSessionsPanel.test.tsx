import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { Goober, InteractiveCapabilities, InteractiveSession, SessionMessage } from "../api/types";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
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
