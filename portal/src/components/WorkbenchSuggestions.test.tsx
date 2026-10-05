import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { DaemonClient, InteractiveCapabilities, SourceView, SuggestionCandidate, SuggestionReview } from "../api/types";
import { goWireFixtures } from "../api/wire.generated";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { WorkbenchSuggestions } from "./WorkbenchSuggestions";

const f = goWireFixtures;
const gaggle = "web";
const source: SourceView = { bindingId: "strategy", kind: "documents", provider: "github", owner: "acme", repository: "strategy", branch: "planning", paths: ["plan.md"], writeRelationships: ["references"] };
const capabilities: InteractiveCapabilities = { gaggle, policyConfigured: true, viewer: true, operator: true, sourceWriteMode: "pull-request", actions: [{ action: "source.proposeChange", authorized: true, credentialConfigured: true, available: true, reasonCode: "" }] };
function setup() {
  const client = new FixtureDaemonClient(populatedDaemonFixtures());
  vi.spyOn(client, "getInteractiveCapabilities").mockResolvedValue(capabilities);
  vi.spyOn(client, "listSuggestionArtifacts").mockResolvedValue(f.suggestionInventory);
  vi.spyOn(client, "loadSuggestions").mockResolvedValue(f.suggestionBatch);
  vi.spyOn(client, "previewSuggestion").mockResolvedValue(f.suggestionPreview);
  vi.spyOn(client, "decideSuggestion").mockResolvedValue(f.suggestionReview);
  vi.spyOn(client, "getSuggestionReview").mockResolvedValue(f.suggestionReview);
  vi.spyOn(client, "checkMetadataProposal").mockResolvedValue(f.metadataProposal);
  vi.spyOn(client, "continueMetadataProposal").mockResolvedValue(f.metadataProposal);
  render(<WorkbenchSuggestions client={client} gaggle={gaggle} sources={[source]} />);
  fireEvent.click(screen.getByText("Review agent relationship suggestions"));
  return client;
}
async function select() {
  fireEvent.change(screen.getByLabelText("Suggestion run ID"), { target: { value: f.suggestionSelection.runId } });
  fireEvent.click(screen.getByRole("button", { name: "List run artifacts" }));
  fireEvent.click(await screen.findByRole("button", { name: /suggestions.json/ }));
  fireEvent.click(await screen.findByRole("button", { name: /→ references →/ }));
  await waitFor(() => expect(screen.getByRole("button", { name: "Reject suggestion" })).toBeEnabled());
}
async function previewAndAccept() {
  await select();
  fireEvent.click(screen.getByRole("button", { name: "Preview relationship PR" }));
  const button = await screen.findByRole("button", { name: "Accept suggestion and open draft PR" });
  expect(button).toBeDisabled();
  fireEvent.click(screen.getByLabelText("I reviewed this exact source diff."));
  fireEvent.click(button);
}
describe("relationship suggestion review", () => {
  it("requires explicit artifact selection and reviewed exact source before acceptance", async () => {
    const client = setup();
    expect(client.listSuggestionArtifacts).not.toHaveBeenCalled();
    expect(client.loadSuggestions).not.toHaveBeenCalled();
    await previewAndAccept();
    await screen.findByText(/Review (accepted|rejected):/);
    expect(client.previewSuggestion).toHaveBeenCalledExactlyOnceWith(gaggle, f.suggestionPreviewRequest, expect.anything());
    expect(client.decideSuggestion).toHaveBeenCalledExactlyOnceWith(gaggle, { ...f.suggestionDecision, reason: "" }, expect.anything());
  });
  it("retains the exact decision after an uncertain response and never retries automatically", async () => {
    const client = setup();
    vi.mocked(client.decideSuggestion).mockRejectedValueOnce(new TypeError("lost response"));
    await previewAndAccept(); await screen.findByRole("alert");
    expect(client.decideSuggestion).toHaveBeenCalledTimes(1);
    const first = vi.mocked(client.decideSuggestion as DaemonClient["decideSuggestion"]).mock.calls[0][1];
    fireEvent.click(screen.getByRole("button", { name: "Retry same decision" }));
    await screen.findByText(/Review (accepted|rejected):/);
    expect(vi.mocked(client.decideSuggestion as DaemonClient["decideSuggestion"]).mock.calls[1][1]).toEqual(first);
    expect(client.previewSuggestion).toHaveBeenCalledTimes(1);
  });
  it("shows unsupported suggestions as inert text and permits explicit rejection without a preview", async () => {
    const client = setup();
    const candidate: SuggestionCandidate = structuredClone(f.suggestionBatch.candidates[0]);
    candidate.supported = false; candidate.reason = "native relationship editing unavailable";
    candidate.suggestion.proposal.rationale = '<img src="https://tracker.test" onerror="alert(1)">';
    vi.mocked(client.loadSuggestions).mockResolvedValue({ ...f.suggestionBatch, candidates: [candidate] });
    vi.mocked(client.decideSuggestion).mockResolvedValue({ ...f.suggestionReview, decision: "reject", state: "rejected", proposal: undefined });
    await select();
    expect(screen.getByRole("button", { name: "Preview relationship PR" })).toBeDisabled();
    expect(screen.getByRole("region", { name: "Selected relationship suggestion" }).querySelector("img,script,iframe")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Reject suggestion" }));
    await screen.findByText(/Review (accepted|rejected):/);
    expect(client.previewSuggestion).not.toHaveBeenCalled();
    expect(client.decideSuggestion).toHaveBeenCalledExactlyOnceWith(gaggle, { selection: f.suggestionSelection, key: candidate.suggestion.key, decision: "reject", reason: "" }, expect.anything());
  });
  it("loads retained review custody and separates provider observation from explicit continuation", async () => {
    const client = setup();
    const review: SuggestionReview = { ...f.suggestionReview, proposal: { ...f.metadataProposal, gaggle, sourceBindingId: source.bindingId, state: "unknown" } };
    vi.mocked(client.loadSuggestions).mockResolvedValue({ ...f.suggestionBatch, candidates: [{ ...f.suggestionBatch.candidates[0], reviewId: review.id, reviewState: "linked" }] });
    vi.mocked(client.getSuggestionReview).mockResolvedValue(review);
    vi.mocked(client.checkMetadataProposal).mockResolvedValue({ ...review.proposal!, state: "prepared" });
    vi.mocked(client.continueMetadataProposal).mockResolvedValue({ ...review.proposal!, state: "confirmed" });
    fireEvent.change(screen.getByLabelText("Suggestion run ID"), { target: { value: f.suggestionSelection.runId } });
    fireEvent.click(screen.getByRole("button", { name: "List run artifacts" }));
    fireEvent.click(await screen.findByRole("button", { name: /suggestions.json/ }));
    fireEvent.click(await screen.findByRole("button", { name: /→ references →/ }));
    fireEvent.click(await screen.findByRole("button", { name: "Check retained provider state" }));
    const next = await screen.findByRole("button", { name: "Continue this retained proposal" });
    expect(client.continueMetadataProposal).not.toHaveBeenCalled();
    expect(client.decideSuggestion).not.toHaveBeenCalled();
    fireEvent.click(next);
    await waitFor(() => expect(client.continueMetadataProposal).toHaveBeenCalledExactlyOnceWith(gaggle, source.bindingId, review.proposal!.id, expect.anything()));
  });
});
