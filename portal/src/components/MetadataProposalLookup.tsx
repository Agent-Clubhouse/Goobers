import { useEffect, useRef, useState } from "react";
import { DaemonAuthError } from "../api/errors";
import type { DaemonClient, MetadataProposalCommand, SourceView } from "../api/types";
import { MetadataProposalReceipt } from "./MetadataProposalReceipt";

/** Reads exact retained custody; a browser refresh never creates a replacement proposal. */
export function MetadataProposalLookup({ client, gaggle, source, path }: { client: DaemonClient; gaggle: string; source: SourceView; path: string }) {
  const [id, setId] = useState("");
  const [command, setCommand] = useState<MetadataProposalCommand>();
  const [canCheck, setCanCheck] = useState(false);
  const [canContinue, setCanContinue] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const lifetime = useRef<AbortController>(undefined);
  const active = useRef(false);
  useEffect(() => { const controller = new AbortController(); lifetime.current = controller; return () => controller.abort(); }, []);
  async function load(kind: "read" | "check" | "continue") {
    const controller = lifetime.current;
    if (!controller || controller.signal.aborted || active.current || !/^workbench-[a-f0-9]{32}$/.test(id)) return;
    active.current = true; setBusy(true); setError("");
    try {
      const options = { signal: controller.signal };
      let result: MetadataProposalCommand;
      if (kind === "check") result = await client.checkMetadataProposal(gaggle, source.bindingId, id, options);
      else if (kind === "continue") result = await client.continueMetadataProposal(gaggle, source.bindingId, id, options);
      else result = await client.getMetadataProposal(gaggle, source.bindingId, id, options);
      if (controller.signal.aborted) return;
      if (result.id !== id || result.gaggle !== gaggle || result.sourceBindingId !== source.bindingId || result.path !== path) throw new Error("Retained source identity changed");
      setCommand(result);
      const policy = await client.getInteractiveCapabilities(gaggle, options);
      if (controller.signal.aborted) return;
      setCanCheck(policy.gaggle === gaggle && policy.operator);
      setCanContinue(policy.gaggle === gaggle && policy.operator && policy.sourceWriteMode === "pull-request" && policy.actions.some((action) => action.action === "source.proposeChange" && action.authorized && action.credentialConfigured && action.available));
    } catch (cause) {
      if (controller.signal.aborted) return;
      setCanCheck(false); setCanContinue(false); setCommand(undefined);
      setError(cause instanceof DaemonAuthError ? "This proposal is unavailable for your current access." : "The exact proposal receipt is unavailable. Keep the command ID; no replacement proposal was sent.");
    } finally { if (!controller.signal.aborted) { active.current = false; setBusy(false); } }
  }
  return <details className="workbench-command"><summary>Find a retained proposal</summary><p>Enter a prior command ID for this file. Receipts remain scoped to the human who initiated them.</p>
    <form onSubmit={(event) => { event.preventDefault(); void load("read"); }}><label>Proposal command ID<input value={id} required maxLength={42} pattern="workbench-[a-f0-9]{32}" disabled={busy} onChange={(event) => { setId(event.target.value); setCommand(undefined); setCanCheck(false); setCanContinue(false); setError(""); }} /></label><button type="submit" disabled={busy}>Load retained proposal</button></form>
    {error && <p role="alert">{error}</p>}
    {command && <MetadataProposalReceipt command={command} source={source} />}
    {command && canCheck && ["unknown", "attempting"].includes(command.state) && <button type="button" disabled={busy} onClick={() => void load("check")}>Check retained provider state</button>}
    {command && canContinue && ["accepted", "prepared"].includes(command.state) && <button type="button" disabled={busy} onClick={() => void load("continue")}>Continue this retained proposal</button>}
  </details>;
}
