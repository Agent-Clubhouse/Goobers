import { useEffect, useRef, useState } from "react";
import type { DaemonClient, InteractiveRunAction, InteractiveRunCommand, InteractiveRunView } from "../api/types";
import { DaemonApiError, DaemonAuthError } from "../api/errors";

/** Deliberately separate from read-only monitoring: permission failures cannot
 * hide the run, and saved guidance is never presented as agent delivery. */
export function RunInterventionPanel({ client, runId, revision }: { client: DaemonClient; runId: string; revision?: string | number }) {
  const [loaded, setLoaded] = useState<{ client: DaemonClient; runId: string; view?: InteractiveRunView; error?: string }>();
  const current = loaded?.client === client && loaded.runId === runId ? loaded : undefined;
  const view = current?.view;
  const unavailable = current?.error ?? "";
  const [refresh, setRefresh] = useState(0);
  useEffect(() => {
    const controller = new AbortController();
    void client.getInteractiveRun(runId, { signal: controller.signal }).then((next) => {
      if (controller.signal.aborted) return;
      if (next.runId !== runId) throw new Error("Run observation identity changed");
      setLoaded({ client, runId, view: next });
    }).catch((error: unknown) => {
      if (controller.signal.aborted) return;
      setLoaded({ client, runId, error: error instanceof DaemonAuthError
        ? "Human operations require sign-in and explicit access to this gaggle. Monitoring remains available."
        : "Human operations are unavailable for this run. Monitoring remains available." });
    });
    return () => controller.abort();
  }, [client, runId, revision, refresh]);
  return <section className="run-human-operations" aria-label="Human operations">
    <h2>Human operations</h2>
    <button type="button" onClick={() => setRefresh((n) => n + 1)}>Refresh human operations</button>
    {unavailable && <p role="status">{unavailable}</p>}
    {!view && !unavailable && <p role="status">Checking run permissions…</p>}
    {view && <>
      <p>Decisions apply to the observed stage occurrence. Saved guidance is shared with authorized gaggle viewers; it does not deliver a message or restart work.</p>
      {view.actions.length === 0 && <p>No approval or escalation action is available at this run position.</p>}
      <div className="run-human-actions">{view.actions.map((action) => <HumanActionForm key={`${action.kind}:${action.stage}:${action.subjectSequence}`} action={action} guidance={view.guidance} client={client} runId={runId} refresh={() => setRefresh((n) => n + 1)} />)}</div>
      <p>{view.restartReason}</p>
      <h3>Saved guidance</h3>
      {view.guidance.length === 0 ? <p>No shared guidance has been saved.</p> : <ol>{view.guidance.map(({ request }) => <li key={request.requestId}>
        <strong>{request.targetAddress}</strong><p>{request.content.text}</p><small>Saved by {request.principalRef} · {new Date(request.requestedAt).toLocaleString()}</small>
      </li>)}</ol>}
    </>}
  </section>;
}

function HumanActionForm({ action, guidance, client, runId, refresh }: { action: InteractiveRunAction; guidance: InteractiveRunView["guidance"]; client: DaemonClient; runId: string; refresh: () => void }) {
  const [guidanceIds, setGuidanceIds] = useState<string[]>([]);
  const [continuation, setContinuation] = useState("");
  const [decision, setDecision] = useState(action.decisions[0] ?? "");
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState("");
  const [pending, setPending] = useState(false);
  const request = useRef<{ key: string; command: InteractiveRunCommand } | undefined>(undefined);
  const lifetime = useRef<AbortController>(undefined);
  const active = useRef(false);
  useEffect(() => {
    const controller = new AbortController(); lifetime.current = controller;
    return () => controller.abort();
  }, [client, runId, action.kind, action.stage, action.subjectSequence]);
  const label = action.kind === "restart" ? "Restart stage" : action.kind === "guidance" ? "Save guidance" : action.kind === "deny" ? "Deny escalation" : action.kind === "override" ? "Override gate" : "Apply gate decision";
  async function submit() {
    const controller = lifetime.current;
    if (!controller || controller.signal.aborted || active.current) return;
    active.current = true;
    setBusy(true); setNotice("");
    if (!request.current) request.current = { key: crypto.randomUUID(), command: {
      kind: action.kind, stage: action.stage, expectedSubjectSequence: action.subjectSequence,
      ...(action.decisions.length > 0 ? { decision } : {}),
      ...(action.kind === "restart" ? { guidanceIds } : {}),
      ...(action.kind === "guidance" ? { guidance: text } : text ? { rationale: text } : {}),
    } };
    try {
      const result = await client.commandInteractiveRun(runId, request.current.key, request.current.command, { signal: controller.signal });
      if (controller.signal.aborted) return;
      if (result.runId !== runId) throw new Error("Command result identity changed");
      if (result.continuationRunId) setContinuation(result.continuationRunId);
      if (result.status === "pending") {
        setPending(true); setNotice(result.accepted && result.continuationRunId
          ? "Restart queued. Check the same request for admission; its execution will be available after it starts."
          : "The outcome is still pending. Check the same request before issuing another command.");
      } else {
        request.current = undefined; setPending(false); setText("");
        setNotice(result.status === "started" ? "Restart accepted as a linked execution. Open it to follow progress." : result.status === "failed" ? "The intervention failed. Review the run journal before issuing a new command." : result.status === "saved" ? "Guidance saved. It has not been delivered to an agent." : "Decision applied to the journal.");
        refresh();
      }
    } catch (error) {
      if (controller.signal.aborted) return;
      // Known refusals permit a new command; transport failures retain the exact
      // occurrence, payload and key because acceptance may already have happened.
      if (error instanceof DaemonAuthError || (error instanceof DaemonApiError && error.status >= 400 && error.status < 500 && error.status !== 429)) {
        request.current = undefined; setPending(false);
        setNotice(error instanceof DaemonApiError && error.status === 409 ? "The stage changed or another intervention is active. Refresh and review before trying again; your text is preserved." : "The command was refused. Review your gaggle permissions and the current stage.");
        refresh();
      } else {
        setPending(true); setNotice("The outcome is unknown. Check the same request; your text and request key are preserved.");
      }
    } finally { if (!controller.signal.aborted) { active.current = false; setBusy(false); } }
  }
  return <form aria-label={`${label}: ${action.stage}`} onSubmit={(event) => { event.preventDefault(); void submit(); }}>
    <h3>{action.stage} · {label}</h3>
    <small>Observed occurrence {action.subjectSequence}</small>
    {action.decisions.length > 0 && <label>Decision<select value={decision} disabled={busy || pending || !action.available} onChange={(event) => setDecision(event.target.value)}>{action.decisions.map((value) => <option key={value}>{value}</option>)}</select></label>}
    {action.kind === "restart" && <fieldset disabled={busy || pending || !action.available}><legend>Saved guidance to deliver (up to 16)</legend>{guidance.length === 0 ? <p>Save guidance before restarting.</p> : guidance.map(({ request }) => <label key={request.requestId}><input type="checkbox" checked={guidanceIds.includes(request.requestId)} disabled={!guidanceIds.includes(request.requestId) && guidanceIds.length >= 16} onChange={(event) => setGuidanceIds((ids) => event.target.checked ? [...ids, request.requestId] : ids.filter((id) => id !== request.requestId))} />{request.content.text} · {request.principalRef}</label>)}</fieldset>}
    <label>{action.kind === "guidance" ? "Guidance" : "Rationale"}<textarea value={text} maxLength={action.kind === "guidance" ? 65536 : 4096} required={action.kind !== "approve"} disabled={busy || pending || !action.available} onChange={(event) => setText(event.target.value)} /></label>
    <button type="submit" disabled={busy || !action.available || (action.kind === "restart" && guidanceIds.length === 0)}>{busy ? "Submitting…" : pending ? "Check same request" : label}</button>
    {!action.available && <p>{action.reason}</p>}
    {notice && <p role="status">{notice}</p>}
    {continuation && (pending ? <p>Queued execution: <code>{continuation}</code></p> : <a href={`#/run/${encodeURIComponent(continuation)}`}>Open restarted execution</a>)}
  </form>;
}
