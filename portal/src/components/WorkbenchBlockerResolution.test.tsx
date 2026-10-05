import { useLayoutEffect } from "react";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { BacklogItem, Goober, InteractiveCapabilities, InteractiveSession, SourceView } from "../api/types";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { WorkbenchBlockerResolution } from "./WorkbenchBlockerResolution";

const at = "2026-10-04T12:00:00Z";
const source: SourceView = { bindingId: "items", kind: "backlog", provider: "github", owner: "acme", repository: "issues", writeFields: ["labels"] };
const item: BacklogItem = { ref: { gaggleId: "team", sourceBindingId: "items", kind: "work-item", sourceId: "987654" }, locator: { id: "42" }, revision: "current", revisionSemantics: "timestamp-preflight", type: "issue", title: "Blocked work", state: "open", labels: ["goobers:needs-human"], assignees: [], relationships: [], objective: false, relationshipCoverage: { parents: "not-loaded", blockers: "not-loaded", milestones: "not-loaded" } };
const goobers = [{ name: "planner", displayName: "Planner" } as Goober];
const session: InteractiveSession = { id: "session-one", gaggle: "team", title: "Resolve blocked work", goober: "planner", configGeneration: "pinned", gooberDigest: "digest", state: "running", createdBy: { issuer: "https://identity.example", subject: "alice" }, createdAt: at, updatedAt: at, nextSequence: 3 };
const access: InteractiveCapabilities = { gaggle: "team", policyConfigured: true, viewer: true, operator: true, sourceWriteMode: "pull-request", actions: ["session.create", "session.message", "backlog.resolve"].map((action) => ({ action: action as "session.create" | "session.message" | "backlog.resolve", authorized: true, credentialConfigured: true, available: true, reasonCode: "" })) };
function clientFixture() {
  const client = new FixtureDaemonClient(populatedDaemonFixtures());
  vi.spyOn(client, "getInteractiveCapabilities").mockResolvedValue(access);
  vi.spyOn(client, "listSessions").mockResolvedValue({ items: [session] });
  vi.spyOn(client, "getSession").mockResolvedValue(session);
  vi.spyOn(client, "getSessionMessages").mockResolvedValue({ items: [{ id: "response", sessionId: session.id, sequence: 1, actorKind: "agent", text: "Still waiting: dependency #43 is open.", createdAt: at }] });
  vi.spyOn(client, "createSession").mockResolvedValue({ session, duplicate: false });
  vi.spyOn(client, "sendSessionMessage").mockResolvedValue({ session, duplicate: false });
  vi.spyOn(client, "patchWorkbenchItem");
  return client;
}
async function open() { await waitFor(() => expect(screen.getByRole("button", { name: "Resolve blocker" })).toBeEnabled()); fireEvent.click(screen.getByRole("button", { name: "Resolve blocker" })); await screen.findByLabelText("Your answer or guidance"); }
function fill() { fireEvent.change(screen.getByLabelText("Resolution agent"), { target: { value: "planner" } }); fireEvent.change(screen.getByLabelText("Your answer or guidance"), { target: { value: "The requested design review is complete." } }); }
function start() { fireEvent.click(screen.getByRole("button", { name: "Ask agent to resolve blocker" })); }

describe("WorkbenchBlockerResolution", () => {
  it("starts an attributed shared session with the exact source and shows remaining blockers", async () => {
    const client = clientFixture(); render(<WorkbenchBlockerResolution client={client} item={item} source={source} goobers={goobers} />); await open(); fill(); start();
    expect(await screen.findByText("Still waiting: dependency #43 is open.")).toBeInTheDocument();
    expect(client.createSession).toHaveBeenCalledWith("team", expect.stringMatching(/^blocker:create:/), { title: "Resolve blocker #42", goober: "planner" });
    const call = vi.mocked(client.sendSessionMessage).mock.calls[0];
    expect(call.slice(0, 2)).toEqual(["team", session.id]);
    expect(call[2]).toMatch(/^blocker:message:/);
    expect(call[3].text).toContain('"sourceId":"987654"');
    expect(call[3].text).toContain('"repository":"issues"');
    expect(call[3].text).toContain("The requested design review is complete.");
    expect(call[3].text).toContain("this message is my explicit instruction");
    expect(call[3].text).toContain("dependencies remain open");
    expect(client.patchWorkbenchItem).not.toHaveBeenCalled();
    expect(screen.queryByRole("link", { name: "Open turn run" })).not.toBeInTheDocument();
  });
  it("uses an existing session and retries a lost message response with the identical command", async () => {
    const client = clientFixture(); vi.mocked(client.sendSessionMessage).mockRejectedValueOnce(new Error("lost response"));
    render(<WorkbenchBlockerResolution client={client} item={item} source={source} goobers={goobers} />); await open();
    fireEvent.change(screen.getByLabelText("Resolution session"), { target: { value: session.id } });
    fireEvent.change(screen.getByLabelText("Your answer or guidance"), { target: { value: "Recheck the completed dependency." } }); start();
    fireEvent.click(await screen.findByRole("button", { name: "Retry same resolution request" }));
    await screen.findByText("Still waiting: dependency #43 is open.");
    expect(client.createSession).not.toHaveBeenCalled();
    expect(vi.mocked(client.sendSessionMessage).mock.calls[1]).toEqual(vi.mocked(client.sendSessionMessage).mock.calls[0]);
  });
  it("keeps an acknowledged new session when only its message response was lost", async () => {
    const client = clientFixture(); vi.mocked(client.sendSessionMessage).mockRejectedValueOnce(new Error("lost response"));
    render(<WorkbenchBlockerResolution client={client} item={item} source={source} goobers={goobers} />); await open(); fill(); start();
    const retry = await screen.findByRole("button", { name: "Retry same resolution request" });
    expect(screen.getByLabelText("Your answer or guidance")).toBeDisabled();
    expect(screen.getByLabelText("Resolution session")).toBeDisabled();
    expect(screen.getByRole("button", { name: "Refresh resolution access" })).toBeDisabled();
    fireEvent.click(retry); await screen.findByText("Still waiting: dependency #43 is open.");
    expect(client.createSession).toHaveBeenCalledTimes(1);
    expect(vi.mocked(client.sendSessionMessage).mock.calls[1]).toEqual(vi.mocked(client.sendSessionMessage).mock.calls[0]);
  });
  it("requires dedicated resolution and source labels access, recognizing native ADO tag casing", async () => {
    const client = clientFixture(); vi.mocked(client.getInteractiveCapabilities).mockResolvedValue({ ...access, actions: access.actions.filter((entry) => entry.action !== "backlog.resolve") });
    const { rerender } = render(<WorkbenchBlockerResolution client={client} item={{ ...item, labels: ["GOOBERS:NEEDS-HUMAN"] }} source={{ ...source, provider: "ado" }} goobers={goobers} />);
    await screen.findByText(/Resolution requires an enabled shared session/); expect(screen.getByRole("button", { name: "Resolve blocker" })).toBeDisabled();
    vi.mocked(client.getInteractiveCapabilities).mockResolvedValue(access);
    rerender(<WorkbenchBlockerResolution client={client} item={item} source={{ ...source, writeFields: ["title"] }} goobers={goobers} />);
    await screen.findByText(/Resolution requires an enabled shared session/); expect(screen.getByRole("button", { name: "Resolve blocker" })).toBeDisabled();
  });
  it("removes an uncertain old command before a replacement client commits its layout effects", async () => {
    const oldClient = clientFixture(), nextClient = clientFixture(); vi.mocked(oldClient.sendSessionMessage).mockRejectedValue(new Error("lost response"));
    function Host({ changed }: { changed: boolean }) {
      useLayoutEffect(() => { if (changed) screen.queryByRole("button", { name: "Retry same resolution request" })?.click(); }, [changed]);
      return <WorkbenchBlockerResolution client={changed ? nextClient : oldClient} item={item} source={source} goobers={goobers} />;
    }
    const { rerender } = render(<Host changed={false} />); await open(); fill(); start(); await screen.findByRole("button", { name: "Retry same resolution request" });
    rerender(<Host changed />); await waitFor(() => expect(screen.getByRole("button", { name: "Resolve blocker" })).toBeEnabled());
    expect(screen.queryByRole("button", { name: "Retry same resolution request" })).not.toBeInTheDocument();
    expect(nextClient.createSession).not.toHaveBeenCalled(); expect(nextClient.sendSessionMessage).not.toHaveBeenCalled();
    expect(oldClient.sendSessionMessage).toHaveBeenCalledTimes(1);
  });
});
