import { useEffect, useState } from "react";
import type { DaemonClient, InteractiveCapabilities, InteractiveSession, SessionMessagePage, SessionMessageRequest, SessionPRRepairInspection } from "../api/types";
import { SessionPRPicker } from "./SessionPRPicker";
import { PRRepairReceiptLookup } from "./PRRepairReceiptLookup";
import { sessionAction, sessionState, useSessionCommand } from "./sessionCommand";

export function SharedSessionDetail({ client, gaggle, id, parentRevision, pendingChanged }: { client: DaemonClient; gaggle: string; id: string; parentRevision: number; pendingChanged: (value: boolean) => void }) {
  const [session, setSession] = useState<InteractiveSession>();
  const [messages, setMessages] = useState<SessionMessagePage>();
  const [capabilities, setCapabilities] = useState<InteractiveCapabilities>();
  const [after, setAfter] = useState<number>();
  const [revision, setRevision] = useState(0);
  const [busy, setBusy] = useState(true);
  const [error, setError] = useState("");
  const [repairChoice, setRepairChoice] = useState<{ client: DaemonClient; gaggle: string; session: string; value: SessionPRRepairInspection }>();
  const repair = repairChoice?.client === client && repairChoice.gaggle === gaggle && repairChoice.session === id ? repairChoice.value : undefined;
  function setRepair(value: SessionPRRepairInspection | undefined) { setRepairChoice(value ? { client, gaggle, session: id, value } : undefined); }
  const [text, setText] = useState("");
  const [reason, setReason] = useState("");
  const [notice, setNotice] = useState("");
  function refresh() { setRevision((r) => r + 1); }
  const message = useSessionCommand<SessionMessageRequest>((key, input) => client.sendSessionMessage(gaggle, id, key, input), () => { setText(""); setRepair(undefined); setNotice("Message accepted. The agent will process it in order."); refresh(); }, { client, key: JSON.stringify([gaggle, id]) });
  const close = useSessionCommand<{ reason: string }>((key, input) => client.closeSession(gaggle, id, key, input), (value) => { setNotice(value.session.state === "closed" ? "Session closed." : "Close accepted. Any active work still needs to stop."); refresh(); }, { client, key: JSON.stringify([gaggle, id]) });
  useEffect(() => { pendingChanged(message.busy || message.uncertain || close.busy || close.uncertain); }, [message.busy, message.uncertain, close.busy, close.uncertain, pendingChanged]);
  useEffect(() => () => pendingChanged(false), [pendingChanged]);
  useEffect(() => {
    const controller = new AbortController();
    setRepairChoice(undefined); setBusy(true); setError(""); setSession(undefined); setMessages(undefined); setCapabilities(undefined);
    void Promise.all([client.getSession(gaggle, id, { signal: controller.signal }), client.getSessionMessages(gaggle, id, after, { signal: controller.signal }), client.getInteractiveCapabilities(gaggle, { signal: controller.signal })])
      .then(([current, page, access]) => { if (!controller.signal.aborted) { setSession(current); setMessages(page); setCapabilities(access); } })
      .catch(() => { if (!controller.signal.aborted) { setText(""); setRepair(undefined); setReason(""); setNotice(""); setError("This session is unavailable. Check your access or refresh."); } })
      .finally(() => { if (!controller.signal.aborted) setBusy(false); });
    return () => controller.abort();
  }, [client, gaggle, id, after, revision, parentRevision]);
  const active = session && session.state !== "closed" && session.state !== "cancel-requested";
  const canMessage = active && sessionAction(capabilities, "session.message");
  const canRepair = capabilities?.actions.some((entry) => entry.action === "pr.repair" && entry.available) && capabilities.actions.some((entry) => entry.action === "repository.read" && entry.available);
  useEffect(() => { if (capabilities && !canRepair) setRepairChoice(undefined); }, [capabilities, canRepair]);
  const canInspectRepair = ["pr.repair", "repository.read"].every((action) => capabilities?.actions.some((entry) => entry.action === action && entry.authorized && entry.credentialConfigured));
  const commandPending = message.busy || close.busy;
  return <section className="session-detail" aria-label="Session conversation">
    <header><h3>{session?.title ?? "Session conversation"}</h3><button type="button" disabled={busy || commandPending} onClick={refresh}>Refresh conversation</button></header>
    {busy && <p role="status">Loading conversation…</p>}
    {error && <p role="status">{error}</p>}
    {session && messages && !busy && <>
      <p>{sessionState(session)} · {session.goober}{session.lastOutcome ? ` · Last turn: ${session.lastOutcome}` : ""}</p>
      <p className="session-help">Messages queue while the agent is working. Closing this browser does not stop a turn. The agent uses a bounded conversation history.</p>
      <ol className="session-messages">{messages.items.map((item) => <li key={item.id}>
        <div><strong>{item.actorKind === "human" ? item.actor?.subject ?? "Human" : item.actorKind === "agent" ? "Agent" : "Session"}</strong><time dateTime={item.createdAt}>{new Date(item.createdAt).toLocaleString()}</time></div>
        {item.actor && <small>{item.actor.issuer}</small>}
        <p>{item.text}</p>
        {item.repairTarget && <p>Selected PR #{item.repairTarget.id} · {item.repairTarget.repository.owner}/{item.repairTarget.repository.name} · <code>{item.repairTarget.expectedHeadSha.slice(0, 12)}</code></p>}
        {item.outcome && <small>Turn outcome: {item.outcome}</small>}
        {item.runId && <a href={`#/run/${encodeURIComponent(item.runId)}`}>Open turn run</a>}
      </li>)}</ol>
      {messages.items.length === 0 && <p>No messages yet.</p>}
      <nav className="session-buttons" aria-label="Message pages">{after !== undefined && <button type="button" disabled={commandPending} onClick={() => setAfter(undefined)}>First messages</button>}{messages.nextCursor !== undefined && <button type="button" disabled={commandPending} onClick={() => setAfter(messages.nextCursor)}>Next messages</button>}</nav>
      {canMessage && <form onSubmit={(e) => { e.preventDefault(); void message.submit(repair && canRepair ? { text, repairTarget: repair.target } : { text }); }}>
        {canRepair && <SessionPRPicker client={client} gaggle={gaggle} disabled={commandPending || message.uncertain || close.uncertain} value={repair} onChange={setRepair} />}
        <label>Message to agent<textarea required rows={4} maxLength={65536} value={text} disabled={commandPending || message.uncertain || close.uncertain} onChange={(e) => setText(e.target.value)} /></label>
        <button type="submit" disabled={commandPending || close.uncertain || message.scopeChanged || (!message.uncertain && !text.trim())}>{message.busy ? "Submitting…" : message.uncertain ? "Retry same message" : repair && canRepair ? "Send message with PR" : "Send message"}</button>
      </form>}
      {active && !canMessage && <p>Messages are read-only with your current permissions and configuration.</p>}
      {canInspectRepair && <PRRepairReceiptLookup key={id} client={client} gaggle={gaggle} />}
      {canMessage && <details><summary>Close session</summary><p>Stop accepting new messages and request cancellation of any active turn.</p><form onSubmit={(e) => { e.preventDefault(); void close.submit({ reason }); }}><label>Reason<input maxLength={4096} value={reason} disabled={commandPending || close.uncertain || message.uncertain} onChange={(e) => setReason(e.target.value)} /></label><button type="submit" disabled={commandPending || message.uncertain || close.scopeChanged}>{close.busy ? "Requesting close…" : close.uncertain ? "Retry same close request" : "Close session"}</button></form></details>}
    </>}
    {(notice || message.notice || close.notice) && <p role="status">{message.notice || close.notice || notice}</p>}
  </section>;
}
