import { useEffect, useRef, useState } from "react";
import type { DaemonClient, PRRepairCommand } from "../api/types";

/** Explicit post-turn observation; no repair writes or automatic checks. */
export function PRRepairReceiptLookup({ client, gaggle }: { client: DaemonClient; gaggle: string }) {
 const [command, setCommand] = useState("");
 const [loaded, setLoaded] = useState<{ client: DaemonClient; gaggle: string; value: PRRepairCommand }>();
 const [busy, setBusy] = useState(false);
 const [error, setError] = useState("");
 const controller = useRef<AbortController>(undefined);
 const active = useRef(false);
 useEffect(() => { const next = new AbortController(); controller.current = next; active.current = false; setBusy(false); setLoaded(undefined); setCommand(""); setError(""); return () => next.abort(); }, [client, gaggle]);
 const receipt = loaded?.client === client && loaded.gaggle === gaggle ? loaded.value : undefined;
 async function request(check: boolean) {
  const lifetime = controller.current;
  if (!lifetime || lifetime.signal.aborted || active.current) return;
  const id = check ? receipt?.id : command;
  if (!id || !/^repair-[a-f0-9]{32}$/.test(id)) return;
  active.current = true; setBusy(true); setError("");
  try {
   const value = check ? await client.checkPRRepairCommand(gaggle, id, { signal: lifetime.signal }) : await client.getPRRepairCommand(gaggle, id, { signal: lifetime.signal });
   if (lifetime.signal.aborted) return;
   if (value.id !== id) throw new Error("Repair identity changed");
   setLoaded({ client, gaggle, value });
  } catch {
   if (!lifetime.signal.aborted) { setLoaded(undefined); setError("Repair evidence is unavailable. Check the command ID and current source permissions. No repair write was retried."); }
  } finally { if (!lifetime.signal.aborted) { active.current = false; setBusy(false); } }
 }
 return <details className="workbench-command"><summary>Check a retained PR repair</summary>
  <p>Inspect a repair after its session turn ends. A provider check only reads the exact retained command; it never retries a branch update.</p>
  <form onSubmit={(event) => { event.preventDefault(); void request(false); }}><label>Repair command ID<input value={command} required pattern="repair-[a-f0-9]{32}" maxLength={39} disabled={busy} onChange={(event) => { setCommand(event.target.value); setLoaded(undefined); setError(""); }} /></label><button type="submit" disabled={busy}>Load repair receipt</button></form>
  {error && <p role="alert">{error}</p>}
  {receipt && <section aria-label="Retained PR repair receipt"><h4>{receipt.state === "observed-applied" ? "Exact repair observed" : `Repair ${receipt.state}`}</h4>
   <p>Command: <code>{receipt.id}</code> · source: {receipt.sourceBindingId}</p>
   <p>Original human: {receipt.actor.subject} · {receipt.actor.issuer}</p><p>Original turn run: <code>{receipt.runId}</code></p>
   <p>Original provider acknowledgement: {receipt.receipt?.providerAcknowledged ? "acknowledged" : receipt.receipt?.mutationAttempted ? "unknown" : "no acknowledged attempt"}. Original outcome: {receipt.receipt?.outcome ?? "pending"}.</p>
   {receipt.state === "observed-applied" && <p>The exact retained repair commit was verified. Target custody is released; the original acknowledgement remains unchanged.</p>}
   {receipt.state === "unknown" && <><p>The target remains reserved until exact positive evidence is recorded. Missing or changed provider evidence does not prove that the repair failed.</p><button type="button" disabled={busy} onClick={() => void request(true)}>Check repair provider state</button></>}
   {receipt.state === "attempting" && <p>The original provider attempt has no joined receipt. This check cannot settle an in-flight or unjoined attempt.</p>}
   <ul>{receipt.observations?.map((value, index) => <li key={`${value.at}:${index}`}>{value.matches ? "Exact commit verified" : "Exact repair not proven"} · checked by {value.checker.subject} ({value.checker.issuer}) at <time dateTime={value.at}>{value.at}</time>{value.commitId && <> · <code>{value.commitId}</code></>}</li>)}</ul>
   {!!receipt.omittedObservations && <p>{receipt.omittedObservations} earlier checks omitted from bounded history.</p>}
  </section>}
 </details>;
}
