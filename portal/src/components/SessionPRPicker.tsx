import { useEffect, useRef, useState } from "react";
import type { DaemonClient, SessionPRRepairInspection, SourceView } from "../api/types";

export function SessionPRPicker({ client, gaggle, disabled, value, onChange }: {
  client: DaemonClient; gaggle: string; disabled: boolean;
  value?: SessionPRRepairInspection; onChange: (value: SessionPRRepairInspection | undefined) => void;
}) {
  const [sources, setSources] = useState<SourceView[]>([]);
  const [source, setSource] = useState("");
  const [number, setNumber] = useState("");
  const [preview, setPreview] = useState<SessionPRRepairInspection>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const active = useRef<AbortController | undefined>(undefined);
  useEffect(() => {
    const controller = new AbortController();
    void client.listWorkbenchSources(gaggle, { signal: controller.signal })
      .then((page) => { if (!controller.signal.aborted) setSources(page.items.filter((item) => item.kind !== "backlog" && item.repository)); })
      .catch(() => { if (!controller.signal.aborted) setError("Configured repositories are unavailable. Refresh the conversation to check your access."); });
    return () => { controller.abort(); active.current?.abort(); };
  }, [client, gaggle]);
  function clear() { active.current?.abort(); setBusy(false); setPreview(undefined); onChange(undefined); setError(""); }
  async function inspect() {
    if (busy || disabled || !source || !/^[1-9][0-9]{0,18}$/.test(number)) return;
    active.current?.abort();
    const controller = new AbortController(); active.current = controller;
    setBusy(true); setPreview(undefined); onChange(undefined); setError("");
    try {
      const result = await client.inspectPullRequest(gaggle, source, number, { signal: controller.signal });
      const selected = sources.find((item) => item.bindingId === source);
      const repo = result.target.repository;
      if (!selected || result.target.sourceBindingId !== source || result.target.id !== number || repo.provider !== selected.provider || repo.owner !== selected.owner || repo.name !== selected.repository || (repo.project ?? "") !== (selected.project ?? "") || result.headSha !== result.target.expectedHeadSha) throw new Error("Changed source");
      if (!controller.signal.aborted) setPreview(result);
    } catch {
      if (!controller.signal.aborted) setError("This PR could not be inspected in the selected repository. It may be unavailable or from a fork.");
    } finally { if (!controller.signal.aborted) setBusy(false); }
  }
  return <fieldset disabled={disabled} className="session-pr-picker">
    <legend>Repair a pull request</legend>
    <p>Choose a PR for this message. The agent checks its head and permissions again before changing files.</p>
    <label>Repository source<select value={source} disabled={busy} onChange={(event) => { clear(); setSource(event.target.value); }}>
      <option value="">Choose a configured repository</option>
      {sources.map((item) => <option key={item.bindingId} value={item.bindingId}>{item.owner}/{item.project ? `${item.project}/` : ""}{item.repository} · {item.bindingId}</option>)}
    </select></label>
    <label>PR number<input inputMode="numeric" maxLength={19} value={number} disabled={busy} onChange={(event) => { clear(); setNumber(event.target.value); }} /></label>
    <button type="button" disabled={busy || !source || !/^[1-9][0-9]{0,18}$/.test(number)} onClick={() => { void inspect(); }}>{busy ? "Inspecting PR…" : "Inspect PR"}</button>
    {preview && <section aria-label="PR inspection">
      <h4>PR #{preview.target.id}: {preview.title}</h4>
      <p>{preview.open ? preview.draft ? "Open draft" : "Open" : "Closed"} · {preview.head} → {preview.base}</p>
      <p>Inspected head: <code>{preview.headSha}</code></p>
      <button type="button" disabled={!preview.open || value !== undefined} onClick={() => onChange(preview)}>Use this PR for the message</button>
    </section>}
    {value && <p role="status">Selected PR #{value.target.id} at <code>{value.headSha.slice(0, 12)}</code>. <button type="button" onClick={() => onChange(undefined)}>Remove PR selection</button></p>}
    {error && <p role="status">{error}</p>}
  </fieldset>;
}
