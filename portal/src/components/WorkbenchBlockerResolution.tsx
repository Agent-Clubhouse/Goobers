import { useEffect, useRef, useState } from "react";
import type { BacklogItem, DaemonClient, Goober, InteractiveCapabilities, InteractiveSession, SessionAcceptance, SessionPage, SourceView } from "../api/types";
import { SharedSessionDetail } from "./SharedSessionDetail";
import { useSessionCommand } from "./sessionCommand";
import "../sharedSessions.css";

interface Props { client: DaemonClient; item: BacklogItem; source: SourceView; goobers: Goober[] }
interface ResolutionInput { sessionId?: string; goober: string; text: string; title: string }

export function WorkbenchBlockerResolution(props: Props) {
  const { client, item, source } = props;
  const identity = JSON.stringify([item.ref.gaggleId, item.ref.sourceBindingId, item.ref.sourceId, source.provider, source.owner, source.project, source.repository]);
  const [scope, setScope] = useState({ client, identity, version: 0 });
  if (scope.client !== client || scope.identity !== identity) { setScope({ client, identity, version: scope.version + 1 }); return null; }
  const marked = item.labels?.some((label) => source.provider === "ado" ? label.toLowerCase() === "goobers:needs-human" : label === "goobers:needs-human");
  if (!marked) return null;
  return <ScopedResolution key={scope.version} {...props} />;
}
function ScopedResolution({ client, item, source, goobers }: Props) {
  const gaggle = item.ref.gaggleId;
  const [opened, setOpened] = useState(false);
  const [loaded, setLoaded] = useState<{ access: InteractiveCapabilities; sessions: SessionPage; revision: number }>();
  const [revision, setRevision] = useState(0);
  const [cursor, setCursor] = useState<string>();
  const [destination, setDestination] = useState("new");
  const [goober, setGoober] = useState("");
  const [guidance, setGuidance] = useState("");
  const [selected, setSelected] = useState<string>();
  const [error, setError] = useState("");
  const [conversationPending, setConversationPending] = useState(false);
  const created = useRef<{ key: string; session: InteractiveSession } | undefined>(undefined);
  const access = loaded?.revision === revision ? loaded.access : undefined;
  const available = source.writeFields?.includes("labels") && permits(access, "backlog.resolve") && permits(access, "session.create") && permits(access, "session.message");
  const command = useSessionCommand<ResolutionInput>(async (key, input) => {
    let sessionId = input.sessionId;
    if (!sessionId) {
      if (created.current?.key !== key) {
        const result = await client.createSession(gaggle, `blocker:create:${key}`, { title: input.title, goober: input.goober });
        verifySession(result, gaggle, undefined, input.goober);
        created.current = { key, session: result.session };
      }
      sessionId = created.current.session.id;
    }
    const result = await client.sendSessionMessage(gaggle, sessionId, `blocker:message:${key}`, { text: input.text });
    verifySession(result, gaggle, sessionId);
    return result;
  }, (value) => { setSelected(value.session.id); setGuidance(""); }, { client, key: JSON.stringify([gaggle, item.ref.sourceBindingId, item.ref.sourceId]) });
  const pending = command.busy || command.uncertain || conversationPending;
  useEffect(() => {
    const controller = new AbortController(); setLoaded(undefined); setError("");
    void client.getInteractiveCapabilities(gaggle, { signal: controller.signal }).then(async (current) => {
      if (current.gaggle !== gaggle) throw new Error("Scope changed");
      const sessions = opened && permits(current, "session.create") && permits(current, "session.message") && permits(current, "backlog.resolve") ? await client.listSessions(gaggle, { cursor, limit: 20 }, { signal: controller.signal }) : { items: [] };
      if (sessions.items.some((session) => session.gaggle !== gaggle)) throw new Error("Session scope changed");
      if (!controller.signal.aborted) setLoaded({ access: current, sessions, revision });
    }).catch(() => { if (!controller.signal.aborted) { setSelected(undefined); setGuidance(""); setError("Blocker resolution is unavailable. Refresh to check your gaggle access."); } });
    return () => controller.abort();
  }, [client, gaggle, opened, cursor, revision]);
  function submit() {
    if (!available || command.busy || conversationPending) return;
    if (!command.uncertain && (!guidance.trim() || (destination === "new" ? !goobers.some((entry) => entry.name === goober) : !loaded?.sessions.items.some((session) => session.id === destination && session.state !== "closed" && session.state !== "cancel-requested")))) return;
    void command.submit({ sessionId: destination === "new" ? undefined : destination, goober, title: `Resolve blocker #${item.locator.id}`, text: resolutionInstruction(item, source, guidance) });
  }
  return <section className="workbench-resolution shared-sessions" aria-label="Resolve backlog blocker">
    <h4>Needs human input</h4>
    <p>Ask an agent to inspect the reason and your answer. It can clear the marker when the recorded reason is resolved and known dependencies are complete.</p>
    {!opened && <button type="button" disabled={!available} onClick={() => setOpened(true)}>Resolve blocker</button>}
    {!access && !error && <p role="status">Checking resolution access…</p>}
    {error && <p role="status">{error}</p>}
    {access && !available && <p role="status">Resolution requires an enabled shared session, backlog resolution permission, and labels access for this source.</p>}
    {opened && <>
      <button type="button" disabled={pending} onClick={() => setRevision((value) => value + 1)}>Refresh resolution access</button>
      {available && !selected && <form onSubmit={(event) => { event.preventDefault(); submit(); }}>
        <p>Source: {source.bindingId} · {source.provider} · {source.owner}{source.project ? ` / ${source.project}` : ""}{source.repository ? ` / ${source.repository}` : ""} · #{item.locator.id} (identity {item.ref.sourceId})</p>
        <label>Resolution session<select value={destination} disabled={pending} onChange={(event) => setDestination(event.target.value)}><option value="new">New shared session</option>{loaded?.sessions.items.filter((session) => session.state !== "closed" && session.state !== "cancel-requested").map((session) => <option key={session.id} value={session.id}>{session.title} · {session.goober}</option>)}</select></label>
        <div className="session-buttons">{cursor && <button type="button" disabled={pending} onClick={() => { setDestination("new"); setCursor(undefined); }}>First resolution sessions</button>}{loaded?.sessions.nextCursor && <button type="button" disabled={pending} onClick={() => { setDestination("new"); setCursor(loaded.sessions.nextCursor); }}>More resolution sessions</button>}</div>
        {destination === "new" && <label>Resolution agent<select required disabled={pending} value={goober} onChange={(event) => setGoober(event.target.value)}><option value="">Select an agent</option>{goobers.map((entry) => <option key={entry.name} value={entry.name}>{entry.displayName || entry.name}</option>)}</select></label>}
        <label>Your answer or guidance<textarea required rows={4} maxLength={8192} disabled={pending} value={guidance} onChange={(event) => setGuidance(event.target.value)} /></label>
        <p>The agent will verify current source evidence and report any dependencies or missing information that still prevent resolution. Work continues if you close this browser.</p>
        <button type="submit" disabled={command.busy || command.scopeChanged || conversationPending || (!command.uncertain && (!guidance.trim() || (destination === "new" && !goober)))}>{command.busy ? "Starting inspection…" : command.uncertain ? "Retry same resolution request" : "Ask agent to resolve blocker"}</button>
      </form>}
      {selected && available && <><p role="status">Resolution request accepted. Follow the agent’s assessment below; refresh the source item to confirm its current marker.</p><SharedSessionDetail key={selected} client={client} gaggle={gaggle} id={selected} parentRevision={revision} pendingChanged={setConversationPending} /></>}
    </>}
    {command.notice && <p role="status">{command.notice}</p>}
  </section>;
}
function permits(access: InteractiveCapabilities | undefined, action: "backlog.resolve" | "session.create" | "session.message") { return access?.actions.some((entry) => entry.action === action && entry.available) ?? false; }
function verifySession(value: SessionAcceptance, gaggle: string, id?: string, goober?: string) {
  if (!value.session.id || value.session.gaggle !== gaggle || (id && value.session.id !== id) || (goober && value.session.goober !== goober)) throw new Error("Session acceptance scope changed");
}
function resolutionInstruction(item: BacklogItem, source: SourceView, guidance: string) {
  const target = { sourceBindingId: item.ref.sourceBindingId, provider: source.provider, owner: source.owner, project: source.project, repository: source.repository, id: item.locator.id, sourceId: item.ref.sourceId };
  return `Help resolve this backlog item's needs-human blocker. Verify this exact source target before any change: ${JSON.stringify(target)}\n\nMy answer or guidance:\n${guidance}\n\nInspect the current reason, source comments, and all known native and learned dependencies. Assess whether the reason is resolved. If it is, use the dedicated inspected needs-human operation and retain your rationale and evidence. For a legacy marker without a recorded reason, this message is my explicit instruction to assess resolution using the guidance above. If evidence is incomplete, dependencies remain open, or the answer is insufficient, keep the marker and explain exactly what is still needed. Report the command receipt and actual outcome; a queued request or uncertain provider reply does not mean the marker was cleared.`;
}
