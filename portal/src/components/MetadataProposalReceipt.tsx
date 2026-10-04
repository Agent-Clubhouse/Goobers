import type { MetadataProposalCommand, SourceView } from "../api/types";
import { metadataPRLink } from "./workbenchMetadataEditing";

export function MetadataProposalReceipt({ command, source }: { command: MetadataProposalCommand; source: SourceView }) {
  const label = { accepted: "Proposal accepted", prepared: "Proposal ready for the next phase", attempting: "Provider attempt pending", unknown: "Provider outcome uncertain", blocked: "Proposal needs inspection", confirmed: "Draft PR confirmed", observed: "Draft PR observed", "not-applied": "Proposal not applied" }[command.state];
  const observed = command.observations.filter((entry) => entry.matches && entry.pullRequest).at(-1)?.pullRequest;
  const pr = command.phases.find((phase) => phase.pullRequest)?.pullRequest ?? observed;
  const link = pr && metadataPRLink(pr.url, source);
  return <section className="workbench-command" aria-label="Metadata proposal receipt">
    <h5>{label}</h5><p role="status">{command.nextAction}</p>
    <dl><div><dt>Command</dt><dd><code>{command.id}</code></dd></div><div><dt>Initiated by</dt><dd>{command.actor.subject} · {command.actor.issuer}</dd></div><div><dt>Accepted</dt><dd><time dateTime={command.acceptedAt}>{command.acceptedAt}</time></dd></div>{command.branch && <div><dt>Proposal branch</dt><dd><code>{command.branch}</code></dd></div>}</dl>
    <ol>{command.phases.map((phase, index) => <li key={index}>{phase.name}: {phase.outcome}{command.observations.some((entry) => entry.phase === index && entry.matches) ? " · exact state observed" : ""}</li>)}</ol>
    {command.state === "observed" && <p>The exact PR was found after an uncertain response. This observation does not prove that the original command created it.</p>}
    {pr && <p>{link ? <a href={link} target="_blank" rel="noopener noreferrer">Review draft PR #{pr.number}</a> : `PR #${pr.number} was reported; its link could not be verified for this source.`}</p>}
    <p>Checking observes existing provider state. Continuing is a separate explicit command. Source content changes only after the PR is merged.</p>
  </section>;
}
