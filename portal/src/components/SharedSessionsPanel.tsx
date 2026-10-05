import { useEffect, useState } from "react";
import type { DaemonClient, Goober, InteractiveCapabilities, SessionAcceptance, SessionPage } from "../api/types";
import { SharedSessionDetail } from "./SharedSessionDetail";
import { sessionAction, sessionState, useSessionCommand } from "./sessionCommand";
import "../sharedSessions.css";

export function SharedSessionsPanel({ client, gaggle, goobers }: { client: DaemonClient; gaggle: string; goobers: Goober[] }) {
  const [page, setPage] = useState<SessionPage>();
  const [capabilities, setCapabilities] = useState<InteractiveCapabilities>();
  const [cursor, setCursor] = useState<string>();
  const [selected, setSelected] = useState<string>();
  const [revision, setRevision] = useState(0);
  const [busy, setBusy] = useState(true);
  const [pending, setPending] = useState(false);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => {
    const controller = new AbortController();
    setBusy(true); setPage(undefined); setCapabilities(undefined); setError("");
    void Promise.all([client.getInteractiveCapabilities(gaggle, { signal: controller.signal }), client.listSessions(gaggle, { cursor, limit: 20 }, { signal: controller.signal })])
      .then(([access, sessions]) => { if (!controller.signal.aborted) { setCapabilities(access); setPage(sessions); } })
      .catch(() => { if (!controller.signal.aborted) { setSelected(undefined); setError("Shared sessions are unavailable. Check your gaggle access or refresh."); } })
      .finally(() => { if (!controller.signal.aborted) setBusy(false); });
    return () => controller.abort();
  }, [client, gaggle, cursor, revision]);
  function created(value: SessionAcceptance) { setSelected(value.session.id); setCursor(undefined); setRevision((r) => r + 1); }
  return <section className="shared-sessions" aria-label="Shared sessions">
    <header><div><h2>Shared sessions</h2><p>Scope work with an agent and other people in this gaggle. Each message records who sent it.</p></div><button type="button" disabled={busy || creating} onClick={() => setRevision((r) => r + 1)}>Refresh sessions</button></header>
    {busy && <p role="status">Loading sessions…</p>}
    {error && <p role="status">{error}</p>}
    <NewSession client={client} gaggle={gaggle} goobers={goobers} created={created} available={!pending && sessionAction(capabilities, "session.create")} pendingChanged={setCreating} />
    {page && capabilities && !sessionAction(capabilities, "session.create") && <p>Session creation is unavailable with your current gaggle permissions and configuration.</p>}
    <div className="session-layout">
      <nav aria-label="Session list">{page && !busy && <><ul>{page.items.map((session) => <li key={session.id}><button type="button" disabled={pending || creating} aria-label={`Open session: ${session.title}`} aria-pressed={selected === session.id} onClick={() => setSelected(session.id)}><strong>{session.title}</strong><span>{sessionState(session)} · {session.goober}</span></button></li>)}</ul>
        {page.items.length === 0 && <p>No sessions on this page.</p>}
        <div className="session-buttons">{cursor && <button type="button" disabled={pending || creating} onClick={() => setCursor(undefined)}>First sessions</button>}{page.nextCursor && <button type="button" disabled={pending || creating} onClick={() => setCursor(page.nextCursor)}>Next sessions</button>}</div></>}</nav>
      {selected ? <SharedSessionDetail key={`${gaggle}:${selected}`} client={client} gaggle={gaggle} id={selected} parentRevision={revision} pendingChanged={setPending} /> : <p>Select a session to read its messages.</p>}
    </div>
  </section>;
}

function NewSession({ client, gaggle, goobers, created, available, pendingChanged }: { client: DaemonClient; gaggle: string; goobers: Goober[]; created: (value: SessionAcceptance) => void; available: boolean; pendingChanged: (value: boolean) => void }) {
  const [title, setTitle] = useState("");
  const [goober, setGoober] = useState("");
  const command = useSessionCommand<{ title: string; goober: string }>((key, input) => client.createSession(gaggle, key, input), (value) => { setTitle(""); created(value); }, { client, key: gaggle });
  useEffect(() => { pendingChanged(command.busy || command.uncertain); }, [command.busy, command.uncertain, pendingChanged]);
  if (!available) return null;
  return <form className="new-session" onSubmit={(event) => { event.preventDefault(); void command.submit({ title, goober }); }}>
    <label>Session title<input required maxLength={256} value={title} disabled={command.busy || command.uncertain} onChange={(e) => setTitle(e.target.value)} /></label>
    <label>Agent<select required value={goober} disabled={command.busy || command.uncertain} onChange={(e) => setGoober(e.target.value)}><option value="">Select an agent</option>{goobers.map((item) => <option key={item.name} value={item.name}>{item.displayName || item.name}</option>)}</select></label>
    <button type="submit" disabled={command.busy || command.scopeChanged || !title.trim() || !goober}>{command.busy ? "Creating…" : command.uncertain ? "Retry same session creation" : "Create session"}</button>
    {command.notice && <p role="status">{command.notice}</p>}
  </form>;
}
