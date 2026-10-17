import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { routeHash, type Navigate } from "../routing";
import { Action } from "../ui/Action";
import { Icon } from "../ui/Icon";
import { PageHeading } from "../ui/Heading";
import { Timestamp } from "../ui/Timestamp";
import { commandId, describeError, HttpInteractiveClient, InteractiveError, isUncertain, mergeEvents, readPending, writePending,
  type Changes, type Command, type InteractiveClient, type InteractiveEvent, type Receipt,
  type Session, type StreamState, type Surface, type RuntimeStatus, type InteractiveShellStatus, type CreateSessionRequest, type ExecutionModeId } from "../interactive/client";
import { OperationsPanel } from "../interactive/OperationsPanel";
import "../interactive/interactive.css";

const defaultClient = new HttpInteractiveClient();
type InspectorTab = "context" | "actions" | "changes" | "children" | "access";
interface PendingCommand { sessionId: string; command: Command }

export function InteractivePage({ sessionId, navigate, client = defaultClient, onShellStatus }: {
  sessionId?: string; navigate: Navigate; client?: InteractiveClient; onShellStatus?: (status: InteractiveShellStatus | undefined) => void;
}) {
  const [runtime, setRuntime] = useState<RuntimeStatus>();
  const [runtimeError, setRuntimeError] = useState<string>();
  const [configurationError, setConfigurationError] = useState<string>();
  const [configurationBusy, setConfigurationBusy] = useState(false);
  const [switchingIdentity, setSwitchingIdentity] = useState(false);
  const [surfaces, setSurfaces] = useState<Surface[]>([]);
  const [sessions, setSessions] = useState<Session[]>([]);
  const [session, setSession] = useState<Session>();
  const [events, setEvents] = useState<InteractiveEvent[]>([]);
  const [changes, setChanges] = useState<Changes>();
  const [loading, setLoading] = useState(true);
  const [discoveryError, setDiscoveryError] = useState<string>();
  const [sessionError, setSessionError] = useState<string>();
  const [actionError, setActionError] = useState<string>();
  const [pending, setPending] = useState<PendingCommand>();
  const [receipt, setReceipt] = useState<Receipt>();
  const [busy, setBusy] = useState(false);
  const [connected, setConnected] = useState(true);
  const [streamState, setStreamState] = useState<StreamState>("connecting");
  const [polling, setPolling] = useState(false);
  const [reload, setReload] = useState(0);
  const [draft, setDraft] = useState("");
  const [tab, setTab] = useState<InspectorTab>("context");
  const [historyOpen, setHistoryOpen] = useState(false);
  const cursor = useRef(0);
  const generation = useRef(0);
  const draftRef = useRef<HTMLTextAreaElement>(null);
  const endRef = useRef<HTMLDivElement>(null);
  const followRef = useRef(true);
  const commandLock = useRef(false);

  const discover = useCallback(async () => {
    setLoading(true); setDiscoveryError(undefined);
    try {
      const [nextSurfaces, nextSessions] = await Promise.all([client.surfaces(), client.sessions()]);
      setSurfaces(nextSurfaces); setSessions(nextSessions);
    } catch (error) {
      setDiscoveryError(error instanceof InteractiveError && error.status === 404
        ? "This daemon does not expose interactive sessions. Start the experimental interactive server and enable a surface to use this workspace."
        : describeError(error));
    } finally { setLoading(false); }
  }, [client]);
  useEffect(() => { void discover(); }, [discover]);
  const refreshRuntime = useCallback(async () => {
    try { setRuntime(await client.status()); setRuntimeError(undefined); }
    catch (error) { setRuntimeError(describeError(error)); }
  }, [client]);
  useEffect(() => { void refreshRuntime(); }, [refreshRuntime]);
  async function refreshConfiguration() {
    setConfigurationBusy(true); setConfigurationError(undefined);
    try { setSurfaces(await client.surfaces()); await refreshRuntime(); }
    catch (error) { setConfigurationError(describeError(error)); }
    finally { setConfigurationBusy(false); }
  }
  useEffect(() => { if (session?.status === "waiting_auth") void refreshRuntime(); }, [session?.status, refreshRuntime]);

  useEffect(() => {
    const currentGeneration = ++generation.current;
    setSession(undefined); setEvents([]); setChanges(undefined); setSessionError(undefined);
    const saved = sessionId ? readPending<PendingCommand>(`pending.${sessionId}`) : undefined;
    setActionError(saved ? "A previous request did not receive a confirmed response. Refresh or retry the original command to reconcile its outcome." : undefined);
    setReceipt(undefined); setPending(saved); setDraft(saved?.command.text ?? "");
    setBusy(false); setConnected(true); cursor.current = 0; followRef.current = true;
    if (!sessionId) return;
    let cancelled = false;
    void client.session(sessionId).then(next => {
      if (!cancelled && currentGeneration === generation.current) setSession(next);
    }).catch(error => { if (!cancelled) setSessionError(describeError(error)); });
    return () => { cancelled = true; };
  }, [client, sessionId]);

  useEffect(() => {
    if (!sessionId || !connected) return;
    let cancelled = false;
    let refreshing = false;
    let streamLive = false;
    let refreshTimer: ReturnType<typeof setTimeout> | undefined;
    setStreamState("connecting"); setPolling(false);
    const refresh = async () => {
      if (refreshing || cancelled) return;
      refreshing = true;
      try {
        const [nextSession, nextEvents] = await Promise.all([client.session(sessionId), client.events(sessionId, cursor.current)]);
        if (cancelled) return;
        setSession(nextSession); setSessionError(undefined);
        setEvents(current => mergeEvents(current, nextEvents));
        if (nextEvents.length) cursor.current = Math.max(cursor.current, ...nextEvents.map(event => event.sequence));
        setSessions(current => [nextSession, ...current.filter(item => item.id !== nextSession.id)]);
        setPolling(!streamLive);
      } catch (error) {
        if (!cancelled) { setSessionError(describeError(error)); setStreamState("reconnecting"); setPolling(false); }
      } finally { refreshing = false; }
    };
    void refresh();
    const subscription = client.subscribe(sessionId, cursor.current, event => {
      if (cancelled || event.sessionId !== sessionId) return;
      setEvents(current => mergeEvents(current, [event]));
      // The REST cursor advances only after a contiguous replay, never because a
      // newer streamed event arrived ahead of an earlier persisted event.
      if (!refreshTimer) refreshTimer = setTimeout(() => { refreshTimer = undefined; void refresh(); }, 150);
    }, state => { if (!cancelled) { streamLive = state === "live"; setStreamState(state); if (streamLive) setPolling(false); } });
    const interval = setInterval(() => { void refresh(); }, 2000);
    return () => { cancelled = true; subscription.close(); clearInterval(interval); clearTimeout(refreshTimer); };
  }, [client, sessionId, connected, reload]);

  useEffect(() => {
    if (!sessionId || tab !== "changes" || !connected) return;
    let cancelled = false;
    void client.changes(sessionId).then(result => { if (!cancelled) setChanges(result); })
      .catch(error => { if (!cancelled) setActionError(describeError(error)); });
    return () => { cancelled = true; };
  }, [client, sessionId, session?.sequence, tab, connected]);

  useEffect(() => { if (followRef.current) endRef.current?.scrollIntoView?.({ block: "nearest" }); }, [events.length, session?.turns?.length]);

  async function dispatch(command: Command, target = sessionId) {
    if (!target || commandLock.current) return;
    const currentGeneration = generation.current;
    commandLock.current = true; setBusy(true); setActionError(undefined);
    writePending(`pending.${target}`, { sessionId: target, command });
    try {
      let result: Receipt | undefined;
      for (let retry = 0; retry < 4; retry++) {
        try { result = await client.command(target, command); break; }
        catch (error) {
          // The server rejects stale_sequence before recording or dispatching a
          // command. Only that definite rejection can refresh the precondition.
          // Unknown responses must retain the exact attempted payload instead.
          if (!(error instanceof InteractiveError) || error.code !== "stale_sequence" || error.status !== 409 || retry === 3) throw error;
          const fresh = await client.session(target);
          if (currentGeneration !== generation.current) return;
          if (fresh.fence !== command.expectedFence) throw new InteractiveError("The execution fence changed. Review the current session before submitting again.", "stale_fence", 409);
          if (command.type === "steer" && (fresh.status !== "running" || fresh.activeTurnId !== command.turnId)) throw new InteractiveError("The targeted turn ended or changed. Your steering was not sent to another turn.", "stale_turn", 409);
          setSession(fresh);
          command = { ...command, expectedSequence: fresh.sequence };
          writePending(`pending.${target}`, { sessionId: target, command });
        }
      }
      if (!result) throw new InteractiveError("The session is changing too quickly. Refresh and submit again.", "stale_sequence", 409);
      writePending(`pending.${target}`, undefined);
      if (currentGeneration !== generation.current) return;
      setReceipt(result); setPending(undefined);
      if (command.type === "message" || command.type === "steer") setDraft("");
      setReload(value => value + 1);
    } catch (error) {
      if (!isUncertain(error)) writePending(`pending.${target}`, undefined);
      if (currentGeneration !== generation.current) return;
      setActionError(describeError(error));
      if (isUncertain(error)) setPending({ sessionId: target, command });
      else { setPending(undefined); setReload(value => value + 1); }
    } finally { commandLock.current = false; if (currentGeneration === generation.current) setBusy(false); }
  }
  function send(type: Command["type"], fields: Partial<Command> = {}) {
    if (!session || !connected || pending) return;
    void dispatch({ commandId: commandId(), type, expectedSequence: session.sequence, expectedFence: session.fence, ...fields });
  }
  function submit(event: FormEvent) {
    event.preventDefault();
    if (!draft.trim() || !session) return;
    send(session.status === "running" ? "steer" : "message", { text: draft.trim(), ...(session.status === "running" ? { turnId: session.activeTurnId } : {}) });
  }
  const inspectorTabs: InspectorTab[] = session?.mode === "helper" ? ["context", "actions", "children", "access"] : ["context", "changes", "actions", "children", "access"];
  async function switchIdentity(name: string) {
    setSwitchingIdentity(true);
    try {
      await client.selectIdentity(name);
      setSession(undefined); setEvents([]); setSessions([]); setConnected(false);
      navigate({ page: "interactive" });
      await refreshRuntime(); await discover();
    } catch (error) { setRuntimeError(describeError(error)); }
    finally { setSwitchingIdentity(false); }
  }
  const surface = surfaces.find(item => item.id === session?.surfaceId);
  const active = session?.status === "running";
  const agentSession = session?.executionMode === "codex-local";
  const demoSession = session?.executionMode === "local-tools";
  const executionOption = runtime?.executionModes?.find(option => option.id === session?.executionMode);
  const retiredPolicy = !!session && (!surface || ((!!session.configRevision || !!surface.configRevision) && session.configRevision !== surface.configRevision));
  const modeDisallowed = !!surface?.executionModes && !!session?.executionMode && !surface.executionModes.includes(session.executionMode);
  const executionUnavailable = retiredPolicy || modeDisallowed || (agentSession ? executionOption?.available !== true : executionOption?.available === false);
  const sessionExecutionLabel = agentSession ? "Agent · Codex" : demoSession ? "Deterministic demo · actual sample tools" : "Execution mode not recorded";
  const canWrite = !!session && !retiredPolicy && !modeDisallowed && connected && !busy && !pending && (!executionUnavailable || active) && ["idle", "running", "error"].includes(session.status);
  const statusLabel = !connected ? "Disconnected" : streamState === "live" ? "Live stream" : polling ? "Connected · polling" : streamState === "reconnecting" ? "Reconnecting" : "Connecting";
  useEffect(() => {
    onShellStatus?.(runtime ? { prototype: runtime.providerMode === "controlled-local", name: "Local prototype", connection: sessionId ? statusLabel : "Interactive service available", worker: session?.lease ? `Worker ${session.lease.status}` : "No active worker" } : undefined);
  }, [runtime, sessionId, statusLabel, session?.lease?.status, onShellStatus]);
  useEffect(() => () => onShellStatus?.(undefined), [onShellStatus]);
  return <div className="interactive-page">
    <PageHeading title="Interactive" description="A conversation with your gaggle. A workspace for the work." actions={<><Action variant="secondary" disabled={configurationBusy} onClick={() => void refreshConfiguration()}>{configurationBusy ? "Refreshing configuration…" : "Refresh configuration"}</Action><span className="ix-prototype">Experimental</span></>} />
    {runtime && <div className="ix-runtime-bar"><span><Icon name="code" size={14} />{session ? sessionExecutionLabel : "Choose execution when you create a session"}</span><span className="ix-provider-label"><Icon name="gate" size={14} />{runtime.capabilities.liveDelegatedAuth ? "Live delegated provider" : "Controlled local provider · no live Azure authentication"}</span><span className="ix-owner">{runtime.localActors?.length ? <label>Local demo actor <select aria-label="Local actor" value={runtime.identity.name} disabled={switchingIdentity || busy} onChange={event => void switchIdentity(event.target.value)}>{runtime.localActors.map(name => <option key={name} value={name}>{name}</option>)}</select></label> : <><Icon name="goober" size={14} />{runtime.identity.name}</>}</span></div>}
    {configurationError && <div role="alert" className="ix-runtime-unavailable">Configuration refresh failed: {configurationError}. Showing the last confirmed configuration.</div>}
    {runtimeError && !discoveryError && <div className="ix-runtime-unavailable" role="status">Execution and provider identity unavailable: {runtimeError}</div>}
    {loading && <div className="ix-loading" role="status"><span className="ix-spinner" />Discovering configured experiences…</div>}
    {discoveryError && <div className="ix-unavailable" role="alert"><Icon name="alert" size={28} /><h2>Interactive workspace unavailable</h2><p>{discoveryError}</p><Action variant="secondary" onClick={() => void discover()}>Retry connection</Action><a href="#/workflows">View configured workflows</a></div>}
    {!loading && !discoveryError && <div className={`ix-workspace${session ? " ix-workspace-session" : ""}`}>
      <aside className={`ix-history${historyOpen ? " ix-history-open" : ""}`} aria-label="Session history">
        <div className="ix-history-heading"><span>YOUR SESSIONS</span><Action variant="icon" aria-label="Close history" className="ix-mobile-only" onClick={() => setHistoryOpen(false)}><Icon name="close" /></Action></div>
        <Action variant="secondary" className="ix-new-session" onClick={() => { navigate({ page: "interactive" }); setHistoryOpen(false); }}>+ New session</Action>
        <div className="ix-history-list">{sessions.length === 0 ? <p className="ix-empty-small">Your conversations will appear here.</p> : sessions.map(item => <a key={item.id} href={routeHash({ page: "interactive", sessionId: item.id })} onClick={event => { event.preventDefault(); navigate({ page: "interactive", sessionId: item.id }); setHistoryOpen(false); }} className={`ix-history-item${item.id === sessionId ? " ix-history-selected" : ""}`} aria-current={item.id === sessionId ? "page" : undefined}><Icon name={item.mode === "helper" ? "chat" : "code"} size={16} /><span><strong>{item.title || "Untitled session"}</strong><small>{humanize(item.status)} · <Timestamp value={item.updatedAt} format="date-time" /></small></span></a>)}</div>
        <p className="ix-history-footnote"><Icon name="gate" size={14} />History belongs to your server identity.</p>
      </aside>
      <div className="ix-center">
        <div className="ix-mobile-toolbar"><Action variant="secondary" onClick={() => setHistoryOpen(!historyOpen)} aria-expanded={historyOpen}><Icon name="menu" />Session history</Action></div>
        {!sessionId && <NewSession runtime={runtime} key={runtime?.identity.subject ?? "local"} principal={runtime?.identity.subject ?? "local"} surfaces={surfaces} client={client} onCreated={created => { setSessions(current => [created, ...current.filter(item => item.id !== created.id)]); navigate({ page: "interactive", sessionId: created.id }); }} />}
        {sessionId && !session && !sessionError && <div className="ix-loading" role="status"><span className="ix-spinner" />Loading session and durable history…</div>}
        {sessionId && sessionError && <div role="alert" className="ix-banner ix-banner-warning"><Icon name="alert" /><div><strong>Session connection needs attention</strong><p>{sessionError}</p><Action onClick={() => { setConnected(true); setReload(value => value + 1); }}>Reconnect</Action></div></div>}
        {session && <>
          <header className="ix-session-header"><div><div className="ix-eyebrow">{surface?.name ?? (session.mode === "helper" ? "Gaggle helper" : "Agent workbench")}</div><h2>{session.title || "Untitled session"}</h2><div className="ix-session-meta"><span className={`ix-status ix-status-${session.status}`}>{humanize(session.status)}</span><span className={`ix-stream${streamState === "live" && connected ? " ix-stream-live" : ""}`}><i />{statusLabel}</span></div></div><div className="ix-session-actions"><Action variant="icon" aria-label={connected ? "Disconnect stream" : "Reconnect"} title={connected ? "Disconnect view; execution continues" : "Reconnect and replay history"} onClick={() => setConnected(value => !value)}><Icon name={connected ? "pause" : "refresh"} /></Action><Action variant="secondary" disabled={busy || !connected || !!pending || (session.status === "stopped" || (session.status === "stopping" && !session.stopFencePending))} onClick={() => send("stop")}><Icon name="close" size={15} />{session.stopFencePending ? "Retry stop" : session.status === "stopping" ? "Stopping…" : "Stop session"}</Action></div></header>
          {agentSession && <div className="ix-execution-notice" role="status"><Icon name="info" size={15} /><span>{executionOption?.liveQualified ? "Agent live qualification reported by this server." : "Agent mode is not live-qualified. Availability only indicates host opt-in; it is not evidence of a successful model run."}</span></div>}
          {executionUnavailable && <div className="ix-banner ix-banner-warning" role="alert"><Icon name="alert" /><p>{retiredPolicy ? "This session is bound to a retired configuration. Create a new session for the current policy." : modeDisallowed ? "This execution mode is no longer allowed by the surface configuration." : executionOption?.reason || "The selected execution mode has not been confirmed available on this host."} This session remains pinned to its original mode; no demo fallback is used.</p></div>}
          {!connected && <div className="ix-banner" role="status"><Icon name="info" /><p>View disconnected. Accepted work may continue on the server. Reconnect to replay saved events; no work is restarted.</p></div>}
          {session.status === "waiting_auth" && <div className="ix-banner ix-banner-warning" role="alert"><Icon name="gate" /><div><strong>Waiting for authorization</strong><p>Provider access is blocked. Review the connection in Access, then explicitly resume when permitted. There is no application identity fallback.</p><Action onClick={() => setTab("access")}>Review access</Action></div></div>}
          {session.status === "recovery_required" && <div className="ix-banner ix-banner-warning" role="alert"><Icon name="alert" /><div><strong>Recovery required</strong><p>{session.recoveryReason || "The previous execution ended unexpectedly. Resume verifies writer custody before accepting new work."}</p><Action disabled={busy || !connected || !!pending || retiredPolicy || executionUnavailable || session.stopFencePending} onClick={() => send("resume")}>Resume session</Action></div></div>}
          {session.stopFencePending && <div className="ix-banner ix-banner-warning" role="alert"><Icon name="alert" /><p>Stop authorization fence is pending. Retry stop to complete propagation; writer custody remains retained.</p></div>}
          {session.status === "stopping" && <div className="ix-banner" role="status"><span className="ix-spinner" /><p>Stop requested. Waiting for the executor to confirm writers and child processes have stopped.</p></div>}
          {session.status === "stopped" && <div className="ix-banner" role="status"><Icon name="check" /><p>Session stopped. History and cumulative changes remain available for review.</p></div>}
          <div className="ix-transcript" role="log" aria-live="polite" aria-relevant="additions" aria-label="Conversation" onScroll={event => { const element = event.currentTarget; followRef.current = element.scrollHeight - element.scrollTop - element.clientHeight < 100; }}>
            {!session.turns?.length && <div className="ix-conversation-empty"><span className="ix-orb"><Icon name={session.mode === "helper" ? "chat" : "code"} size={27} /></span><h3>{demoSession ? "Try the sample investigation." : session.mode === "helper" ? "Start with the bigger picture." : "Go from question to working change."}</h3><p>{demoSession ? "This deterministic demo runs real sample tools from explicit commands. It does not reason about arbitrary requests or use a language model." : session.mode === "helper" ? "Describe your goal in your own words. The agent can inspect gaggle context and propose exact workflow changes for your review." : "Describe the work in your own words. Investigate, edit and test in the session workspace; review proposed workflow actions before they run."}</p><div className="ix-starters">{(demoSession ? session.mode === "helper" ? ["Investigate the sample invoice repository", "Design workflow for the sample gaggle"] : ["Investigate the sample invoice repository", "Fix the sample invoice bug and test it"] : session.mode === "helper" ? ["Help me understand this gaggle and identify a useful next workflow", "Design a workflow for reviewing recent changes and explain the tradeoffs"] : ["Investigate this repository, choose a small improvement, and verify it", "Plan a bounded review workflow for the changes and show me its definition"]).map(text => <button key={text} type="button" onClick={() => { setDraft(text); draftRef.current?.focus(); }}>{text}<Icon name="arrow" size={14} /></button>)}</div></div>}
            {(session.turns ?? []).map(turn => <section className="ix-turn" key={turn.id} aria-label={`Turn ${turn.id}`}><div className="ix-user-message"><div className="ix-message-byline"><span>You</span><Timestamp value={turn.startedAt} /></div><p>{turn.text}</p></div><TurnActivity actorLabel={agentSession ? "Agent" : demoSession ? "Demo executor" : "Executor"} events={events.filter(event => event.turnId === turn.id)} active={active && turn.id === session.activeTurnId} />{turn.error && <p role="alert" className="ix-turn-error">{turn.error}</p>}</section>)}
            <div ref={endRef} />
          </div>
          <div className="ix-composer-wrap">
            {receipt && <div className="ix-receipt" role="status"><Icon name="check" size={14} />{receiptLabel(receipt, events)}<span title={receipt.commandId}>#{receipt.sequence}</span></div>}
            {actionError && <div role="alert" className="ix-action-error"><strong>{pending ? "Command outcome unknown" : "Command not completed"}</strong><p>{actionError}</p>{pending && <><p>Retry keeps the original command ID and payload so the server can return the existing receipt.</p><Action disabled={busy || !connected} onClick={() => void dispatch(pending.command, pending.sessionId)}>Retry same command</Action><Action onClick={() => setReload(value => value + 1)}>Refresh outcome</Action></>}</div>}
            <form onSubmit={submit} className="ix-composer"><label htmlFor="interactive-message">{active ? "Steer this turn" : "Message"}</label><textarea ref={draftRef} id="interactive-message" value={draft} onChange={event => setDraft(event.target.value)} placeholder={active ? "Add direction for the active turn…" : session.status === "stopped" ? "This session is stopped." : demoSession ? "Try investigate, fix, test, review, or wait…" : "Describe the work you want the agent to do…"} disabled={!canWrite} rows={3} onKeyDown={event => { if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) { event.preventDefault(); event.currentTarget.form?.requestSubmit(); } }} /><div className="ix-composer-footer"><span>{active ? "Steering targets this active turn. Delivery is acknowledged separately." : "⌘ / Ctrl + Enter to send · Local edits stay in the session workspace"}</span><Action variant="primary" type="submit" disabled={!canWrite || !draft.trim()}><Icon name="arrow" size={16} />{busy ? "Sending…" : active ? "Send steering" : "Send message"}</Action></div></form>
          </div>
        </>}
      </div>
      {session && <aside className="ix-inspector" aria-label="Session inspector"><div className="ix-inspector-tabs" role="tablist" aria-label="Session details">{inspectorTabs.map(item => <button key={item} type="button" role="tab" id={`ix-tab-${item}`} aria-controls={`ix-panel-${item}`} aria-selected={tab === item} tabIndex={tab === item ? 0 : -1} onClick={() => setTab(item)} onKeyDown={event => { const tabs = inspectorTabs; const index = tabs.indexOf(item); const next = event.key === "ArrowRight" ? tabs[(index + 1) % tabs.length] : event.key === "ArrowLeft" ? tabs[(index + tabs.length - 1) % tabs.length] : undefined; if (next) { event.preventDefault(); setTab(next); document.getElementById(`ix-tab-${next}`)?.focus(); } }}>{humanize(item)}{item === "changes" && !!session.changes?.length && <span>{session.changes.length}</span>}{item === "children" && !!session.children?.length && <span>{session.children.length}</span>}</button>)}</div><div className="ix-inspector-body" role="tabpanel" id={`ix-panel-${tab}`} aria-labelledby={`ix-tab-${tab}`}>
        {tab === "context" && <><div className="ix-inspector-heading"><Icon name="gaggle" /><h3>Session context</h3></div>{surface?.configRevision && !retiredPolicy && <SurfaceSummary surface={surface} />}<p>{surface?.description || "Scope and execution are bound by the configured surface."}</p><dl className="ix-facts"><div><dt>Experience</dt><dd>{surface?.name ?? session.surfaceId}</dd></div><div><dt>Experience mode</dt><dd>{humanize(session.mode)}</dd></div><div><dt>Execution · pinned for this session</dt><dd>{sessionExecutionLabel}</dd></div><div><dt>Owner</dt><dd>{session.owner}</dd></div><div><dt>Worker lease</dt><dd>{session.lease ? humanize(session.lease.status) : "No lease reported"}{session.custodyPending ? " · custody pending" : ""}</dd></div>{session.lease && <><div><dt>Worker</dt><dd>{session.lease.workerId}</dd></div><div><dt>Lease expiry</dt><dd><Timestamp value={session.lease.expiresAt} /></dd></div></>}{session.configDigest && <div><dt>Pinned configuration</dt><dd className="ix-path">{session.configDigest}</dd></div>}<div><dt>Execution fence</dt><dd>{session.fence}</dd></div><div><dt>Persisted sequence</dt><dd>{session.sequence}</dd></div><div><dt>Workspace</dt><dd className="ix-path">{session.workspacePath || "Created when work begins"}</dd></div></dl>{!retiredPolicy && surface?.instructions && <details className="ix-details"><summary>Configured instructions</summary><p>{surface.instructions}</p></details>}<div className="ix-context-note"><Icon name="info" /><p>A new experience starts a new session with its own authority. Server policy controls the worker lease and available tools.</p></div><Action variant="secondary" onClick={() => setTab(session.mode === "helper" ? "actions" : "changes")}>{session.mode === "helper" ? "Manage gaggle" : "Review changes"}</Action><details className="ix-details"><summary>Durable event ledger ({events.length})</summary><ol className="ix-ledger">{events.map(event => <li key={event.sequence}><span>#{event.sequence}</span><strong>{event.type}</strong><Timestamp value={event.time} /></li>)}</ol></details></>}
        {tab === "actions" && <OperationsPanel surface={surface} policyRetired={retiredPolicy} executionMode={session.executionMode} key={session.id} sessionId={session.id} disabled={busy || !connected || !!pending || ["stopped", "stopping", "recovery_required"].includes(session.status)} />}
        {tab === "changes" && <ChangesPanel changes={changes} session={session} events={events} />}
        {tab === "children" && <ChildrenPanel surface={surface} policyRetired={retiredPolicy} executionMode={session.executionMode} session={session} maxChildren={surface?.maxChildren ?? 0} disabled={busy || !connected || !!pending} send={send} />}
        {tab === "access" && <AccessPanel surface={surface} session={session} events={events} runtime={runtime} client={client} refreshRuntime={refreshRuntime} onResume={() => send("resume")} disabled={busy || !connected || !!pending} />}
      </div></aside>}
    </div>}
  </div>;
}

function NewSession({ surfaces, client, onCreated, principal, runtime }: {
  principal: string; surfaces: Surface[]; client: InteractiveClient;
  onCreated: (session: Session) => void; runtime?: RuntimeStatus;
}) {
  const enabled = surfaces.filter(surface => surface.enabled);
  const savedCreate = useRef(readPending<CreateSessionRequest>(`create.${principal}`));
  const [selected, setSelected] = useState(savedCreate.current?.surfaceId ?? enabled[0]?.id ?? "");
  const [executionMode, setExecutionMode] = useState<ExecutionModeId>(savedCreate.current?.executionMode ?? enabled.find(surface => surface.id === selected)?.defaultExecutionMode ?? runtime?.defaultMode ?? "codex-local");
  const [title, setTitle] = useState(savedCreate.current?.title ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const request = useRef<CreateSessionRequest | undefined>(savedCreate.current);
  const [uncertain, setUncertain] = useState(!!savedCreate.current);
  const selectedSurface = enabled.find(surface => surface.id === selected);
  const modes = (runtime?.executionModes ?? []).map(option => selectedSurface?.executionModes && !selectedSurface.executionModes.includes(option.id)
    ? {...option, available: false, reason: "This execution mode is not allowed by the selected surface configuration."} : option);
  const selectedMode = modes.find(option => option.id === executionMode);
  const legacyPending = !!savedCreate.current && !savedCreate.current.executionMode;
  async function create(event: FormEvent) {
    event.preventDefault();
    if (busy || legacyPending || (!uncertain && (!selectedMode?.available || !selectedSurface))) return;
    setBusy(true); setError(undefined);
    const input = request.current ?? { commandId: commandId(), surfaceId: selected, executionMode, title: title.trim() || undefined };
    request.current = input; writePending(`create.${principal}`, input);
    try { const created = await client.create(input); writePending(`create.${principal}`, undefined); onCreated(created); }
    catch (cause) {
      setError(describeError(cause)); setUncertain(isUncertain(cause));
      if (!isUncertain(cause)) { request.current = undefined; writePending(`create.${principal}`, undefined); }
    } finally { setBusy(false); }
  }
  return <div className="ix-welcome">
    <div className="ix-eyebrow">YOUR GAGGLE, IN CONVERSATION</div>
    <h2>What are we working on?</h2>
    <p>Choose an experience and how it executes. Both choices stay fixed for this session.</p>
    {enabled.length === 0 && !uncertain ? <div role="status" className="ix-banner"><Icon name="info" /><p>No interactive experiences are enabled. Configure an optional Helper or Workbench surface on the server.</p></div> : <form onSubmit={event => void create(event)}>
      <fieldset className="ix-surface-grid" disabled={busy || uncertain}>
        <legend className="ix-sr-only">Experience</legend>
        {surfaces.map(surface => <label key={surface.id} className={`ix-surface-card${selected === surface.id ? " ix-surface-selected" : ""}${!surface.enabled ? " ix-surface-disabled" : ""}`}>
          <input type="radio" name="surface" value={surface.id} checked={selected === surface.id} disabled={!surface.enabled} onChange={() => { setSelected(surface.id); setExecutionMode(surface.defaultExecutionMode ?? runtime?.defaultMode ?? "codex-local"); }} />
          <span className="ix-surface-icon"><Icon name={surface.mode === "helper" ? "chat" : "code"} size={25} /></span>
          <strong>{surface.name}</strong><span>{surface.description}</span>
          <small>{surface.enabled ? surface.mode === "helper" ? "Understand · scope · design" : "Investigate · edit · test" : "Disabled by configuration"}</small>
        </label>)}
      </fieldset>
      {!selectedSurface && !uncertain && <p role="status" className="ix-mode-blocked">The selected experience is no longer enabled. Choose a configured experience to start new work.</p>}
      {selectedSurface?.configRevision && <SurfaceSummary surface={selectedSurface} />}
      <fieldset className="ix-execution-choice" disabled={busy || uncertain}>
        <legend>Execution</legend>
        {modes.map(option => <label key={option.id} className={`ix-execution-option${executionMode === option.id ? " ix-execution-selected" : ""}${!option.available ? " ix-execution-unavailable" : ""}`}>
          <input type="radio" name="executionMode" aria-label={option.id === "codex-local" ? "Agent (Codex)" : "Deterministic demo"} value={option.id} checked={executionMode === option.id} disabled={!option.available} onChange={() => setExecutionMode(option.id)} />
          <span><strong>{option.label}</strong>
            <span className="ix-execution-badges">{!option.available && <small>Unavailable</small>}{option.id === "codex-local" && <small>{option.liveQualified ? "Live qualification reported" : "Not live-qualified"}</small>}{option.id === "local-tools" && <small>No language model</small>}</span>
            <p>{option.id === "codex-local" ? "Use natural language to investigate, edit, test, and propose exact workflow actions. Host availability does not prove successful live execution." : "Run explicit commands against the sample repository and provider fixtures. Real tools execute; a deterministic script chooses the steps."}</p>
            {option.reason && <p className="ix-execution-reason">{option.reason}</p>}
          </span>
        </label>)}
        {!modes.length && <p role="status" className="ix-empty-small">Execution choices are unavailable. The server must advertise supported modes before a session can start.</p>}
      </fieldset>
      {selectedMode && !selectedMode.available && <p className="ix-mode-blocked" role="status">The selected execution mode is unavailable for this experience. Choose an allowed mode explicitly; there is no automatic fallback.</p>}
      {legacyPending && <p className="ix-action-error" role="alert">A previous session request did not record an execution mode. Review session history before creating new work; this request cannot be silently assigned a mode.</p>}
      <div className="ix-start-form"><label htmlFor="session-title">Session name <span>(optional)</span></label>
        <input id="session-title" value={title} onChange={event => setTitle(event.target.value)} disabled={busy || uncertain} maxLength={160} placeholder="e.g. Investigate the next improvement" />
        <Action type="submit" variant="primary" disabled={busy || (!uncertain && !selectedSurface) || legacyPending || (!uncertain && !selectedMode?.available)}>{busy ? "Opening session…" : uncertain ? "Retry opening session" : "Start session"}<Icon name="arrow" size={16} /></Action>
      </div>
      {error && <div className="ix-action-error" role="alert">{error}{uncertain && <p>Retrying preserves the original request ID, experience, and execution mode.</p>}</div>}
    </form>}
    <div className="ix-welcome-note"><Icon name="gate" size={16} /><span>Provider connections and execution choices are separate. Neither mode grants live Azure access or approves a proposed action.</span></div>
  </div>;
}

function SurfaceSummary({ surface }: { surface: Surface }) {
  return <div className="ix-surface-policy" aria-label="Configured experience policy">
    <strong>{surface.goober || surface.name}</strong><span>{surface.gaggle ? `Gaggle: ${surface.gaggle} · ` : ""}Model: {surface.model || "Harness default"}</span>
    <span>{surface.maxTurnSeconds ? `${surface.maxTurnSeconds}s per turn · ` : ""}{surface.maxActivePerOwner ? `${surface.maxActivePerOwner} active per owner · ` : ""}{surface.maxActive ? `${surface.maxActive} active total · ` : ""}{surface.maxChildren} children</span>
    <small>DSL configuration · revision {surface.configRevision?.slice(0, 12)}</small>
  </div>;
}

function eventText(event: InteractiveEvent): string {
  const value = event.data.text ?? event.data.delta ?? event.data.message ?? event.data.output ?? event.data.result;
  return typeof value === "string" ? value : value == null ? "" : JSON.stringify(value, null, 2);
}
function TurnActivity({ events, active, actorLabel }: { events: InteractiveEvent[]; active: boolean; actorLabel: string }) {
  const final = events.filter(event => event.type === "assistant.message");
  const partial = events.filter(event => event.type === "assistant.delta").map(eventText).join("");
  const activities = events.filter(event => ["tool.started", "tool.output", "tool.completed", "steering.delivered", "steering.rejected", "auth.required"].includes(event.type));
  return <div className="ix-assistant"><div className="ix-message-byline"><span><Icon name="goober" size={16} />{actorLabel}</span>{active && <span className="ix-working"><span className="ix-spinner" />Working</span>}</div>{activities.length > 0 && <div className="ix-activities">{activities.map(event => <details className="ix-tool" key={event.sequence}><summary><Icon name={event.type === "tool.completed" || event.type === "steering.delivered" ? "check" : event.type === "auth.required" ? "gate" : "code"} size={14} /><span>{activityLabel(event)}</span><span className="ix-event-seq">#{event.sequence}</span></summary><pre>{eventText(event) || JSON.stringify(event.data, null, 2)}</pre></details>)}</div>}{final.length ? final.map(event => <div className="ix-response" key={event.sequence}>{eventText(event)}</div>) : partial ? <div className="ix-response" aria-live="off">{partial}{active && <span className="ix-caret" />}</div> : active ? <p className="ix-empty-small">Waiting for execution output…</p> : null}</div>;
}
function activityLabel(event: InteractiveEvent): string {
  if (event.type === "steering.delivered") return "Steering delivered to the active turn";
  if (event.type === "steering.rejected") return "Steering rejected — turn changed or ended";
  if (event.type === "auth.required") return "Provider authorization required";
  const name = event.data.name ?? event.data.tool ?? event.data.command;
  const verb = event.type === "tool.started" ? "Running" : event.type === "tool.output" ? "Output" : event.data.status === "failed" || (typeof event.data.exitCode === "number" && event.data.exitCode !== 0) ? "Failed" : "Completed";
  return `${verb}: ${typeof name === "string" ? name : Array.isArray(name) ? name.join(" ") : "tool activity"}${typeof event.data.exitCode === "number" ? ` · exit ${event.data.exitCode}` : ""}`;
}
function receiptLabel(receipt: Receipt, events: InteractiveEvent[]): string {
  if (receipt.type === "steer") {
    const delivery = events.find(event => event.commandId === receipt.commandId && ["steering.delivered", "steering.rejected"].includes(event.type));
    return delivery ? delivery.type === "steering.delivered" ? "Steering delivered" : "Steering rejected by executor" : receipt.status === "delivered" ? "Steering delivered" : "Steering accepted · awaiting delivery";
  }
  if (receipt.type === "stop") return "Stop request accepted · check session state for confirmed stop";
  return `${humanize(receipt.type)} ${receipt.status}`;
}
function ChangesPanel({ changes, session, events }: { changes?: Changes; session: Session; events: InteractiveEvent[] }) {
  const files = changes?.changes ?? session.changes ?? [];
  const diff = changes?.diff ?? session.diff;
  const tests = events.filter(event => event.type === "tool.completed" && /(?:test|pytest|vitest|jest)/i.test(String(event.data.name ?? "") + " " + String(event.data.command ?? "")));
  return <><div className="ix-inspector-heading"><Icon name="code" /><h3>Cumulative changes</h3></div><p>Changes are measured against the session workspace base. Review all turns together before taking work elsewhere.</p><section className="ix-test-evidence" aria-label="Test evidence"><h4>Test evidence</h4>{tests.length ? tests.slice(-5).map(event => <details key={event.sequence}><summary><Icon name={event.data.exitCode === 0 ? "check" : "code"} size={13} /><span>{activityLabel(event)}</span></summary><Timestamp value={event.time} /><pre>{eventText(event) || "The executor reported this completion without output."}</pre></details>) : <p>No completed test command has been reported.</p>}</section>{!files.length && !diff ? <div className="ix-inspector-empty"><Icon name="artifact" size={28} /><strong>No changes reported</strong><span>Edits and test output will appear after the executor performs them.</span></div> : <><ul className="ix-change-list">{files.map(file => <li key={file.path}><span title={file.path}>{file.path}</span><small>{file.status}</small>{(file.additions > 0 || file.deletions > 0) && <><b className="ix-added">+{file.additions}</b><b className="ix-deleted">−{file.deletions}</b></>}</li>)}</ul>{diff && <pre className="ix-diff" tabIndex={0} aria-label="Cumulative diff">{diff.split("\n").map((line, index) => <span key={index} className={line.startsWith("+") ? "ix-diff-add" : line.startsWith("-") ? "ix-diff-remove" : line.startsWith("@@") ? "ix-diff-hunk" : ""}>{line}{"\n"}</span>)}</pre>}</>}</>;
}
function ChildrenPanel({ session, maxChildren, disabled, send, executionMode, surface, policyRetired }: {
  surface?: Surface; policyRetired?: boolean; executionMode?: string; session: Session; maxChildren: number; disabled: boolean;
  send: (type: Command["type"], fields?: Partial<Command>) => void;
}) {
  const [prompt, setPrompt] = useState("");
  const isDemo = executionMode === "local-tools";
  const proposalBlocked = disabled || policyRetired || !["idle", "running"].includes(session.status) || maxChildren === 0 || session.children.length >= maxChildren;
  const approvalBlocked = disabled || policyRetired || session.status !== "idle" || maxChildren === 0;
  const workflowAllowed = !surface?.configRevision || surface.tools?.includes("propose_child_workflow");
  return <>
    <div className="ix-inspector-heading"><Icon name="workflow" /><h3>Bounded children</h3></div>
    <p>{isDemo ? "The demo can run the fixed sample test workflow in an isolated snapshot. It does not interpret arbitrary child instructions." : "The agent can propose an exact dynamic workflow or a semantic review task. Each proposal needs your explicit decision. Child work has no external provider authority or management tools."}</p>
    {!isDemo && <p className="ix-mode-blocked">Model child execution is experimental and not live-qualified. A displayed proposal is not an execution receipt.</p>}
    <form className="ix-child-form" onSubmit={event => { event.preventDefault(); if (!proposalBlocked && prompt.trim()) { send("child.request", { text: prompt.trim() }); setPrompt(""); } }}>
      <label htmlFor="child-prompt">Child task</label>
      <textarea id="child-prompt" rows={3} value={prompt} onChange={event => setPrompt(event.target.value)} disabled={proposalBlocked} placeholder={isDemo ? "Run the sample tests in a separate review snapshot…" : "Review these changes and identify missing tests…"} />
      <Action variant="secondary" type="submit" disabled={proposalBlocked || !prompt.trim()}>Propose child</Action>
      <small>{maxChildren ? `Configured limit: ${maxChildren} children` : "Child execution is disabled on this surface."}</small>
    </form>
    <div className="ix-child-list">{(session.children ?? []).map(child => <article className="ix-child" key={child.id}>
      <div><span className={`ix-status ix-status-${child.status}`}>{humanize(child.status)}</span><small>{child.id}</small></div>
      <h4>{child.workflowYaml ? "Dynamic native workflow" : isDemo ? "Fixed sample test workflow" : "Semantic model review"}</h4>
      <p>{child.prompt}</p>
      {child.workflowYaml ? <div className="ix-child-definition">
        <details open={child.status === "proposed"}><summary>Exact workflow definition</summary><pre aria-label={`Workflow definition for ${child.id}`} tabIndex={0}>{child.workflowYaml}</pre></details>
        {child.workflowDigest && <small className="ix-definition-digest">Definition digest: {child.workflowDigest}</small>}
        <p>Approval applies to this saved definition. To change it, request a new proposal; this preview is read-only.</p>
      </div> : !isDemo && <p className="ix-child-review-note">Review the task text above. This proposal requests a model reviewer, not a dynamic native workflow.</p>}
      {child.status === "proposed" && <>
        <div className="ix-child-scope"><Icon name="gate" size={14} />Isolated snapshot · {child.workflowYaml ? "exact approved definition · " : isDemo ? "test-only native workflow · " : "experimental model reviewer · "}no external authority · no patch adoption</div>
        <div className="ix-child-actions"><Action variant="primary" disabled={approvalBlocked || (!!child.workflowYaml && !workflowAllowed)} onClick={() => send("child.review", { childId: child.id, decision: "approve" })}>Run child</Action><Action disabled={disabled} onClick={() => send("child.review", { childId: child.id, decision: "deny" })}>Deny child</Action></div>
      </>}
      {child.result && <pre className="ix-child-result">{child.result}</pre>}
      {["succeeded", "failed"].includes(child.status) && <Action variant="secondary" disabled={disabled} onClick={() => send("child.review", { childId: child.id, decision: "accept" })}>Mark reviewed</Action>}
      {child.status === "uncertain" && <p role="status">The child outcome is unknown after interruption. It is not automatically restarted.</p>}
    </article>)}</div>
  </>;
}

function AccessPanel({ session, events, runtime, client, refreshRuntime, onResume, disabled, surface }: {
  surface?: Surface; session: Session; events: InteractiveEvent[]; runtime?: RuntimeStatus; client: InteractiveClient;
  refreshRuntime: () => Promise<void>; onResume: () => void; disabled: boolean;
}) {
  const [pendingAction, setPendingAction] = useState<string>();
  const [error, setError] = useState<string>();
  const [result, setResult] = useState<string>();
  const authEvents = events.filter(event => event.type === "auth.required" || event.type.startsWith("auth."));
  async function act(id: string, action: "expire" | "reauthenticate" | "revoke") {
    setPendingAction(`${id}:${action}`); setError(undefined); setResult(undefined);
    try { await client.connectionAction(id, action); await refreshRuntime(); setResult(`${humanize(action)} confirmed by the server.`); }
    catch (cause) { setError(describeError(cause)); await refreshRuntime(); }
    finally { setPendingAction(undefined); }
  }
  return <><div className="ix-inspector-heading"><Icon name="gate" /><h3>Identity & access</h3></div>
    <div className="ix-provider-notice"><strong>{runtime?.capabilities.liveDelegatedAuth ? "Live delegated provider" : "Controlled local provider"}</strong><p>{runtime?.capabilities.liveDelegatedAuth ? "Provider authority is mediated by the server." : "ADO and Azure-shaped fixtures exercise the authorization boundary. No live Azure sign-in or delegation has been demonstrated."}</p></div>
    <dl className="ix-facts"><div><dt>Session principal</dt><dd>{session.owner}</dd></div><div><dt>Identity issuer</dt><dd>{runtime?.identity.issuer ?? "Unavailable"}</dd></div><div><dt>Execution · pinned</dt><dd>{session.executionMode ?? "Not recorded"}</dd></div><div><dt>Turn limit</dt><dd>{surface?.maxTurnSeconds && surface.configRevision === session.configRevision ? `${surface.maxTurnSeconds}s` : runtime && !session.configRevision ? `${runtime.limits.maxTurnSeconds}s` : "See bound configuration"}</dd></div></dl>
    {surface?.configRevision === session.configRevision && surface?.configRevision && <SurfaceSummary surface={surface} />}
    {(runtime?.connections ?? []).map(connection => <article className="ix-connection" key={connection.id}><h4>{connection.label}</h4><span className={`ix-status ix-status-${connection.status}`}>{humanize(connection.status)}</span><dl className="ix-facts"><div><dt>Resource scope</dt><dd>{connection.resources?.join(", ") || "None"}</dd></div>{connection.expiresAt && <div><dt>Credential expiry</dt><dd><Timestamp value={connection.expiresAt} /></dd></div>}</dl>{runtime?.providerMode === "controlled-local" && <div className="ix-connection-actions"><Action variant="secondary" disabled={disabled || !!pendingAction || connection.status === "revoked"} onClick={() => void act(connection.id, "reauthenticate")}>Reauthenticate demo connection</Action><Action disabled={disabled || !!pendingAction || connection.status === "revoked"} onClick={() => void act(connection.id, "expire")}>Expire demo connection</Action><Action disabled={disabled || !!pendingAction || connection.status === "revoked"} onClick={() => void act(connection.id, "revoke")}>Revoke demo connection</Action></div>}{connection.status === "revoked" && <p className="ix-empty-small">Revoked access is fenced. Reauthentication cannot restore this grant.</p>}</article>)}
    {pendingAction && <p role="status" className="ix-empty-small">Connection change pending server confirmation…</p>}{error && <p role="alert" className="ix-action-error">{error}</p>}{result && <p role="status" className="ix-empty-small">{result}</p>}
    {session.status === "waiting_auth" && <Action variant="primary" disabled={disabled || !!pendingAction} onClick={onResume}>Resume session</Action>}
    <div className="ix-context-note"><Icon name="info" /><p>A session identity is not a provider credential. Tokens are never requested or displayed in chat. There is no silent application identity fallback.</p></div>
    {authEvents.map(event => <div className="ix-banner ix-banner-warning" key={event.sequence}><p>{eventText(event) || "Provider authorization requires attention on the server."}</p></div>)}
  </>;
}

function humanize(value: string): string { return value.replace(/[._]/g, " ").replace(/^\w/, character => character.toUpperCase()); }
