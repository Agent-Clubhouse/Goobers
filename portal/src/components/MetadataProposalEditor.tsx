import { useEffect, useRef, useState } from "react";
import { DaemonApiError, DaemonAuthError } from "../api/errors";
import type { DaemonClient, MetadataChangeRequest, MetadataPreview, MetadataProposalCommand, SourceView, WorkbenchDocumentFile } from "../api/types";
import { MetadataProposalLookup } from "./MetadataProposalLookup";
import { MetadataProposalReceipt } from "./MetadataProposalReceipt";
import { MetadataRelationshipInput } from "./MetadataRelationshipInput";
import { editableEdges, metadataFieldText, metadataModes, refText, sameMetadataCommand, sameMetadataPreview, type EditableEdge, type MetadataEditMode } from "./workbenchMetadataEditing";

interface EditorProps { client: DaemonClient; gaggle: string; source: SourceView; sources: SourceView[]; file: WorkbenchDocumentFile }
interface ReviewedChange { request: MetadataChangeRequest; preview: MetadataPreview }
interface PendingProposal extends ReviewedChange { key: string }
const labels: Record<MetadataEditMode, string> = { title: "Objective title", description: "Document body", "relationship-add": "Add relationship", "relationship-remove": "Remove relationship" };

export function MetadataProposalEditor(props: EditorProps) {
  const identity = JSON.stringify([props.gaggle, props.source, props.sources, props.file.path, props.file.provenance]);
  const [scope, setScope] = useState({ client: props.client, identity, version: 0 });
  if (scope.client !== props.client || scope.identity !== identity) {
    setScope({ client: props.client, identity, version: scope.version + 1 });
    return <p role="status">Checking proposal access…</p>;
  }
  if (props.file.status !== "available" || !props.file.provenance) return null;
  return <div key={scope.version}>{metadataModes(props.source, props.file).length > 0 && <ScopedMetadataEditor {...props} />}<MetadataProposalLookup client={props.client} gaggle={props.gaggle} source={props.source} path={props.file.path} /></div>;
}
function ScopedMetadataEditor({ client, gaggle, source, sources, file }: EditorProps) {
  const modes = metadataModes(source, file);
  const [mode, setMode] = useState(modes[0]);
  const [text, setText] = useState(metadataFieldText(file, modes[0]));
  const [edge, setEdge] = useState<EditableEdge>(() => ({ edgeId: `edge-${crypto.randomUUID()}`, kind: source.writeRelationships?.includes("references") ? "references" : "contributes-to", from: file.ref ?? { gaggleId: gaggle, sourceBindingId: "", kind: "work-item", sourceId: "" }, to: { gaggleId: gaggle, sourceBindingId: "", kind: "objective-document", sourceId: "" } }));
  const edges = editableEdges(source, file);
  const [removeId, setRemoveId] = useState(edges[0]?.edgeId ?? "");
  const [allowed, setAllowed] = useState<boolean>();
  const [reviewed, setReviewed] = useState<ReviewedChange>();
  const [pending, setPending] = useState<PendingProposal>();
  const [command, setCommand] = useState<MetadataProposalCommand>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const lifetime = useRef<AbortController>(undefined);
  const active = useRef(false);
  useEffect(() => {
    const controller = new AbortController(); lifetime.current = controller;
    void client.getInteractiveCapabilities(gaggle, { signal: controller.signal }).then((value) => {
      if (!controller.signal.aborted) setAllowed(value.gaggle === gaggle && value.operator && value.sourceWriteMode === "pull-request" && value.actions.some((action) => action.action === "source.proposeChange" && action.authorized && action.credentialConfigured && action.available));
    }).catch(() => { if (!controller.signal.aborted) setAllowed(false); });
    return () => controller.abort();
  }, [client, gaggle]);

  function changed() { setReviewed(undefined); setError(""); }
  function deny() { setAllowed(false); setReviewed(undefined); setPending(undefined); setCommand(undefined); setText(""); setError(""); }
  function request(): MetadataChangeRequest {
    const expected = file.provenance!;
    const base = { path: file.path, expected: { commit: expected.commit, blobId: expected.blobId, contentDigest: expected.contentDigest } };
    if (mode === "title" || mode === "description") return { ...base, field: mode, value: text };
    const selected = mode === "relationship-remove" ? edges.find((item) => item.edgeId === removeId) : edge;
    if (!selected) throw new Error("Select an observed relationship");
    return { ...base, relationship: { action: mode === "relationship-add" ? "add" : "remove", edge: selected } };
  }
  async function action(kind: "preview" | "submit" | "receipt" | "check" | "continue", proposal?: PendingProposal) {
    const controller = lifetime.current;
    if (!controller || controller.signal.aborted || active.current) return;
    active.current = true; setBusy(true); setError("");
    try {
      if (kind === "preview") {
        setReviewed(undefined);
        const input = request();
        const preview = await client.previewMetadataChange(gaggle, source.bindingId, input, { signal: controller.signal });
        if (controller.signal.aborted) return;
        if (!sameMetadataPreview(preview, input)) throw new Error("Preview identity changed");
        setReviewed({ request: input, preview }); return;
      }
      const retained = proposal ?? pending;
      if (!retained || (kind !== "submit" && !command)) return;
      if (kind === "submit") setPending(retained);
      const options = { signal: controller.signal };
      let value: MetadataProposalCommand;
      switch (kind) {
        case "submit": value = await client.submitMetadataProposal(gaggle, source.bindingId, retained.key, retained.request, options); break;
        case "check": value = await client.checkMetadataProposal(gaggle, source.bindingId, command!.id, options); break;
        case "continue": value = await client.continueMetadataProposal(gaggle, source.bindingId, command!.id, options); break;
        default: value = await client.getMetadataProposal(gaggle, source.bindingId, command!.id, options);
      }
      if (controller.signal.aborted) return;
      if (!sameMetadataCommand(value, gaggle, source.bindingId, retained.preview) || (command && command.id !== value.id)) throw new Error("Command identity changed");
      setCommand(value);
    } catch (cause) {
      if (controller.signal.aborted) return;
      if (cause instanceof DaemonAuthError) { deny(); return; }
      if (kind === "preview") setError(cause instanceof DaemonApiError && cause.status === 409 ? "The source or editing policy changed. Refresh documents before preparing another preview." : "The requested preview could not be verified. Check the declared field, relationship and source revision.");
      else if (kind === "submit" && cause instanceof DaemonApiError && cause.code === "invalid_request") { setPending(undefined); setError("The request was rejected before a proposal. Review the edit and try again."); }
      else setError("No reliable receipt was received. Keep this request identity and check the same command. No new proposal should be started.");
    } finally { if (!controller.signal.aborted) { active.current = false; setBusy(false); } }
  }

  if (allowed === undefined) return <p role="status">Checking proposal access…</p>;
  if (!allowed) return <p>Repository proposals are unavailable for your current access.</p>;
  return <section className="workbench-editor" aria-label="Propose repository change">
    <h4>Propose a source change</h4><p>Review the exact source preview, then create a draft PR using the configured interactive identity.</p>
    <form onSubmit={(event) => { event.preventDefault(); if (!pending) void action("preview"); }}>
      <fieldset disabled={busy || !!pending}>
        <label>Change<select value={mode} onChange={(event) => { const chosen = event.target.value as MetadataEditMode; setMode(chosen); setText(metadataFieldText(file, chosen)); changed(); }}>{modes.map((value) => <option key={value} value={value}>{labels[value]}</option>)}</select></label>
        {(mode === "title" || mode === "description") && <label>Proposed {mode === "title" ? "title" : "body"}<textarea value={text} required={mode === "title"} maxLength={mode === "title" ? 512 : 1024 * 1024} rows={mode === "title" ? 2 : 8} onChange={(event) => { setText(event.target.value); changed(); }} /></label>}
        {mode === "relationship-add" && <MetadataRelationshipInput gaggle={gaggle} source={source} sources={sources} file={file} edge={edge} change={(value) => { setEdge(value); changed(); }} />}
        {mode === "relationship-remove" && <label>Observed relationship<select value={removeId} onChange={(event) => { setRemoveId(event.target.value); changed(); }}>{edges.map((item) => <option key={item.edgeId} value={item.edgeId}>{item.kind}: {refText(item.from)} → {refText(item.to)} · {item.edgeId}</option>)}</select></label>}
        <p>Expected branch commit: <code>{file.provenance!.commit}</code></p><button type="submit">Preview source change</button>
      </fieldset>
    </form>
    {reviewed && <section aria-label="Source change preview"><h5>Review exact source</h5><details><summary>Before</summary><pre className="workbench-source-text">{reviewed.preview.before}</pre></details><details open><summary>Proposed source</summary><pre className="workbench-source-text">{reviewed.preview.after}</pre></details>{!reviewed.preview.changed && <p>This edit would not change the source.</p>}{!pending && <button type="button" disabled={busy || !reviewed.preview.changed} onClick={() => void action("submit", { ...reviewed, key: crypto.randomUUID() })}>Create draft PR</button>}</section>}
    {error && <p role="alert">{error}</p>}
    {pending && <p>Request key: <code>{pending.key}</code></p>}
    {command && <MetadataProposalReceipt command={command} source={source} />}
    {pending && !command && <button type="button" disabled={busy} onClick={() => void action("submit", pending)}>Check same command</button>}
    {command && <button type="button" disabled={busy} onClick={() => void action("receipt")}>Refresh proposal receipt</button>}
    {command && ["unknown", "attempting", "blocked"].includes(command.state) && <button type="button" disabled={busy} onClick={() => void action("check")}>Check provider state</button>}
    {command && ["accepted", "prepared"].includes(command.state) && <button type="button" disabled={busy} onClick={() => void action("continue")}>Continue retained proposal</button>}
    {busy && <p role="status">Waiting for the proposal service…</p>}
  </section>;
}
