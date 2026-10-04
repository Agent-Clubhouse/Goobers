import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { BacklogItem, BacklogPage, WorkbenchSourcePage } from "../api/types";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { WorkbenchPanel } from "./WorkbenchPanel";

const sources: WorkbenchSourcePage = { generation: "generation-a", items: [
  { bindingId: "planning", kind: "backlog", provider: "github", owner: "acme", repository: "factory" },
  { bindingId: "roadmap", kind: "backlog", provider: "ado", owner: "acme", project: "Tools" },
  { bindingId: "strategy", kind: "documents", provider: "github", owner: "acme", repository: "strategy", branch: "main", paths: ["objectives/plan.md"] },
] };
const item: BacklogItem = {
  ref: { gaggleId: "team", sourceBindingId: "planning", kind: "work-item", sourceId: "stable-101" },
  locator: { id: "12", url: "https://github.com/acme/factory/issues/12" }, revision: "2026-10-04T12:00:00Z", revisionSemantics: "timestamp-preflight",
  type: "Epic", title: "Human operations", description: "Scope the human surface.", state: "open", objective: true,
  relationshipCoverage: { parents: "not-loaded", blockers: "partial", milestones: "complete" },
  relationships: [
    { kind: "parent-of", incoming: true, target: { kind: "work-item", stableId: "unknown", locator: { id: "7", url: "https://example.test/7" } } },
    { kind: "milestone-member", target: { ref: { gaggleId: "team", sourceBindingId: "planning", kind: "milestone", sourceId: "milestone-6" }, kind: "milestone", locator: { id: "6" } } },
    { kind: "blocked-by", target: { ref: { gaggleId: "team", sourceBindingId: "roadmap", kind: "work-item", sourceId: "42" }, kind: "work-item", locator: { id: "42" } } },
  ],
};
const page: BacklogPage = { items: [item], nextCursor: "opaque/source+cursor=", exhausted: false, partial: true, reasons: ["more-candidates"], candidates: 50, omitted: 3, sourceTargetDigest: "digest-a" };
function setup() {
  const client = new FixtureDaemonClient(populatedDaemonFixtures());
  vi.spyOn(client, "listWorkbenchSources").mockResolvedValue(sources);
  vi.spyOn(client, "getWorkbenchItems").mockResolvedValue(page);
  vi.spyOn(client, "getWorkbenchItem").mockResolvedValue(item);
  const view = render(<WorkbenchPanel client={client} gaggle="team" />);
  return { client, ...view };
}
async function selectSource(name = "planning") {
  fireEvent.change(await screen.findByLabelText("Planning source"), { target: { value: name } });
}
async function selectItem() {
  await selectSource();
  fireEvent.click(await screen.findByRole("button", { name: /Human operations/ }));
  await screen.findByText(item.description!);
}

describe("WorkbenchPanel", () => {
  it("loads only a selected bounded window, replaces it on paging and treats objective status as classification", async () => {
    const { client } = setup();
    await screen.findByLabelText("Planning source"); expect(client.getWorkbenchItems).not.toHaveBeenCalled();
    await selectItem();
    expect(client.getWorkbenchItems).toHaveBeenCalledTimes(1);
    expect(client.getWorkbenchItems).toHaveBeenCalledWith("team", "planning", { cursor: undefined, limit: 50 }, expect.objectContaining({ signal: expect.any(AbortSignal) }));
    expect(client.getWorkbenchItem).toHaveBeenCalledWith("team", "planning", { id: "12", expectedSourceId: "stable-101" }, expect.anything());
    expect(screen.getByText(/does not establish completion or outcome progress/)).toBeInTheDocument();
    expect(screen.getByText(/1 items shown · 50 candidates checked · 3 omitted/)).toBeInTheDocument();
    expect(screen.getByText(/Partial coverage/)).toBeInTheDocument();
    vi.mocked(client.getWorkbenchItems).mockResolvedValueOnce({ ...page, items: [], nextCursor: undefined, exhausted: true });
    fireEvent.click(screen.getByRole("button", { name: "Next items" }));
    expect(screen.queryByText(item.description!)).not.toBeInTheDocument();
    await screen.findByText(/End of this provider window/);
    expect(client.getWorkbenchItems).toHaveBeenLastCalledWith("team", "planning", { cursor: page.nextCursor, limit: 50 }, expect.anything());
    expect(screen.queryByRole("button", { name: /Human operations/ })).not.toBeInTheDocument();
    expect(screen.getByText(/does not establish that the source is empty/)).toBeInTheDocument();
  });
  it("preserves native direction and separates missing relation coverage from unverified links", async () => {
    const { client } = setup(); await selectItem();
    const detail = screen.getByRole("region", { name: "Backlog item details" });
    expect(within(detail).getByText("Parent")).toBeInTheDocument();
    expect(within(detail).getByText("Milestone")).toBeInTheDocument();
    expect(within(detail).getByText("Blocked by")).toBeInTheDocument();
    expect(within(detail).getByText("Relationships not loaded.")).toBeInTheDocument();
    expect(within(detail).getByText("Partial relationship coverage.")).toBeInTheDocument();
    expect(within(detail).getByText(/Unverified source link; content not loaded/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Load related item 7" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Load related item 6" })).not.toBeInTheDocument();
    expect(client.getWorkbenchItem).toHaveBeenCalledTimes(1);
    const related: BacklogItem = { ...item, ref: { ...item.ref, sourceBindingId: "roadmap", sourceId: "42" }, locator: { id: "42" }, title: "Permissions", description: "Explicit operator grants" };
    vi.mocked(client.getWorkbenchItem).mockResolvedValueOnce(related);
    fireEvent.click(screen.getByRole("button", { name: "Load related item 42" }));
    await screen.findByText("Explicit operator grants");
    expect(client.getWorkbenchItem).toHaveBeenLastCalledWith("team", "roadmap", { id: "42", expectedSourceId: "42" }, expect.anything());
  });
  it("renders provider text without HTML execution and refuses unsafe or credential-bearing links", async () => {
    const { client } = setup();
    vi.mocked(client.getWorkbenchItem).mockResolvedValue({ ...item, description: '<img src=x onerror="alert(1)"> [not a link](javascript:alert(1))', locator: { id: "12", url: "javascript:alert(1)" }, relationships: [{ kind: "references", target: { kind: "document", locator: { id: "unsafe", url: "https://name:secret@example.test/path" } } }] });
    await selectSource(); fireEvent.click(await screen.findByRole("button", { name: /Human operations/ }));
    await screen.findByText(/<img src=x/);
    const detail = screen.getByRole("region", { name: "Backlog item details" });
    expect(detail.querySelector("img")).toBeNull(); expect(within(detail).queryByRole("link")).not.toBeInTheDocument();
  });
  it("clears all source data on access failure and ignores an old response after changing source", async () => {
    const { client } = setup(); await selectItem();
    vi.mocked(client.listWorkbenchSources).mockRejectedValueOnce(new Error("access revoked"));
    fireEvent.click(screen.getByRole("button", { name: "Refresh sources" }));
    expect(screen.queryByText(item.description!)).not.toBeInTheDocument();
    await screen.findByText(/Planning sources are unavailable/);
    expect(screen.queryByLabelText("Planning source")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Refresh sources" }));
    await selectSource();
    let resolveOld!: (value: BacklogItem) => void;
    vi.mocked(client.getWorkbenchItem).mockImplementationOnce(() => new Promise((resolve) => { resolveOld = resolve; }));
    fireEvent.click(await screen.findByRole("button", { name: /Human operations/ }));
    await screen.findByText("Loading item details…");
    await selectSource("strategy");
    resolveOld(item);
    await screen.findByText(/Documents are unavailable/);
    expect(screen.queryByText(item.description!)).not.toBeInTheDocument();
  });
  it("discards a stale locator response and a changed source target", async () => {
    const { client } = setup();
    vi.mocked(client.getWorkbenchItem).mockResolvedValueOnce({ ...item, ref: { ...item.ref, sourceId: "replacement" } });
    await selectSource(); fireEvent.click(await screen.findByRole("button", { name: /Human operations/ }));
    await screen.findByText(/Source access or identity changed/);
    expect(screen.queryByText(item.description!)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Refresh sources" }));
    await screen.findByRole("button", { name: /Human operations/ });
    vi.mocked(client.getWorkbenchItems).mockResolvedValueOnce({ ...page, sourceTargetDigest: "new-target" });
    fireEvent.click(screen.getByRole("button", { name: "Next items" }));
    await screen.findByText(/source changed while browsing/);
    expect(screen.queryByRole("button", { name: /Human operations/ })).not.toBeInTheDocument();
  });
  it("drops a prior client's content while new client authorization is pending", async () => {
    const { client, rerender } = setup(); await selectItem();
    const next = new FixtureDaemonClient(populatedDaemonFixtures());
    vi.spyOn(next, "listWorkbenchSources").mockReturnValue(new Promise(() => {}));
    rerender(<WorkbenchPanel client={next} gaggle="team" />);
    expect(screen.queryByText(item.description!)).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Planning source")).not.toBeInTheDocument();
    await waitFor(() => expect(next.listWorkbenchSources).toHaveBeenCalledTimes(1));
    expect(client.getWorkbenchItem).toHaveBeenCalledTimes(1);
  });
});
