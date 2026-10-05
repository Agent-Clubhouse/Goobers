import { useEffect, useRef, useState } from "react";
import { DaemonAuthError } from "../api/errors";
import type { DaemonClient, SourceView, SuggestionBatch, SuggestionCandidate, SuggestionDecisionRequest, SuggestionEndpoint, SuggestionInventory, SuggestionPreview, SuggestionReview } from "../api/types";
import { MetadataProposalReceipt } from "./MetadataProposalReceipt";

/** Explicit artifact selection; candidates never become accepted graph data. */
export function WorkbenchSuggestions({ client, gaggle, sources }: { client: DaemonClient; gaggle: string; sources: SourceView[] }) {
  const [run, setRun] = useState("");
  const [inventory, setInventory] = useState<SuggestionInventory>();
  const [batch, setBatch] = useState<SuggestionBatch>();
  const [candidate, setCandidate] = useState<SuggestionCandidate>();
  const [preview, setPreview] = useState<SuggestionPreview>();
  const [review, setReview] = useState<SuggestionReview>();
  const [pending, setPending] = useState<SuggestionDecisionRequest>();
  const [reason, setReason] = useState("");
  const [approved, setApproved] = useState(false);
  const [canDecide, setCanDecide] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const lifetime = useRef<AbortController>(undefined);
  const active = useRef(false);
  useEffect(() => { const controller = new AbortController(); lifetime.current = controller; return () => controller.abort(); }, []);
  function resetCandidate() { setCandidate(undefined); setPreview(undefined); setReview(undefined); setPending(undefined); setReason(""); setApproved(false); setCanDecide(false); }
  async function perform(action: (signal: AbortSignal) => Promise<void>) {
    const controller = lifetime.current;
    if (!controller || controller.signal.aborted || active.current) return;
    active.current = true; setBusy(true); setError("");
    try { await action(controller.signal); }
    catch (cause) {
      if (controller.signal.aborted) return;
      setPreview(undefined); setApproved(false); setCanDecide(false);
      if (cause instanceof DaemonAuthError) { setBatch(undefined); resetCandidate(); setError("This suggestion is unavailable for your current access."); }
      else setError("The request could not be verified. Keep any retained review or proposal ID. No replacement proposal was sent.");
    } finally { if (!controller.signal.aborted) { active.current = false; setBusy(false); } }
  }
  async function capabilities(signal: AbortSignal) {
    const value = await client.getInteractiveCapabilities(gaggle, { signal });
    if (signal.aborted) return;
    setCanDecide(value.gaggle === gaggle && value.operator && value.sourceWriteMode === "pull-request" && value.actions.some((a) => a.action === "source.proposeChange" && a.authorized && a.available && a.credentialConfigured));
  }
  function list(after?: number) { void perform(async (signal) => {
    resetCandidate(); setBatch(undefined);
    const result = await client.listSuggestionArtifacts(gaggle, run, after, { signal });
    if (signal.aborted) return;
    if (result.runId !== run) throw new Error("Run identity changed");
    setInventory(result);
  }); }
  function load(sequence: number) { void perform(async (signal) => {
    resetCandidate(); setBatch(undefined);
    const result = await client.loadSuggestions(gaggle, { runId: run, sequence }, { signal });
    if (signal.aborted) return;
    if (result.selection.runId !== run || result.selection.sequence !== sequence) throw new Error("Artifact identity changed");
    setBatch(result);
  }); }
  function choose(value: SuggestionCandidate) { void perform(async (signal) => {
    resetCandidate(); setCandidate(value);
    await capabilities(signal);
    if (value.reviewId) {
      const result = await client.getSuggestionReview(gaggle, value.reviewId, { signal });
      if (signal.aborted) return;
      if (result.suggestion.key !== value.suggestion.key || result.id !== value.reviewId) throw new Error("Review identity changed");
      setReview(result);
    }
  }); }
  function previewPR() { if (!candidate || !batch) return; void perform(async (signal) => {
    setPreview(undefined); setApproved(false);
    const result = await client.previewSuggestion(gaggle, { selection: batch.selection, key: candidate.suggestion.key }, { signal });
    if (signal.aborted) return;
    if (!sources.some((source) => source.bindingId === result.sourceBindingId)) throw new Error("Unknown proposal source");
    setPreview(result);
  }); }
  function decide(decision: "accept" | "reject", retry?: SuggestionDecisionRequest) {
    if (!candidate || !batch || (!retry && decision === "accept" && (!preview || !approved))) return;
    const input: SuggestionDecisionRequest = retry ?? { selection: batch.selection, key: candidate.suggestion.key, decision, reason, ...(decision === "accept" && preview ? { expectedOwner: preview.preview.expected, expectedOperationDigest: preview.preview.operationDigest } : {}) };
    setPending(input);
    void perform(async (signal) => {
      const result = await client.decideSuggestion(gaggle, input, { signal });
      if (signal.aborted) return;
      if (result.suggestion.key !== input.key || result.decision !== input.decision) throw new Error("Decision identity changed");
      setReview(result); setPending(undefined); setPreview(undefined); setApproved(false);
    });
  }
  function updateProposal(kind: "check" | "continue") { if (!review?.proposal) return; const prior = review; const command = review.proposal; void perform(async (signal) => {
    const result = kind === "check" ? await client.checkMetadataProposal(gaggle, command.sourceBindingId, command.id, { signal }) : await client.continueMetadataProposal(gaggle, command.sourceBindingId, command.id, { signal });
    if (signal.aborted) return;
    if (result.id !== command.id || result.sourceBindingId !== command.sourceBindingId || result.gaggle !== gaggle) throw new Error("Proposal identity changed");
    setReview({ ...prior, proposal: result });
  }); }
  const source = sources.find((item) => item.bindingId === review?.proposal?.sourceBindingId);
  return <details className="workbench-command"><summary>Review agent relationship suggestions</summary>
    <p>Select a retained run and artifact. Suggestions are proposals; they stay out of the planning graph until their source PR is merged.</p>
    <form onSubmit={(event) => { event.preventDefault(); list(); }}><label>Suggestion run ID<input value={run} required pattern="[a-f0-9]{32}" maxLength={32} disabled={busy} onChange={(event) => { setRun(event.target.value); setInventory(undefined); setBatch(undefined); resetCandidate(); setError(""); }} /></label><button type="submit" disabled={busy}>List run artifacts</button></form>
    {error && <p role="alert">{error}</p>}
    {inventory && <><p>Artifacts contain untrusted output. Only the selected artifact is parsed as relationship suggestions.</p><ul>{inventory.artifacts.map((artifact) => <li key={artifact.sequence}><button type="button" disabled={busy} onClick={() => load(artifact.sequence)}>{artifact.name} · {artifact.stageId} attempt {artifact.attempt} · event {artifact.sequence}</button></li>)}</ul>{inventory.artifacts.length === 0 && <p>No eligible stage artifacts in this window.</p>}{inventory.partial && <button type="button" disabled={busy} onClick={() => list(inventory.nextSequence)}>Next artifact window</button>}</>}
    {batch && <><p>{batch.candidates.length} visible suggestions{batch.omitted > 0 ? ` · ${batch.omitted} unavailable for current source access` : ""}.</p><ul>{batch.candidates.map((value) => <li key={value.suggestion.key}><button type="button" disabled={busy} onClick={() => choose(value)}>{endpointName(value.suggestion.proposal.from)} → {value.suggestion.proposal.kind} → {endpointName(value.suggestion.proposal.to)}</button>{value.reviewState && ` · ${value.reviewState}`}</li>)}</ul></>}
    {candidate && <section aria-label="Selected relationship suggestion"><h4>Suggested {candidate.suggestion.proposal.kind}</h4><p>{candidate.suggestion.proposal.rationale}</p><p>Producer: {candidate.suggestion.origin.runId} · {candidate.suggestion.origin.stageId} attempt {candidate.suggestion.origin.attempt}</p><p>Artifact SHA-256: <code>{candidate.suggestion.origin.artifactDigest}</code></p>
      {!candidate.supported && <p>This operation is unsupported: {candidate.reason}. No fallback source will be edited.</p>}
      {!review && !pending && <><label>Review note<textarea value={reason} maxLength={4096} disabled={busy} onChange={(event) => setReason(event.target.value)} /></label><button type="button" disabled={busy || !canDecide || !candidate.supported} onClick={previewPR}>Preview relationship PR</button><button type="button" disabled={busy || !canDecide} onClick={() => decide("reject")}>Reject suggestion</button></>}
      {preview && !pending && <><h5>Review source diff: {preview.sourceBindingId} / {preview.preview.path}</h5><details><summary>Before</summary><pre>{preview.preview.before}</pre></details><details open><summary>Proposed source</summary><pre>{preview.preview.after}</pre></details><label><input type="checkbox" checked={approved} disabled={busy} onChange={(event) => setApproved(event.target.checked)} />I reviewed this exact source diff.</label><button type="button" disabled={busy || !approved || !canDecide} onClick={() => decide("accept")}>Accept suggestion and open draft PR</button></>}
      {pending && !review && <><p>The decision request is retained in this view. Retrying uses the same suggestion and reviewed content; it may begin work if the server never accepted it.</p><button type="button" disabled={busy} onClick={() => decide(pending.decision, pending)}>Retry same decision</button></>}
      {review && <><p role="status">Review {review.decision === "reject" ? "rejected" : "accepted"}: <code>{review.id}</code></p>{review.reason && <p>{review.reason}</p>}{review.proposal && source && <MetadataProposalReceipt command={review.proposal} source={source} />}{review.proposal && ["unknown", "attempting"].includes(review.proposal.state) && <button type="button" disabled={busy} onClick={() => updateProposal("check")}>Check retained provider state</button>}{review.proposal && canDecide && ["accepted", "prepared"].includes(review.proposal.state) && <button type="button" disabled={busy} onClick={() => updateProposal("continue")}>Continue this retained proposal</button>}</>}
    </section>}
  </details>;
}
function endpointName(endpoint: SuggestionEndpoint): string {
  if (endpoint.ref) return `${endpoint.ref.sourceBindingId} / ${endpoint.ref.sourceId}`;
  return endpoint.creation ? `Unresolved creation: ${endpoint.creation.sourceBindingId}` : "Unresolved endpoint";
}
