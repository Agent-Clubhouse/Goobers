import type { BacklogEditCommand } from "../api/types";

export function WorkbenchCommandReceipt({ command }: { command: BacklogEditCommand }) {
  const outcome = { accepted: "Command accepted; attempt not claimed", attempting: "Attempt in progress or reply lost", confirmed: "Change confirmed", "not-applied": "Change not applied", unknown: "Outcome uncertain" }[command.state];
  return <section className="workbench-command" aria-label="Backlog command receipt">
    <h5>{outcome}</h5><p role="status">{command.nextAction}</p>
    <dl><div><dt>Command</dt><dd><code>{command.id}</code></dd></div><div><dt>Initiated by</dt><dd>{command.actor.subject} · {command.actor.issuer}</dd></div><div><dt>Accepted</dt><dd><time dateTime={command.acceptedAt}>{command.acceptedAt}</time></dd></div></dl>
    {command.receipt && <p>Provider acknowledgement: <strong>{command.receipt.providerAcknowledged ? "received" : "not received"}</strong>. Observed value matches: <strong>{command.receipt.observedMatches ? "yes" : "not verified"}</strong>.</p>}
    {command.state === "unknown" && <p>A matching observation does not prove this command made the change. Inspect the source; this write will not be retried.</p>}
    <p>Settled receipts remain available for 30 days, with the request key reserved for another 30 days. Uncertain commands remain retained.</p>
  </section>;
}
