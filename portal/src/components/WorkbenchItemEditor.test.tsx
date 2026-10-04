import { useLayoutEffect } from "react";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { DaemonAuthError } from "../api/errors";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { BacklogEditCommand, BacklogItem, BacklogWriteCapabilities, DaemonClient } from "../api/types";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { WorkbenchItemEditor } from "./WorkbenchItemEditor";
import { WorkbenchItemDetail } from "./WorkbenchItemDetail";

const item: BacklogItem = { ref: { gaggleId: "team", sourceBindingId: "issues", sourceId: "987654", kind: "work-item" }, locator: { id: "42" }, title: "Old title", description: "Existing context", state: "open", type: "Issue", revision: "revision-one", revisionSemantics: "timestamp-preflight", objective: false, labels: ["bug", "goobers:needs-human", "Goobers:claim-run:one"], assignees: ["alice"], relationshipCoverage: { parents: "not-loaded", blockers: "not-loaded", milestones: "not-loaded" } };
const capabilities: BacklogWriteCapabilities = { fields: ["title", "description", "labels", "assignees"], relationships: [], revisionSemantics: "timestamp-preflight", maxAssignees: 10, controlLabelChanges: false };
function receipt(state: BacklogEditCommand["state"] = "confirmed"): BacklogEditCommand {
  return { id: `workbench-${"a".repeat(32)}`, gaggle: "team", sourceBindingId: "issues", actor: { issuer: "https://identity.example", subject: "alice" }, itemId: "42", sourceId: "987654", field: "title", state, duplicate: false, requestDigest: "b".repeat(64), operationDigest: "c".repeat(64), acceptedAt: "2026-10-04T12:00:00Z", nextAction: state === "confirmed" ? "Refresh before another edit." : "Inspect the source; do not retry.", receipt: { operationDigest: "c".repeat(64), outcome: state === "confirmed" ? "confirmed" : "unknown", revisionSemantics: "timestamp-preflight", providerAcknowledged: state === "confirmed", observedMatches: true } };
}
function setup(value: BacklogItem = item) {
  const client = new FixtureDaemonClient(populatedDaemonFixtures());
  vi.spyOn(client, "getWorkbenchWriteCapabilities").mockResolvedValue(capabilities);
  vi.spyOn(client, "patchWorkbenchItem").mockResolvedValue(receipt());
  vi.spyOn(client, "getWorkbenchCommand").mockResolvedValue(receipt("unknown"));
  vi.spyOn(client, "getWorkbenchItem").mockResolvedValue({ ...item, title: "Updated", revision: "revision-two" });
  const refreshed = vi.fn();
  const view = render(<WorkbenchItemEditor client={client} item={value} refreshed={refreshed} />);
  return { client, refreshed, ...view };
}
async function editTitle(title = "Revised title") {
  fireEvent.change(await screen.findByLabelText("New value"), { target: { value: title } });
  fireEvent.click(screen.getByRole("button", { name: "Apply field change" }));
}

describe("WorkbenchItemEditor", () => {
  it("uses current server capabilities and sends one source-pinned field without human authority in the body", async () => {
    const { client } = setup();
    const field = await screen.findByLabelText("Field");
    expect(field.querySelector('option[value="state"]')).toBeNull();
    expect(client.patchWorkbenchItem).not.toHaveBeenCalled();
    await editTitle(); await screen.findByText("Change confirmed");
    const [gaggle, source, id, key, body] = vi.mocked(client.patchWorkbenchItem).mock.calls[0];
    expect([gaggle, source, id]).toEqual(["team", "issues", "42"]); expect(key).toMatch(/^[0-9a-f-]{36}$/);
    expect(body).toEqual({ sourceId: "987654", expectedRevision: "revision-one", field: "title", value: "Revised title" });
    expect(screen.getByText(/GitHub is checked before/)).toBeInTheDocument();
    expect(screen.getByText("alice · https://identity.example")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Apply field change" })).toBeDisabled();
  });
  it("preserves control labels while editing the native set", async () => {
    const { client } = setup();
    vi.mocked(client.patchWorkbenchItem).mockResolvedValue({ ...receipt(), field: "labels" });
    fireEvent.change(await screen.findByLabelText("Field"), { target: { value: "labels" } });
    expect(screen.getByLabelText("Values (one per line)")).toHaveValue("bug");
    fireEvent.change(screen.getByLabelText("Values (one per line)"), { target: { value: "feature\nscoping" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply field change" })); await screen.findByText("Change confirmed");
    expect(vi.mocked(client.patchWorkbenchItem).mock.calls[0][4]).toEqual({ sourceId: "987654", expectedRevision: "revision-one", field: "labels", values: ["feature", "scoping", "goobers:needs-human", "Goobers:claim-run:one"] });
  });
  it("recovers a lost reply with the same immutable request and never replays a known unknown outcome", async () => {
    const { client, rerender, refreshed } = setup();
    vi.mocked(client.patchWorkbenchItem).mockRejectedValueOnce(new Error("lost reply")).mockResolvedValueOnce(receipt("unknown"));
    await editTitle(); await screen.findByText(/No reliable reply/);
    const first = vi.mocked(client.patchWorkbenchItem).mock.calls[0];
    rerender(<WorkbenchItemEditor client={client} item={{ ...item, revision: "someone-elses-revision" }} refreshed={refreshed} />);
    expect(screen.getByRole("button", { name: "Apply field change" })).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "Check same command" })); await screen.findByText("Outcome uncertain");
    const second = vi.mocked(client.patchWorkbenchItem).mock.calls[1]; expect(second.slice(0, 5)).toEqual(first.slice(0, 5));
    expect(screen.queryByRole("button", { name: "Check same command" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Refresh for another edit" })).not.toBeInTheDocument();
    expect(screen.getByText(/matching observation does not prove/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Check receipt" })); await waitFor(() => expect(client.getWorkbenchCommand).toHaveBeenCalledTimes(1));
    expect(client.patchWorkbenchItem).toHaveBeenCalledTimes(2);
  });
  it("refreshes the item and permission before opening another settled edit", async () => {
    const { client, refreshed } = setup(); await editTitle(); await screen.findByText("Change confirmed");
    fireEvent.click(screen.getByRole("button", { name: "Refresh for another edit" }));
    await waitFor(() => expect(refreshed).toHaveBeenCalledWith(expect.objectContaining({ revision: "revision-two" })));
    expect(client.getWorkbenchWriteCapabilities).toHaveBeenCalledTimes(2);
    // Parent publishes the freshly verified item, as the real detail component does.
    const latest = vi.mocked(client.getWorkbenchItem).mock.results[0]; expect(latest.type).toBe("return");
    expect(screen.queryByText("Change confirmed")).not.toBeInTheDocument();
    await editTitle("Second edit"); await screen.findByText("Change confirmed");
    expect(vi.mocked(client.patchWorkbenchItem).mock.calls[1][4]).toMatchObject({ expectedRevision: "revision-two", value: "Second edit" });
  });
  it("clears command evidence after current authorization is revoked", async () => {
    const { client } = setup(); await editTitle(); await screen.findByText("Change confirmed");
    vi.mocked(client.getWorkbenchCommand).mockRejectedValue(new DaemonAuthError(403));
    fireEvent.click(screen.getByRole("button", { name: "Check receipt" })); await screen.findByText(/Native editing is unavailable/);
    expect(screen.queryByText(receipt().id)).not.toBeInTheDocument();
    expect(screen.queryByLabelText("New value")).not.toBeInTheDocument();
  });
  it("joins old view requests and ignores late replies when the source identity changes", async () => {
    const { client, rerender, refreshed } = setup(); let finish!: (value: BacklogEditCommand) => void;
    vi.mocked(client.patchWorkbenchItem).mockImplementationOnce(() => new Promise((resolve) => { finish = resolve; }));
    await editTitle();
    const signal = vi.mocked(client.patchWorkbenchItem).mock.calls[0][5]?.signal;
    const other = { ...item, ref: { ...item.ref, sourceId: "other-item" }, locator: { id: "43" }, title: "Other item" };
    rerender(<WorkbenchItemEditor client={client} item={other} refreshed={refreshed} />);
    expect(signal?.aborted).toBe(true);
    await act(async () => finish(receipt()));
    expect(screen.queryByText("Change confirmed")).not.toBeInTheDocument();
    expect(screen.queryByText(receipt().id)).not.toBeInTheDocument();
  });
  it("rejects a response for another item without enabling a new write", async () => {
    const { client } = setup(); vi.mocked(client.patchWorkbenchItem).mockResolvedValue({ ...receipt(), sourceId: "different" });
    await editTitle(); await screen.findByText(/No reliable reply/);
    expect(screen.queryByText("Change confirmed")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Apply field change" })).toBeDisabled();
  });
  it("requires a source revision and never uses the configuration allowlist as human permission", async () => {
    const { client } = setup({ ...item, revision: undefined });
    await screen.findByText(/native source revision is required/);
    expect(screen.queryByRole("button", { name: "Apply field change" })).not.toBeInTheDocument();
    expect(client.patchWorkbenchItem).not.toHaveBeenCalled();
  });
  it("keeps an unresolved command mounted while the actual detail view refreshes", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    vi.spyOn(client, "getWorkbenchWriteCapabilities").mockResolvedValue(capabilities);
    vi.spyOn(client, "getWorkbenchItem").mockResolvedValue(item);
    vi.spyOn(client, "patchWorkbenchItem").mockRejectedValueOnce(new Error("lost reply")).mockResolvedValueOnce(receipt("unknown"));
    render(<WorkbenchItemDetail client={client} gaggle="team" selected={item} sources={[]} unavailable={vi.fn()} />);
    await editTitle(); await screen.findByText(/No reliable reply/);
    const first = vi.mocked(client.patchWorkbenchItem).mock.calls[0];
    let finish!: (value: BacklogItem) => void;
    vi.mocked(client.getWorkbenchItem).mockImplementationOnce(() => new Promise((resolve) => { finish = resolve; }));
    fireEvent.click(screen.getByRole("button", { name: "Refresh item" }));
    await screen.findByText("Loading item details…");
    expect(screen.getByRole("button", { name: "Check same command" })).toBeInTheDocument();
    await act(async () => finish({ ...item, revision: "revision-two" }));
    fireEvent.click(screen.getByRole("button", { name: "Check same command" })); await screen.findByText("Outcome uncertain");
    expect(vi.mocked(client.patchWorkbenchItem).mock.calls[1].slice(0, 5)).toEqual(first.slice(0, 5));
  });
  it("removes old command controls before layout effects when the client changes", async () => {
    const first = new FixtureDaemonClient(populatedDaemonFixtures());
    const next = new FixtureDaemonClient(populatedDaemonFixtures());
    vi.spyOn(first, "getWorkbenchWriteCapabilities").mockResolvedValue(capabilities);
    vi.spyOn(first, "patchWorkbenchItem").mockRejectedValue(new Error("lost reply"));
    vi.spyOn(next, "getWorkbenchWriteCapabilities").mockImplementation(() => new Promise(() => {}));
    vi.spyOn(next, "patchWorkbenchItem").mockResolvedValue(receipt());
    let oldControlVisible = false;
    function Host({ client }: { client: DaemonClient }) {
      useLayoutEffect(() => {
        if (client !== next) return;
        const oldControl = screen.queryByRole("button", { name: "Check same command" });
        oldControlVisible = !!oldControl;
        oldControl?.click();
      }, [client]);
      return <WorkbenchItemEditor client={client} item={item} refreshed={vi.fn()} />;
    }
    const view = render(<Host client={first} />);
    await editTitle(); await screen.findByText(/No reliable reply/);
    view.rerender(<Host client={next} />);
    expect(oldControlVisible).toBe(false);
    expect(next.patchWorkbenchItem).not.toHaveBeenCalled();
    expect(screen.queryByLabelText("Edit backlog item")).not.toBeInTheDocument();
    expect(screen.getByText("Checking editing access…")).toBeInTheDocument();
  });

});
