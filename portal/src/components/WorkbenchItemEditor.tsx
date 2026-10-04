import { useEffect, useRef, useState } from "react";
import { DaemonApiError, DaemonAuthError } from "../api/errors";
import type { BacklogEditCommand, BacklogItem, BacklogPatchInput, BacklogWriteCapabilities, BacklogWriteField, DaemonClient } from "../api/types";
import { WorkbenchCommandReceipt } from "./WorkbenchCommandReceipt";
import { controlLabel, editFields, editInput, fieldText, sameCommand, sameItem } from "./workbenchEditing";

interface PendingEdit { key: string; input: BacklogPatchInput }
export function WorkbenchItemEditor({ client, item, refreshed }: { client: DaemonClient; item: BacklogItem; refreshed: (item: BacklogItem) => void }) {
  const [capabilities, setCapabilities] = useState<BacklogWriteCapabilities>();
  const [basis, setBasis] = useState(item);
  const [field, setField] = useState<BacklogWriteField>("title");
  const [text, setText] = useState(item.title);
  const [pending, setPending] = useState<PendingEdit>();
  const [command, setCommand] = useState<BacklogEditCommand>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [denied, setDenied] = useState(false);
  const lifetime = useRef<AbortController>(undefined);
  const active = useRef(false);
  const lastItem = useRef(item);
  const { gaggleId: gaggle, sourceBindingId: source, sourceId } = item.ref;
  useEffect(() => {
    const controller = new AbortController(); lifetime.current = controller; active.current = false;
    setBasis(item); setCapabilities(undefined); setPending(undefined); setCommand(undefined); setError(""); setDenied(false); setBusy(false);
    void client.getWorkbenchWriteCapabilities(gaggle, source, { signal: controller.signal }).then((value) => {
      if (controller.signal.aborted) return;
      setCapabilities(value);
      const first = editFields.find((candidate) => value.fields.includes(candidate));
      if (first) { setField(first); setText(fieldText(item, first)); }
    }).catch(() => { if (!controller.signal.aborted) setDenied(true); });
    return () => controller.abort();
  }, [client, gaggle, source, sourceId]);
  useEffect(() => { if (lastItem.current === item) return; lastItem.current = item; if (!pending) { setBasis(item); setText(fieldText(item, field)); } }, [item, field, pending]);

  function rejectAccess() { setCapabilities(undefined); setCommand(undefined); setPending(undefined); setText(""); setError(""); setDenied(true); }
  function record(result: BacklogEditCommand, edit: PendingEdit) {
    if (!sameCommand(result, basis, edit.input)) throw new Error("Command identity changed");
    setCommand(result); setError("");
  }
  async function send(edit: PendingEdit) {
    const controller = lifetime.current; if (!controller || controller.signal.aborted || active.current) return;
    active.current = true; setPending(edit); setBusy(true); setError("");
    try {
      const result = await client.patchWorkbenchItem(gaggle, source, basis.locator.id, edit.key, edit.input, { signal: controller.signal });
      if (!controller.signal.aborted) record(result, edit);
    } catch (cause) {
      if (controller.signal.aborted) return;
      if (cause instanceof DaemonAuthError) { rejectAccess(); return; }
      if (cause instanceof DaemonApiError && cause.code === "invalid_request") {
        setPending(undefined); setError("The request was rejected before an edit. Check the field value and try again.");
      } else setError("No reliable reply was received. Keep this request key and check the same command; do not submit a new edit.");
    } finally { if (!controller.signal.aborted) { active.current = false; setBusy(false); } }
  }
  async function checkReceipt() {
    const controller = lifetime.current; if (!controller || controller.signal.aborted || active.current || !command || !pending) return;
    active.current = true; setBusy(true); setError("");
    try {
      const result = await client.getWorkbenchCommand(gaggle, source, command.id, { signal: controller.signal });
      if (!controller.signal.aborted) record(result, pending);
    } catch (cause) {
      if (controller.signal.aborted) return;
      if (cause instanceof DaemonAuthError) { rejectAccess(); return; }
      setError(cause instanceof DaemonApiError && cause.status === 410 ? "The full receipt has expired. This request key remains reserved during its replay window." : "The receipt is unavailable. Keep the command identity and inspect the source; no new write was sent.");
    } finally { if (!controller.signal.aborted) { active.current = false; setBusy(false); } }
  }
  async function startFresh() {
    const controller = lifetime.current; if (!controller || controller.signal.aborted || active.current) return;
    active.current = true; setBusy(true); setError("");
    try {
      const [latest, policy] = await Promise.all([client.getWorkbenchItem(gaggle, source, { id: basis.locator.id, expectedSourceId: sourceId }, { signal: controller.signal }), client.getWorkbenchWriteCapabilities(gaggle, source, { signal: controller.signal })]);
      if (controller.signal.aborted) return;
      if (!sameItem(latest, basis) || !latest.revision) throw new Error("Source changed");
      const first = editFields.find((candidate) => policy.fields.includes(candidate));
      if (!first) { rejectAccess(); return; }
      setCapabilities(policy); setBasis(latest); setField(first); setText(fieldText(latest, first)); setPending(undefined); setCommand(undefined); refreshed(latest);
    } catch (cause) {
      if (controller.signal.aborted) return;
      if (cause instanceof DaemonAuthError) { rejectAccess(); return; }
      setError("The current item and editing access could not be verified. No new edit is available.");
    } finally { if (!controller.signal.aborted) { active.current = false; setBusy(false); } }
  }

  if (denied || (capabilities && capabilities.fields.length === 0)) return <p>Native editing is unavailable for this source and your current access.</p>;
  if (!capabilities) return <p role="status">Checking editing access…</p>;
  if (!basis.revision) return <p>A native source revision is required before editing. Refresh the item.</p>;
  const choices = editFields.filter((candidate) => capabilities.fields.includes(candidate));
  const scalarLimit = field === "title" ? 4096 : field === "state" ? 128 : 192 * 1024;
  const isSet = field === "labels" || field === "assignees";
  const settled = command?.state === "confirmed" || command?.state === "not-applied";
  return <section className="workbench-editor" aria-label="Edit backlog item">
    <h4>Edit native backlog fields</h4>
    <p>One field per command using the configured interactive identity. Your human identity is recorded in the command receipt.</p>
    <p>{capabilities.revisionSemantics === "atomic-revision-test" ? "Azure DevOps checks the expected revision atomically when applying the edit." : "GitHub is checked before the edit; its issue API does not provide an atomic revision condition."}</p>
    <form onSubmit={(event) => { event.preventDefault(); if (!pending) void send({ key: crypto.randomUUID(), input: editInput(basis, field, text) }); }}>
      <fieldset disabled={busy || !!pending}>
        <label>Field<select value={field} onChange={(event) => { const chosen = event.target.value as BacklogWriteField; setField(chosen); setText(fieldText(basis, chosen)); }}>{choices.map((value) => <option key={value} value={value}>{value}</option>)}</select></label>
        <label>{isSet ? "Values (one per line)" : "New value"}<textarea value={text} onChange={(event) => setText(event.target.value)} maxLength={isSet ? 128 * 401 : scalarLimit} required={!isSet && field !== "description"} rows={field === "description" ? 8 : 3} /></label>
        {field === "labels" && <p>Existing Goobers control labels are preserved: {(basis.labels ?? []).filter(controlLabel).join(", ") || "none"}. Escalation resolution uses a separate operation.</p>}
        {field === "assignees" && <p>Enter native account identities. Maximum {capabilities.maxAssignees}; an empty list clears assignments.</p>}
        <p>Expected source revision: <code>{basis.revision}</code></p>
        <button type="submit">Apply field change</button>
      </fieldset>
    </form>
    {error && <p role="alert">{error}</p>}
    {pending && <p>Request key: <code>{pending.key}</code></p>}
    {command && <WorkbenchCommandReceipt command={command} />}
    {pending && !command && <button type="button" disabled={busy} onClick={() => void send(pending)}>Check same command</button>}
    {command && <button type="button" disabled={busy} onClick={() => void checkReceipt()}>Check receipt</button>}
    {command?.state === "accepted" && pending && <button type="button" disabled={busy} onClick={() => void send(pending)}>Continue retained command</button>}
    {settled && <button type="button" disabled={busy} onClick={() => void startFresh()}>Refresh for another edit</button>}
    {busy && <p role="status">Waiting for the command service…</p>}
  </section>;
}
