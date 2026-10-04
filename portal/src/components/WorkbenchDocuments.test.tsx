import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { DaemonApiError, DaemonAuthError } from "../api/errors";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { SourceView, WorkbenchDocumentPage, WorkbenchEdge, WorkbenchSourcePage } from "../api/types";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { WorkbenchPanel } from "./WorkbenchPanel";

const commit = "a".repeat(40);
const objectiveId = "obj-00000000-0000-0000-0000-000000000001";
const objectiveRef = { gaggleId: "team", sourceBindingId: "strategy", kind: "objective-document", sourceId: objectiveId };
const itemRef = { gaggleId: "team", sourceBindingId: "backlog", kind: "work-item", sourceId: "42" };
const source: SourceView = { bindingId: "strategy", kind: "documents", provider: "github", owner: "acme", repository: "strategy", branch: "planning", paths: ["goal.md", "notes.md", "missing.md", "invalid.md", "large.md", "six.md", "seven.md", "eight.md", "nine.md"] };
const manifestSource: SourceView = { ...source, bindingId: "links", kind: "relationships", paths: ["relationships.yaml"] };
const sources: WorkbenchSourcePage = { generation: "generation-a", items: [source, manifestSource] };
const edge: WorkbenchEdge = { edgeId: "edge-00000000-0000-0000-0000-000000000001", kind: "contributes-to", from: itemRef, to: objectiveRef, rationale: "Explicit source contribution" };
const provenance = { commit, blobId: "b".repeat(40), contentDigest: "d".repeat(64) };
const page: WorkbenchDocumentPage = {
  sourceBindingId: "strategy", repository: { provider: "github", owner: "acme", name: "strategy" }, branch: "planning", commit, sourceTargetDigest: "target-a", startOffset: 0, totalPaths: 9, nextCursor: "opaque/+next=", exhausted: false, coverage: "partial", reasons: ["more-paths", "source-omissions"],
  files: [
    { path: "goal.md", status: "available", provenance, ref: objectiveRef, objective: { schemaVersion: "objectives/v1", objectiveId, title: "Human operations", edges: [edge] }, body: "Objective source text" },
    { path: "notes.md", status: "available", provenance, body: '<img src="https://example.test/track" onerror="alert(1)"> [link](javascript:alert(1))' },
    { path: "missing.md", status: "unavailable" },
    { path: "invalid.md", status: "invalid-source", provenance, body: "INVALID CONTENT MUST NOT BE SHOWN" },
    { path: "large.md", status: "oversized" },
    ...["six.md", "seven.md", "eight.md"].map((path) => ({ path, status: "available" as const, provenance, body: path })),
  ],
};
const lastPage: WorkbenchDocumentPage = { ...page, files: [{ path: "nine.md", status: "available", provenance, body: "Last configured file" }], startOffset: 8, nextCursor: undefined, exhausted: true, reasons: ["window-only"] };
function clientFixture() {
  const client = new FixtureDaemonClient(populatedDaemonFixtures());
  vi.spyOn(client, "listWorkbenchSources").mockResolvedValue(sources);
  vi.spyOn(client, "getWorkbenchDocuments").mockResolvedValue(page);
  return client;
}
function setup() { const client = clientFixture(); return { client, ...render(<WorkbenchPanel client={client} gaggle="team" />) }; }
async function selectSource(binding = "strategy") { fireEvent.change(await screen.findByLabelText("Planning source"), { target: { value: binding } }); }
async function selectFile(path = "goal.md") { fireEvent.click(await screen.findByRole("button", { name: new RegExp(`^${path.replaceAll(".", "\\.")}`) })); }

describe("repository workbench documents", () => {
  it("reads only after selection and shows pinned source identity with authored objective relationships", async () => {
    const { client } = setup();
    await screen.findByLabelText("Planning source"); expect(client.getWorkbenchDocuments).not.toHaveBeenCalled();
    await selectSource(); await selectFile();
    expect(client.getWorkbenchDocuments).toHaveBeenCalledExactlyOnceWith("team", "strategy", { cursor: undefined, limit: 8 }, expect.objectContaining({ signal: expect.any(AbortSignal) }));
    expect(screen.getByText("Configured files 1–8 · 9 configured paths.")).toBeInTheDocument();
    expect(screen.getByText(/Partial coverage/)).toBeInTheDocument();
    const detail = screen.getByRole("region", { name: "Document details" });
    expect(within(detail).getByText(objectiveId)).toBeInTheDocument();
    expect(within(detail).getByText(commit)).toBeInTheDocument();
    expect(within(detail).getByText(provenance.contentDigest)).toBeInTheDocument();
    expect(within(detail).getByText("Contributes to")).toBeInTheDocument();
    expect(within(detail).getByText("team / backlog / work-item / 42")).toBeInTheDocument();
    expect(within(detail).getByText(/does not establish progress or completion/)).toBeInTheDocument();
    expect(within(detail).getByText("Objective source text")).toBeInTheDocument();
  });
  it("renders ordinary Markdown as inert source content and per-file failures without content", async () => {
    setup(); await selectSource(); await selectFile("notes.md");
    const detail = screen.getByRole("region", { name: "Document details" });
    expect(within(detail).getByText(/does not declare an objective identity/)).toBeInTheDocument();
    expect(within(detail).getByText(/<img src=/)).toBeInTheDocument();
    expect(detail.querySelector("img, iframe, script, a")).toBeNull();
    for (const [path, message] of [["missing.md", /could not be read/], ["invalid.md", /source metadata is invalid/], ["large.md", /exceeds the source read limit/]] as const) {
      await selectFile(path); expect(screen.getByText(message)).toBeInTheDocument();
      expect(screen.queryByText("INVALID CONTENT MUST NOT BE SHOWN")).not.toBeInTheDocument();
      expect(screen.queryByText(/<img src=/)).not.toBeInTheDocument();
    }
  });
  it("replaces the bounded window and retains exact pins without crawling other paths", async () => {
    const { client } = setup(); await selectSource(); await selectFile();
    vi.mocked(client.getWorkbenchDocuments).mockResolvedValueOnce(lastPage);
    fireEvent.click(screen.getByRole("button", { name: "Next files" }));
    expect(screen.queryByText("Objective source text")).not.toBeInTheDocument();
    await screen.findByText("Configured files 9–9 · 9 configured paths.");
    expect(screen.queryByRole("button", { name: /goal.md/ })).not.toBeInTheDocument();
    expect(client.getWorkbenchDocuments).toHaveBeenLastCalledWith("team", "strategy", { cursor: page.nextCursor, limit: 8 }, expect.anything());
    expect(screen.queryByRole("button", { name: "Next files" })).not.toBeInTheDocument();
    expect(screen.getByText(/Unavailable files do not establish deletion/)).toBeInTheDocument();
  });
  it("shows aliases and each authored edge direction in bounded manifest windows", async () => {
    const { client } = setup();
    const kinds: WorkbenchEdge["kind"][] = ["parent-of", "blocked-by", "contributes-to", "references", "milestone-member", "implemented-by"];
    const edges = Array.from({ length: 21 }, (_, index) => ({ ...edge, edgeId: `edge-${index}`, kind: kinds[index % kinds.length] }));
    vi.mocked(client.getWorkbenchDocuments).mockResolvedValueOnce({ ...page, sourceBindingId: "links", totalPaths: 1, nextCursor: undefined, exhausted: true, coverage: "complete", reasons: undefined, files: [{ path: "relationships.yaml", status: "available", provenance, manifest: { schemaVersion: "relationships/v1", aliases: Array.from({ length: 21 }, (_, index) => ({ name: `alias-${index}`, target: objectiveRef })), edges } }] });
    await selectSource("links"); await selectFile("relationships.yaml");
    expect(screen.getByText("All configured paths were read at this commit.")).toBeInTheDocument();
    expect(screen.getByText(/Aliases name existing identities/)).toBeInTheDocument();
    const relationships = screen.getByRole("region", { name: "Authored relationships" });
    expect(within(relationships).getAllByRole("listitem")).toHaveLength(20);
    for (const label of ["Parent of", "Blocked by", "Contributes to", "References", "Milestone member", "Implemented by"]) expect(within(relationships).getAllByText(label).length).toBeGreaterThan(0);
    expect(screen.queryByText("alias-20")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Next aliases" }));
    expect(screen.getByText("alias-20")).toBeInTheDocument(); expect(screen.queryByText("alias-0")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Next relationships" }));
    expect(within(relationships).getAllByRole("listitem")).toHaveLength(1);
    expect(screen.getByText("edge-20")).toBeInTheDocument();
    expect(client.getWorkbenchDocuments).toHaveBeenCalledTimes(1);
  });
  it.each(["conflict", "commit", "target"])("drops content and requires an explicit restart on %s change", async (change) => {
    const { client } = setup(); await selectSource(); await selectFile();
    if (change === "conflict") vi.mocked(client.getWorkbenchDocuments).mockRejectedValueOnce(new DaemonApiError(409, "workbench_source_changed", "changed"));
    else vi.mocked(client.getWorkbenchDocuments).mockResolvedValueOnce({ ...lastPage, ...(change === "commit" ? { commit: "c".repeat(40), files: [{ ...lastPage.files[0], provenance: { ...provenance, commit: "c".repeat(40) } }] } : { sourceTargetDigest: "target-b" }) });
    fireEvent.click(screen.getByRole("button", { name: "Next files" }));
    await screen.findByText(/source or branch changed while browsing/);
    expect(screen.queryByRole("region", { name: "Document details" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Next files" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Refresh documents" }));
    await screen.findByRole("button", { name: /goal.md/ });
    expect(client.getWorkbenchDocuments).toHaveBeenLastCalledWith("team", "strategy", { cursor: undefined, limit: 8 }, expect.anything());
  });
  it.each(["binding", "repository", "path", "objective", "provenance"])("refuses content from a changed %s identity", async (change) => {
    const { client } = setup();
    const changed = structuredClone(page);
    if (change === "binding") changed.sourceBindingId = "foreign";
    if (change === "repository") changed.repository.name = "foreign";
    if (change === "path") changed.files[0].path = "not-configured.md";
    if (change === "objective") changed.files[0].ref!.gaggleId = "foreign";
    if (change === "provenance") changed.files[0].provenance = undefined;
    vi.mocked(client.getWorkbenchDocuments).mockResolvedValueOnce(changed);
    await selectSource(); await screen.findByText(/document source identity changed/);
    expect(screen.queryByRole("button", { name: /goal.md/ })).not.toBeInTheDocument();
  });
  it("clears content on access revocation and source refresh never grants read access", async () => {
    const { client } = setup(); await selectSource(); await selectFile();
    vi.mocked(client.getWorkbenchDocuments).mockRejectedValueOnce(new DaemonAuthError(403));
    fireEvent.click(screen.getByRole("button", { name: "Refresh documents" }));
    expect(screen.queryByText("Objective source text")).not.toBeInTheDocument();
    await screen.findByText(/Documents are unavailable/);
    expect(screen.queryByRole("region", { name: "Document details" })).not.toBeInTheDocument();
    expect(screen.getByText(/Source configuration does not grant read access/)).toBeInTheDocument();
  });
  it("ignores late document replies after source, client, or gaggle changes", async () => {
    const { client, rerender } = setup();
    let resolveOld!: (value: WorkbenchDocumentPage) => void;
    vi.mocked(client.getWorkbenchDocuments).mockReturnValueOnce(new Promise((resolve) => { resolveOld = resolve; }));
    await selectSource(); await screen.findByText("Loading configured documents…");
    await selectSource("");
    await act(async () => { resolveOld(page); });
    expect(screen.queryByRole("button", { name: /goal.md/ })).not.toBeInTheDocument();
    await selectSource(); await selectFile();
    const next = clientFixture(); vi.mocked(next.listWorkbenchSources).mockReturnValue(new Promise(() => {}));
    rerender(<WorkbenchPanel client={next} gaggle="team" />);
    expect(screen.queryByText("Objective source text")).not.toBeInTheDocument();
    await waitFor(() => expect(next.listWorkbenchSources).toHaveBeenCalledTimes(1));
    rerender(<WorkbenchPanel client={client} gaggle="another-team" />);
    expect(screen.queryByRole("region", { name: "Document details" })).not.toBeInTheDocument();
    await waitFor(() => expect(client.listWorkbenchSources).toHaveBeenLastCalledWith("another-team", expect.anything()));
  });
  it("discards a prior generation before loading a changed file allowlist", async () => {
    const { client } = setup(); await selectSource(); await selectFile();
    const newSource = { ...source, paths: ["new.md"] };
    vi.mocked(client.listWorkbenchSources).mockResolvedValueOnce({ generation: "generation-b", items: [newSource] });
    vi.mocked(client.getWorkbenchDocuments).mockResolvedValueOnce({ ...page, sourceTargetDigest: "new-target", files: [{ path: "new.md", status: "available", provenance, body: "New generation content" }], totalPaths: 1, nextCursor: undefined, exhausted: true, coverage: "complete" });
    fireEvent.click(screen.getByRole("button", { name: "Refresh sources" }));
    expect(screen.queryByText("Objective source text")).not.toBeInTheDocument();
    await selectFile("new.md");
    expect(screen.getByText("New generation content")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /goal.md/ })).not.toBeInTheDocument();
  });
});
