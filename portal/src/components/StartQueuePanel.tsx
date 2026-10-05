import { useEffect, useRef, useState } from "react";
import { DaemonApiError, DaemonAuthError } from "../api/errors";
import type { DaemonClient, InteractiveCapabilities, StartQueueCancelInput, StartQueueItem, StartQueuePage } from "../api/types";

interface Snapshot { client: DaemonClient; gaggle: string; page: StartQueuePage; access: InteractiveCapabilities }
export function StartQueuePanel({ client, gaggle }: { client: DaemonClient; gaggle: string }) {
  const [snapshot, setSnapshot] = useState<Snapshot>();
  const [window, setWindow] = useState<{ client: DaemonClient; gaggle: string; cursor?: string }>({ client, gaggle });
  const [revision, setRevision] = useState(0);
  const [busy, setBusy] = useState(true);
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);
  const cursor = window.client === client && window.gaggle === gaggle ? window.cursor : undefined;
  const visible = snapshot?.client === client && snapshot.gaggle === gaggle ? snapshot : undefined;
  useEffect(() => {
    const controller = new AbortController();
    setSnapshot(undefined); setBusy(true); setError(""); setPending(false);
    void Promise.all([client.getInteractiveCapabilities(gaggle, { signal: controller.signal }), client.getStartQueue(gaggle, { cursor, limit: 25 }, { signal: controller.signal })])
      .then(([access, page]) => {
        if (controller.signal.aborted) return;
        if (access.gaggle !== gaggle || !access.viewer || page.gaggle !== gaggle || page.items.length > 25 || page.items.some((item) => item.gaggle !== gaggle)) throw new Error("Queue scope changed");
        setSnapshot({ client, gaggle, page, access });
      }).catch(() => { if (!controller.signal.aborted) setError("Start queue is unavailable. Check your gaggle access or refresh."); })
      .finally(() => { if (!controller.signal.aborted) setBusy(false); });
    return () => controller.abort();
  }, [client, gaggle, cursor, revision]);
  const canCancel = visible?.access.actions.some((action) => action.action === "queue.cancel" && action.available);
  function replace(item: StartQueueItem) { setSnapshot((prior) => prior && prior.client === client && prior.gaggle === gaggle ? { ...prior, page: { ...prior.page, items: prior.page.items.map((old) => old.acceptanceId === item.acceptanceId ? item : old) } } : prior); }
  return <section aria-label="Workflow start queue" className="shared-sessions">
    <header><div><h2>Workflow start queue</h2><p>Accepted starts retain their original configuration. A cancellation request is not confirmation that execution stopped.</p></div>
      <button type="button" disabled={busy || pending} onClick={() => { setWindow({ client, gaggle }); setRevision((r) => r + 1); }}>Refresh start queue</button></header>
    {busy && <p role="status">Loading start queue…</p>}{error && <p role="status">{error}</p>}
    {visible && !busy && <>
      {!canCancel && <p>Cancellation is unavailable with your current gaggle permissions or configuration.</p>}
      {visible.page.items.length === 0 && <p>No starts in this queue window.</p>}
      <ul>{visible.page.items.map((item) => <li key={item.acceptanceId}><QueueItem client={client} gaggle={gaggle} item={item} available={Boolean(canCancel) && !pending} changed={replace} pendingChanged={setPending} /></li>)}</ul>
      {cursor && <button type="button" disabled={pending} onClick={() => setWindow({ client, gaggle })}>First starts</button>}
      {visible.page.nextCursor && <button type="button" disabled={pending} onClick={() => setWindow({ client, gaggle, cursor: visible.page.nextCursor })}>Next starts</button>}
    </>}
  </section>;
}
function QueueItem({ client, gaggle, item, available, changed, pendingChanged }: { client: DaemonClient; gaggle: string; item: StartQueueItem; available: boolean; changed: (item: StartQueueItem) => void; pendingChanged: (pending: boolean) => void }) {
  const [reason, setReason] = useState(""); const [notice, setNotice] = useState(""); const [busy, setBusy] = useState(false); const [uncertain, setUncertain] = useState(false);
  const command = useRef<StartQueueCancelInput>(undefined); const controller = useRef<AbortController>(undefined);
  useEffect(() => () => controller.current?.abort(), [client, gaggle]);
  async function cancel() {
    if (busy) return;
    const abort = new AbortController(); controller.current = abort; setBusy(true); pendingChanged(true);
    command.current ??= { requestId: crypto.randomUUID(), reason: reason.trim() };
    try {
      const result = await client.cancelQueuedStart(gaggle, item.acceptanceId, command.current, { signal: abort.signal });
      if (abort.signal.aborted) return;
      if (result.gaggle !== gaggle || result.acceptanceId !== item.acceptanceId) throw new Error("Queue identity changed");
      changed(result); setUncertain(false); command.current = undefined; pendingChanged(false);
      setNotice(result.disposition === "cancelled" ? "Cancelled before execution." : "Cancellation recorded. Refresh to check for confirmed termination.");
    } catch (error) {
      if (abort.signal.aborted) return;
      if (error instanceof DaemonAuthError || (error instanceof DaemonApiError && error.status >= 400 && error.status < 500 && error.status !== 429)) {
        command.current = undefined; setUncertain(false); pendingChanged(false); setNotice("Cancellation was refused. Refresh and review the current receipt and access.");
      } else { setUncertain(true); setNotice("The response is unknown. Retry the same cancellation to recover its receipt."); }
    } finally { if (!abort.signal.aborted) setBusy(false); }
  }
  const cancellable = item.state !== "rejected" && !item.cancellation;
  return <article aria-label={`Queued start ${item.acceptanceId}`}>
    <h3>{item.workflow} · {item.source}</h3><p><code>{item.acceptanceId}</code> · {item.disposition ?? item.state}</p>
    <p>Accepted {item.acceptedAt}{item.deadline ? ` · Pending deadline ${item.deadline}` : " · No queue deadline"}</p><p>Configuration <code>{item.generation}</code></p>
    {item.waitingReason && <p>{item.waitingReason}</p>}{item.runId && <a href={`#/run/${encodeURIComponent(item.runId)}`}>Open execution</a>}
    {item.cancellation && <p>Cancellation {item.cancellation.state} · {item.cancellation.actor} · {item.cancellation.reason}</p>}
    {cancellable && <form onSubmit={(event) => { event.preventDefault(); void cancel(); }}><label>Cancellation reason<input maxLength={512} required disabled={busy || uncertain || !available} value={reason} onChange={(event) => setReason(event.target.value)} /></label>
      <button type="submit" disabled={busy || (!available && !uncertain) || (!uncertain && !reason.trim())}>{busy ? "Recording cancellation…" : uncertain ? "Retry same cancellation" : "Cancel start"}</button></form>}
    {notice && <p role="status">{notice}</p>}
  </article>;
}
