import { humanControlHeaders } from "./humanAuth";
import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { Action } from "../ui/Action";
import { Icon } from "../ui/Icon";
import { Timestamp } from "../ui/Timestamp";
import { commandId, describeError, InteractiveError, isUncertain, readPending, writePending, type Surface } from "./client";

interface Definition { metadata: { name: string }; spec: { displayName?: string; enabled?: boolean; [key: string]: unknown } }
interface Run { id: string; workflow: string; gaggle: string; phase: string; reason?: string; journalPath?: string; startedAt: string }
interface Proposal { kind: string; idempotencyKey: string; gaggle?: string; workflow?: string; displayName?: string; enabled?: boolean; workflowYaml?: string; runId?: string; message?: string }
interface Operation {
  id: string; operationId: string; kind: string; status: string; operationStatus: string;
  summary: string; preview: string; configDigest: string; payloadDigest: string;
  createdAt: string; updatedAt: string; runId?: string; output?: string; error?: string;
  stopRequested?: boolean; request: Proposal;
}
interface OperationsState {
  mode: string; actorId: string; sessionId: string; instanceRoot: string; repository: string;
  configDigest: string; gaggles: Definition[]; workflows: Definition[]; runs: Run[];
  actions: Operation[]; backlog: {id: string; title: string; state: string; source: string}[];
  workflowDraft: string; capabilities: string[]; limitations: string[];
}
interface Pending { path: string; body: unknown }

export function OperationsPanel({ sessionId, disabled, executionMode, surface, policyRetired }: { sessionId: string; disabled: boolean; executionMode?: string; surface?: Surface; policyRetired?: boolean }) {
  const canRead = !surface?.configRevision || !!surface.tools?.some(tool => tool === "session_context" || tool === "inspect_action");
  const canPropose = !policyRetired && (!surface?.configRevision || !!surface.tools?.includes("propose_action"));
  const [state, setState] = useState<OperationsState>();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [readBusy, setReadBusy] = useState(false);
  const [kind, setKind] = useState("submit_run");
  const [workflow, setWorkflow] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [enabled, setEnabled] = useState(true);
  const [yaml, setYaml] = useState("");
  const [chat, setChat] = useState("");
  const [pending, setPending] = useState<Pending | undefined>(() => readPending<Pending>(`ops.${sessionId}`));
  const [announcement, setAnnouncement] = useState("");
  const lock = useRef(false);
  const draftInitialized = useRef(false);
  const base = `/api/v1/interactive/operations`;
  const query = `?sessionId=${encodeURIComponent(sessionId)}`;
  const request = useCallback(async <T,>(path: string, body?: unknown): Promise<T> => {
    let response: Response;
    try { response = await fetch(base + path + query, { method: body === undefined ? "GET" : "POST", credentials: "same-origin", signal: typeof AbortSignal.timeout === "function" ? AbortSignal.timeout(30000) : undefined, headers: { Accept: "application/json", ...humanControlHeaders(), ...(body === undefined ? {} : { "Content-Type": "application/json" }) }, ...(body === undefined ? {} : { body: JSON.stringify(body) }) }); }
    catch { throw new InteractiveError("The response was lost. The action outcome is unknown. Refresh its receipt before retrying.", "network", 0); }
    const payload = await response.json().catch(() => undefined);
    if (!response.ok) throw new InteractiveError(typeof payload?.error === "string" ? payload.error : payload?.error?.message ?? `Operation returned HTTP ${response.status}.`, "operation", response.status);
    if (payload === undefined) throw new InteractiveError("The operation returned an unreadable response. Its outcome is unknown.", "invalid_response", 0);
    return payload as T;
  }, [base, query]);
  const refresh = useCallback(async () => {
    if (!canRead) { setState(undefined); return; }
    try {
      const result = await request<OperationsState>("/state");
      setState(result); setReadBusy(false);
      if (!draftInitialized.current) { draftInitialized.current = true; setYaml(result.workflowDraft); setWorkflow(result.workflows?.[0]?.metadata.name ?? ""); setDisplayName(result.gaggles?.[0]?.spec.displayName ?? ""); }
    } catch (cause) {
      if (cause instanceof InteractiveError && cause.status === 409 && /^helper busy:/i.test(cause.message)) setReadBusy(true);
      else { setReadBusy(false); setError(describeError(cause)); }
    }
  }, [request, canRead]);
  useEffect(() => { void refresh(); const interval = setInterval(() => void refresh(), 2000); return () => clearInterval(interval); }, [refresh]);
  async function mutate(path: string, body: unknown) {
    if (lock.current || disabled || !canPropose) return;
    lock.current = true; setBusy(true); setError(undefined); setAnnouncement("");
    writePending(`ops.${sessionId}`, { path, body });
    try {
      const operation = await request<Operation>(path, body);
      writePending(`ops.${sessionId}`, undefined); setPending(undefined);
      setAnnouncement(`${operation.summary}: ${operation.status.replace(/_/g, " ")}.`);
      await refresh();
    } catch (cause) {
      setError(describeError(cause));
      if (isUncertain(cause)) setPending({ path, body });
      else { setPending(undefined); writePending(`ops.${sessionId}`, undefined); }
      await refresh();
    } finally { lock.current = false; setBusy(false); }
  }
  function propose(event: FormEvent) {
    event.preventDefault();
    if (blocked) return;
    const input: Proposal = { kind, idempotencyKey: commandId(), gaggle: state?.gaggles?.[0]?.metadata.name ?? "interactive" };
    if (kind === "submit_run" || kind === "tweak_workflow") input.workflow = workflow;
    if (kind === "tweak_gaggle") { input.displayName = displayName; input.enabled = enabled; }
    if (kind === "tweak_workflow") input.enabled = enabled;
    if (kind === "add_workflow") input.workflowYaml = yaml;
    void mutate("/proposals", input);
  }
  const blocked = disabled || busy || !!pending || !canPropose;
  return <>
    <div className="ix-inspector-heading"><Icon name="workflow" /><h3>Gaggle controls</h3></div>
    <p>{executionMode === "codex-local" ? "Agent-proposed workflow and management actions appear here. Inspect the exact payload and apply it yourself; the model cannot approve or run its proposal." : "Inspect the demo gaggle, preview a configuration change, or admit an actual workflow run. These controls are deterministic."}</p>
    {!canRead && <p role="status" className="ix-mode-blocked">Workflow and management context is not enabled for this surface. Its configuration must allow session_context or inspect_action.</p>}
    {canRead && !canPropose && <p role="status" className="ix-mode-blocked">Workflow actions are read-only for this session. {policyRetired ? "The bound configuration is retired; create a new session to use the current policy." : "The configured Goober does not have the propose_action tool."}</p>}
    {canRead && !state && !error && <p role="status">Loading authoritative gaggle state…</p>}
    {error && <div className="ix-action-error" role="alert">{error}</div>}
    {pending && <div className="ix-banner ix-banner-warning"><div><strong>Operation outcome unknown</strong><p>The original request is retained across reloads. Retry reconciles the same intent.</p><Action disabled={busy || disabled || !canPropose} onClick={() => void mutate(pending.path, pending.body)}>Retry same operation</Action><Action onClick={() => void refresh()}>Refresh operation state</Action></div></div>}
    {readBusy && <p role="status" className="ix-empty-small">Configuration update in progress. Showing the last confirmed state; refreshing automatically.</p>}
    {announcement && <p className="ix-operation-receipt" role="status">{announcement}</p>}
    {state && <>
      <div className="ix-gaggle-scope"><strong>{state.gaggles?.[0]?.spec.displayName || state.gaggles?.[0]?.metadata.name || "Local gaggle"}</strong><span>{state.workflows?.length ?? 0} workflows · {state.runs?.length ?? 0} runs</span><small>Scope: {state.sessionId}</small></div>
      {canPropose && executionMode !== "codex-local" && <form className="ix-ops-chat" onSubmit={event => { event.preventDefault(); void mutate("/chat", { text: chat, idempotencyKey: commandId() }); }}><label htmlFor="ops-chat">Describe a gaggle action</label><textarea id="ops-chat" rows={2} placeholder="Design workflow, run workflow, rename gaggle…" value={chat} onChange={event => setChat(event.target.value)} disabled={blocked} /><Action variant="secondary" type="submit" disabled={blocked || !chat.trim()}>Preview request</Action><small>Bounded local intent parser. Review the exact preview before applying.</small></form>}
      <details className="ix-operation-form" open={canPropose}><summary>Configure an action yourself</summary><form onSubmit={propose}><label htmlFor="ops-kind">Action</label><select id="ops-kind" value={kind} onChange={event => setKind(event.target.value)} disabled={blocked}><option value="submit_run">Submit workflow run</option><option value="add_workflow">Add workflow</option><option value="tweak_workflow">Enable or disable workflow</option><option value="tweak_gaggle">Tweak gaggle</option></select>{(kind === "submit_run" || kind === "tweak_workflow") && <><label htmlFor="ops-workflow">Workflow</label><select id="ops-workflow" value={workflow} onChange={event => setWorkflow(event.target.value)} disabled={blocked}>{state.workflows?.map(item => <option key={item.metadata.name} value={item.metadata.name}>{item.metadata.name}</option>)}</select></>}{kind === "tweak_gaggle" && <><label htmlFor="ops-display-name">Gaggle display name</label><input id="ops-display-name" value={displayName} onChange={event => setDisplayName(event.target.value)} disabled={blocked} /></>}{(kind === "tweak_gaggle" || kind === "tweak_workflow") && <label className="ix-check-label"><input type="checkbox" checked={enabled} onChange={event => setEnabled(event.target.checked)} disabled={blocked} />Enabled</label>}{kind === "add_workflow" && <><label htmlFor="ops-yaml">Workflow YAML</label><textarea id="ops-yaml" className="ix-yaml" rows={13} value={yaml} onChange={event => setYaml(event.target.value)} disabled={blocked} spellCheck={false} /></>}<Action variant="secondary" type="submit" disabled={blocked || ((kind === "submit_run" || kind === "tweak_workflow") && !workflow)}>Preview action</Action></form></details>
      <section className="ix-operations" aria-label="Action proposals"><h4>Proposals & receipts</h4>{!state.actions?.length && <p className="ix-empty-small">Proposals appear here before anything is applied.</p>}{[...(state.actions ?? [])].reverse().map(action => <article className="ix-operation" key={action.id}><div><span className={`ix-status ix-status-${action.status}`}>{action.status.replace(/_/g, " ")}</span><Timestamp value={action.updatedAt} /></div><h4>{action.summary}</h4><details open={action.status === "proposed"}><summary>Exact action preview</summary><pre>{typeof action.preview === "string" ? action.preview : JSON.stringify(action.preview, null, 2)}</pre></details>{action.status === "proposed" && <Action variant="primary" disabled={blocked} onClick={() => void mutate(`/actions/${encodeURIComponent(action.id)}/apply`, {})}>{action.kind === "submit_run" ? "Start run" : "Apply action"}</Action>}{action.error && <p role="alert" className="ix-turn-error">{action.error}</p>}{action.output && <details open={action.status === "succeeded" || action.status === "failed"}><summary>Execution output</summary><pre>{action.output}</pre></details>}{action.runId && <p className="ix-run-reference">Run <code>{action.runId}</code> · {action.operationStatus}</p>}<small className="ix-operation-id">Receipt {action.operationId || action.id}</small></article>)}</section>
      <section className="ix-operations" aria-label="Observed runs"><h4>Observed runs</h4>{!(state.runs?.length) && <p className="ix-empty-small">No workflow has run in this local scope.</p>}{state.runs?.map(run => <article className="ix-observed-run" key={run.id}><strong>{run.workflow}</strong><span className={`ix-status ix-status-${run.phase}`}>{run.phase}</span><p>{run.reason || run.id}</p>{run.journalPath && <details><summary>Run evidence</summary><code>{run.journalPath}</code></details>}{!["succeeded", "failed", "finished", "cancelled", "stopped", "complete", "completed"].includes(run.phase) && <Action disabled={blocked} onClick={() => void mutate("/proposals", { kind: "stop_run", runId: run.id, idempotencyKey: commandId() })}>Preview stop run</Action>}</article>)}</section>
      <details className="ix-details"><summary>Grounding & limitations</summary><p>Repository: {state.repository}</p><p>Config revision: {state.configDigest}</p><ul>{state.backlog?.map(item => <li key={item.id}>{item.id} · {item.title} ({item.source})</li>)}</ul>{state.limitations?.map(text => <p key={text}>{text}</p>)}</details>
    </>}
  </>;
}
