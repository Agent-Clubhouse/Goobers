import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { DaemonApiError, DaemonAuthError } from "../api/errors";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { DaemonClient, InteractiveCapabilities, MetadataChangeRequest, MetadataPreview, MetadataProposalCommand, SourceView, WorkbenchDocumentFile } from "../api/types";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { MetadataProposalEditor } from "./MetadataProposalEditor";
import { WorkbenchPanel } from "./WorkbenchPanel";
import { MetadataProposalReceipt } from "./MetadataProposalReceipt";

const gaggle = "team";
const source: SourceView = { bindingId: "strategy", kind: "documents", provider: "github", owner: "acme", repository: "strategy", branch: "planning", paths: ["goal.md"], writeFields: ["title", "description"], writeRelationships: ["references", "contributes-to"] };
const backlog: SourceView = { bindingId: "backlog", kind: "backlog", provider: "github", owner: "acme", repository: "product" };
const sources = [source, backlog];
const expected = { commit: "a".repeat(40), blobId: "b".repeat(40), contentDigest: "c".repeat(64) };
const file: WorkbenchDocumentFile = { path: "goal.md", status: "available", provenance: expected, body: "Prior body", ref: { gaggleId: gaggle, sourceBindingId: source.bindingId, kind: "objective-document", sourceId: "obj-00000000-0000-0000-0000-000000000001" }, objective: { schemaVersion: "objectives/v1", objectiveId: "obj-00000000-0000-0000-0000-000000000001", title: "Old title" } };
const request: MetadataChangeRequest = { path: file.path, expected, field: "title", value: "New title" };
const preview: MetadataPreview = { path: file.path, expected, targetDigest: "d".repeat(64), operationDigest: "e".repeat(64), proposedContentDigest: "f".repeat(64), changed: true, before: "Old title", after: '<img src="https://tracking.test/pixel" onerror="alert(1)"> New title' };
const receipt: MetadataProposalCommand = { id: `workbench-${"a".repeat(32)}`, gaggle, sourceBindingId: source.bindingId, path: file.path, actor: { issuer: "test", subject: "human" }, state: "confirmed", duplicate: false, requestDigest: "1".repeat(64), operationDigest: preview.operationDigest, expected, proposedContentDigest: preview.proposedContentDigest, branch: `goobers/workbench/${"a".repeat(32)}`, acceptedAt: "2026-10-04T00:00:00Z", phases: [{ name: "pull-request", outcome: "acknowledged", claimedAt: "2026-10-04T00:00:00Z", pullRequest: { id: "1", number: 1, url: "https://github.com/acme/strategy/pull/1" } }], observations: [], omittedObservations: 0, nextAction: "Review the draft PR." };
const capabilities: InteractiveCapabilities = { gaggle, policyConfigured: true, viewer: true, operator: true, sourceWriteMode: "pull-request", actions: [{ action: "source.proposeChange", authorized: true, credentialConfigured: true, available: true, reasonCode: "" }] };
function clientFixture() {
  const client = new FixtureDaemonClient(populatedDaemonFixtures());
  vi.spyOn(client, "getInteractiveCapabilities").mockResolvedValue(capabilities);
  vi.spyOn(client, "previewMetadataChange").mockResolvedValue(preview);
  vi.spyOn(client, "submitMetadataProposal").mockResolvedValue(receipt);
  vi.spyOn(client, "getMetadataProposal").mockResolvedValue(receipt);
  vi.spyOn(client, "checkMetadataProposal").mockResolvedValue(receipt);
  vi.spyOn(client, "continueMetadataProposal").mockResolvedValue(receipt);
  return client;
}
function setup() { const client = clientFixture(); return { client, ...render(<MetadataProposalEditor client={client} gaggle={gaggle} source={source} sources={sources} file={file} />) }; }
async function makePreview() {
  fireEvent.change(await screen.findByLabelText("Proposed title"), { target: { value: "New title" } });
  fireEvent.click(screen.getByRole("button", { name: "Preview source change" }));
  await screen.findByRole("button", { name: "Create draft PR" });
}
async function submit() { await makePreview(); fireEvent.click(screen.getByRole("button", { name: "Create draft PR" })); }

describe("governed metadata proposal editor", () => {
  it("opens the proposal editor from the configured document browser", async () => {
    const client = clientFixture();
    vi.spyOn(client, "listWorkbenchSources").mockResolvedValue({ generation: "current", items: sources });
    vi.spyOn(client, "getWorkbenchDocuments").mockResolvedValue({ sourceBindingId: source.bindingId, repository: { provider: "github", owner: "acme", name: "strategy" }, branch: "planning", commit: expected.commit, sourceTargetDigest: "d".repeat(64), files: [file], startOffset: 0, totalPaths: 1, exhausted: true, coverage: "complete" });
    render(<WorkbenchPanel client={client} gaggle={gaggle} />);
    fireEvent.change(await screen.findByLabelText("Planning source"), { target: { value: "strategy" } });
    fireEvent.click(await screen.findByRole("button", { name: /goal.md/ }));
    await makePreview(); expect(client.submitMetadataProposal).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Create draft PR" }));
    await screen.findByText("Draft PR confirmed");
  });
  it("requires exact preview review before any submission and renders source as inert text", async () => {
    const { client } = setup(); await makePreview();
    expect(client.previewMetadataChange).toHaveBeenCalledExactlyOnceWith(gaggle, source.bindingId, request, expect.anything());
    expect(client.submitMetadataProposal).not.toHaveBeenCalled();
    expect(screen.getByRole("region", { name: "Source change preview" }).querySelector("img,script,iframe,a")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Create draft PR" }));
    expect(await screen.findByText("Draft PR confirmed")).toBeInTheDocument();
    expect(client.submitMetadataProposal).toHaveBeenCalledExactlyOnceWith(gaggle, source.bindingId, expect.any(String), request, expect.anything());
    expect(screen.getByRole("link", { name: "Review draft PR #1" })).toHaveAttribute("href", "https://github.com/acme/strategy/pull/1");
    expect(screen.getByLabelText("Proposed title")).toBeDisabled();
  });
  it("invalidates preview on edits and refuses unchanged or mismatched preview identity", async () => {
    const { client } = setup(); await makePreview();
    fireEvent.change(screen.getByLabelText("Proposed title"), { target: { value: "Different" } });
    expect(screen.queryByRole("button", { name: "Create draft PR" })).not.toBeInTheDocument();
    vi.mocked(client.previewMetadataChange).mockResolvedValueOnce({ ...preview, changed: false });
    fireEvent.click(screen.getByRole("button", { name: "Preview source change" }));
    expect(await screen.findByRole("button", { name: "Create draft PR" })).toBeDisabled();
    vi.mocked(client.previewMetadataChange).mockResolvedValueOnce({ ...preview, path: "foreign.md" });
    fireEvent.click(screen.getByRole("button", { name: "Preview source change" }));
    await screen.findByRole("alert"); expect(screen.queryByRole("button", { name: "Create draft PR" })).not.toBeInTheDocument();
    expect(client.submitMetadataProposal).not.toHaveBeenCalled();
  });
  it("reuses the same immutable request after lost submission response, with no automatic retry", async () => {
    const { client } = setup();
    vi.mocked(client.submitMetadataProposal).mockRejectedValueOnce(new TypeError("connection lost"));
    await submit(); await screen.findByRole("alert");
    expect(client.submitMetadataProposal).toHaveBeenCalledTimes(1);
    const first = vi.mocked(client.submitMetadataProposal).mock.calls[0];
    fireEvent.click(screen.getByRole("button", { name: "Retry same proposal request" }));
    await screen.findByText("Draft PR confirmed");
    expect(vi.mocked(client.submitMetadataProposal).mock.calls[1].slice(0, 4)).toEqual(first.slice(0, 4));
  });
  it("observes unknown effects without submission and requires a later explicit continuation", async () => {
    const { client } = setup();
    vi.mocked(client.submitMetadataProposal).mockResolvedValueOnce({ ...receipt, state: "unknown", phases: [{ name: "branch", outcome: "unknown", claimedAt: receipt.acceptedAt }] });
    vi.mocked(client.checkMetadataProposal).mockResolvedValueOnce({ ...receipt, state: "prepared", phases: [{ name: "branch", outcome: "unknown", claimedAt: receipt.acceptedAt }], observations: [{ phase: 0, at: receipt.acceptedAt, found: true, matches: true }] });
    await submit(); await screen.findByText("Provider outcome uncertain");
    fireEvent.click(screen.getByRole("button", { name: "Check provider state" }));
    await screen.findByText("Proposal ready for the next phase");
    expect(client.submitMetadataProposal).toHaveBeenCalledTimes(1); expect(client.continueMetadataProposal).not.toHaveBeenCalled();
    expect(screen.getByText(/branch: unknown/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Continue retained proposal" }));
    await screen.findByText("Draft PR confirmed"); expect(client.continueMetadataProposal).toHaveBeenCalledExactlyOnceWith(gaggle, source.bindingId, receipt.id, expect.anything());
  });
  it.each(["client", "revision", "source"])("clears old preview and ignores late responses when %s changes", async (change) => {
    const { client, rerender } = setup(); await makePreview();
    let resolve!: (value: MetadataProposalCommand) => void;
    vi.mocked(client.submitMetadataProposal).mockReturnValueOnce(new Promise((done) => { resolve = done; }));
    fireEvent.click(screen.getByRole("button", { name: "Create draft PR" }));
    const next = clientFixture();
    rerender(<MetadataProposalEditor client={change === "client" ? next : client} gaggle={gaggle} source={change === "source" ? { ...source, repository: "different" } : source} sources={sources} file={change === "revision" ? { ...file, provenance: { ...expected, commit: "9".repeat(40) } } : file} />);
    expect(screen.queryByRole("region", { name: "Source change preview" })).not.toBeInTheDocument();
    await act(async () => { resolve(receipt); });
    expect(screen.queryByText("Draft PR confirmed")).not.toBeInTheDocument();
  });
  it("clears retained content and effects on revoked access; rejects foreign command receipts", async () => {
    const { client } = setup(); vi.mocked(client.submitMetadataProposal).mockResolvedValueOnce({ ...receipt, gaggle: "foreign" });
    await submit(); await screen.findByRole("alert"); expect(screen.queryByRole("region", { name: "Metadata proposal receipt" })).not.toBeInTheDocument();
    vi.mocked(client.submitMetadataProposal).mockRejectedValueOnce(new DaemonAuthError(403));
    fireEvent.click(screen.getByRole("button", { name: "Retry same proposal request" }));
    await screen.findByText(/Repository proposals are unavailable/);
    expect(screen.queryByRole("region", { name: "Source change preview" })).not.toBeInTheDocument();
    expect(screen.queryByText(/Request key:/)).not.toBeInTheDocument();
  });
  it("requires current implemented authority in addition to the configured writes", async () => {
    const client = clientFixture(); vi.mocked(client.getInteractiveCapabilities).mockResolvedValueOnce({ ...capabilities, actions: [{ ...capabilities.actions[0], available: false }] });
    render(<MetadataProposalEditor client={client} gaggle={gaggle} source={source} sources={sources} file={file} />);
    await screen.findByText(/Repository proposals are unavailable/); expect(client.previewMetadataChange).not.toHaveBeenCalled();
  });
  it("sends full source-owned relationship removals and bounded configured target additions", async () => {
    const edge = { edgeId: `edge-${"00000000-0000-0000-0000-000000000003"}`, kind: "references" as const, from: file.ref!, to: { gaggleId: gaggle, sourceBindingId: "backlog", kind: "work-item", sourceId: "native-42" }, rationale: "Observed exact rationale" };
    const client = clientFixture();
    render(<MetadataProposalEditor client={client} gaggle={gaggle} source={source} sources={sources} file={{ ...file, objective: { ...file.objective!, edges: [edge] } }} />);
    fireEvent.change(await screen.findByLabelText("Change"), { target: { value: "relationship-remove" } });
    fireEvent.click(screen.getByRole("button", { name: "Preview source change" })); await screen.findByRole("button", { name: "Create draft PR" });
    expect(client.previewMetadataChange).toHaveBeenLastCalledWith(gaggle, source.bindingId, { path: file.path, expected, relationship: { action: "remove", edge } }, expect.anything());
    fireEvent.change(screen.getByLabelText("Change"), { target: { value: "relationship-add" } });
    fireEvent.change(screen.getByLabelText("To source"), { target: { value: "backlog" } });
    fireEvent.change(screen.getByLabelText("To stable source ID"), { target: { value: "native-43" } });
    fireEvent.click(screen.getByRole("button", { name: "Preview source change" })); await screen.findByRole("button", { name: "Create draft PR" });
    expect(client.previewMetadataChange).toHaveBeenLastCalledWith(gaggle, source.bindingId, { path: file.path, expected, relationship: { action: "add", edge: { edgeId: expect.stringMatching(/^edge-/), kind: "references", from: file.ref, to: { gaggleId: gaggle, sourceBindingId: "backlog", kind: "work-item", sourceId: "native-43" } } } }, expect.anything());
  });
  it("uses an explicit backlog origin for relationship manifests", async () => {
    const client = clientFixture(); const manifestSource = { ...source, kind: "relationships", bindingId: "links", paths: ["links.yaml"], writeFields: [] };
    const manifestFile: WorkbenchDocumentFile = { path: "links.yaml", status: "available", provenance: expected, manifest: { schemaVersion: "relationships/v1", edges: [] } };
    vi.mocked(client.previewMetadataChange).mockResolvedValueOnce({ ...preview, path: "links.yaml" });
    render(<MetadataProposalEditor client={client} gaggle={gaggle} source={manifestSource} sources={[...sources, manifestSource]} file={manifestFile} />);
    fireEvent.change(await screen.findByLabelText("From source"), { target: { value: "backlog" } });
    fireEvent.change(screen.getByLabelText("From stable source ID"), { target: { value: "native-42" } });
    fireEvent.change(screen.getByLabelText("To source"), { target: { value: "strategy" } });
    fireEvent.change(screen.getByLabelText("To stable source ID"), { target: { value: file.ref!.sourceId } });
    fireEvent.click(screen.getByRole("button", { name: "Preview source change" }));
    await waitFor(() => expect(client.previewMetadataChange).toHaveBeenCalledTimes(1));
    const sent = vi.mocked(client.previewMetadataChange as DaemonClient["previewMetadataChange"]).mock.calls[0][2];
    expect(sent.relationship?.edge.from).toEqual({ gaggleId: gaggle, sourceBindingId: "backlog", kind: "work-item", sourceId: "native-42" });
    expect(sent.relationship?.edge.to).toEqual(file.ref);
  });
  it("shows source conflicts without proposing a replacement or emitting another write", async () => {
    const { client } = setup(); vi.mocked(client.previewMetadataChange).mockRejectedValueOnce(new DaemonApiError(409, "source_changed", "private"));
    fireEvent.click(await screen.findByRole("button", { name: "Preview source change" }));
    await screen.findByText(/source or editing policy changed/); expect(screen.queryByText("private")).not.toBeInTheDocument(); expect(client.submitMetadataProposal).not.toHaveBeenCalled();
  });
  it("loads exact retained custody after a refresh without the original edit or a new submission", async () => {
    const client = clientFixture();
    vi.mocked(client.getMetadataProposal).mockResolvedValueOnce({ ...receipt, state: "prepared" });
    render(<MetadataProposalEditor client={client} gaggle={gaggle} source={{ ...source, writeFields: [], writeRelationships: [] }} sources={sources} file={file} />);
    expect(client.getInteractiveCapabilities).not.toHaveBeenCalled();
    fireEvent.change(screen.getByLabelText("Proposal command ID"), { target: { value: receipt.id } });
    fireEvent.click(screen.getByRole("button", { name: "Load retained proposal", hidden: true }));
    await screen.findByText("Proposal ready for the next phase");
    await waitFor(() => expect(client.getInteractiveCapabilities).toHaveBeenCalledTimes(1));
    expect(client.previewMetadataChange).not.toHaveBeenCalled(); expect(client.submitMetadataProposal).not.toHaveBeenCalled(); expect(client.continueMetadataProposal).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Continue this retained proposal", hidden: true }));
    await screen.findByText("Draft PR confirmed"); expect(client.continueMetadataProposal).toHaveBeenCalledExactlyOnceWith(gaggle, source.bindingId, receipt.id, expect.anything());
  });
  it("distinguishes observed PRs and refuses links outside the exact provider target", () => {
    render(<MetadataProposalReceipt source={source} command={{ ...receipt, state: "observed", phases: [], observations: [{ phase: 3, at: receipt.acceptedAt, found: true, matches: true, pullRequest: { id: "1", number: 1, url: "https://github.com/foreign/project/pull/1" } }] }} />);
    expect(screen.getByText("Draft PR observed")).toBeInTheDocument(); expect(screen.getByText(/does not prove/)).toBeInTheDocument(); expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });
});
