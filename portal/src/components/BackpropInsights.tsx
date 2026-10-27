import { useState } from "react";
import type {
  AttributionCohort,
  AttributionEvidenceLink,
  AttributionFaultFinding,
  TelemetryStatsResult,
} from "../api/types";
import { routeHash } from "../routing";
import { ControlGroup, FilterField } from "../ui/Filters";
import { SectionHeading } from "../ui/Heading";

export function BackpropInsights({
  stats,
  stageScoped,
}: {
  stats: TelemetryStatsResult;
  stageScoped: boolean;
}) {
  const [workload, setWorkload] = useState("");
  const [version, setVersion] = useState("");
  const [search, setSearch] = useState("");
  const [confidence, setConfidence] = useState("0");
  const cohorts = stats.attributionCohorts ?? [];
  const audit = stats.faultAudit;
  const recognizedAudit = audit?.schema === "goobers.dev/backprop/fault-audit/v1";
  const needle = search.trim().toLocaleLowerCase();
  const selected = cohorts
    .filter((cohort) => (!workload || cohort.workload === workload) &&
      (!version || cohort.effectiveVersion === version))
    .map((cohort) => ({
      ...cohort,
      topContributingPaths: (cohort.topContributingPaths ?? []).filter((path) =>
        path.confidence >= Number(confidence) &&
        (!needle || (path.nodes ?? []).some((node) => node.toLocaleLowerCase().includes(needle)))),
    }))
    .filter((cohort) => cohort.topContributingPaths.length > 0);

  if (stageScoped) {
    return <p className="inline-empty">Stage-scoped Backprop is unavailable. Choose a workflow, gaggle, or instance scope.</p>;
  }
  if (!stats.attributionCohorts && !recognizedAudit) {
    return <p className="inline-empty">Backprop attribution is unavailable from this telemetry snapshot.</p>;
  }
  const findings = recognizedAudit ? [
    ...(audit.productReliabilityFindings ?? []),
    ...(audit.externalFindings ?? []),
    ...(audit.workflowFindings ?? []),
    ...(audit.mixedOrUnknownFindings ?? []),
  ] : [];

  return (
    <div className="backprop-insights">
      <section className="content-section">
        <SectionHeading title="Backprop attribution" className="" />
        <p>
          Recorded attribution grouped by effective version and workload. Signed score is
          attributed credit/blame; responsibility is unsigned. Neither establishes a causal improvement.
        </p>
        <p className="muted">
          Only the backend's returned top paths are shown. Evidence links are recorded references,
          not independent artifact verification. Model identity is available only when recorded in a path.
        </p>
        {audit?.truncated && <p role="status">Partial data: the fault audit was truncated.</p>}
        {!recognizedAudit && <p role="status">Fault-audit provenance is unavailable or uses an unsupported schema.</p>}
        <ControlGroup label="Backprop filters">
          <FilterField label="Workload">
            <select aria-label="Workload" value={workload} onChange={(event) => setWorkload(event.target.value)}>
              <option value="">All workloads</option>
              {[...new Set([workload, ...cohorts.map((cohort) => cohort.workload ?? "")])]
                .filter(Boolean).sort().map((value) => <option key={value}>{value}</option>)}
            </select>
          </FilterField>
          <FilterField label="Effective version">
            <select aria-label="Effective version" value={version} onChange={(event) => setVersion(event.target.value)}>
              <option value="">All versions</option>
              {[...new Set([version, ...cohorts.map((cohort) => cohort.effectiveVersion ?? "")])]
                .filter(Boolean).sort().map((value) => <option key={value}>{value}</option>)}
            </select>
          </FilterField>
          <FilterField label="Minimum confidence">
            <select aria-label="Minimum confidence" value={confidence} onChange={(event) => setConfidence(event.target.value)}>
              <option value="0">Any confidence</option>
              <option value="0.5">50%</option>
              <option value="0.8">80%</option>
              <option value="0.95">95%</option>
            </select>
          </FilterField>
          <FilterField kind="search" label="Recorded node or model">
            <input aria-label="Recorded node or model" type="search" value={search} onChange={(event) => setSearch(event.target.value)} />
          </FilterField>
        </ControlGroup>
        {selected.length === 0 ? (
          <p className="inline-empty">
            {cohorts.length > 0
              ? "No returned attribution paths match these filters."
              : recognizedAudit && audit.observationsScanned === 0
                ? "No attribution observations in this scope and window."
                : "No versioned cohort paths are available; this does not mean no observations were recorded."}
          </p>
        ) : selected.map((cohort) => (
          <Cohort key={JSON.stringify([cohort.effectiveVersion, cohort.workload])} cohort={cohort} />
        ))}
      </section>
      <section className="content-section">
        <SectionHeading title="Fault-domain audit" className="" />
        {recognizedAudit && (
          <p>
            {audit.mode}; {audit.observationsScanned} observations scanned; {audit.suppressed} findings
            suppressed. Audit window: {audit.since} to {audit.until}.
          </p>
        )}
        <p>Fault findings below retain their own scope; cohort path filters above do not filter the audit.</p>
        {findings.length === 0
          ? <p className="inline-empty">{recognizedAudit ? "No fault findings returned. This is not proof of recovery." : "Fault findings unavailable."}</p>
          : findings.map((finding) => <Finding finding={finding} key={finding.id} />)}
      </section>
      <section className="content-section">
        <SectionHeading title="Rotation and remediation" className="" />
        <p>
          Unavailable: this endpoint supplies no governed rotation decisions or linked remediation
          PR, CI, rollout, and measured-effect status. Audit verification is not deployment success.
          This view cannot disable nodes, approve changes, or execute remediation.
        </p>
      </section>
    </div>
  );
}

function Cohort({ cohort }: { cohort: AttributionCohort }) {
  return (
    <article className="backprop-cohort">
      <h3>{cohort.workload || "Unspecified workload"} / {cohort.effectiveVersion || "Unspecified version"}</h3>
      <p>{cohort.runCount} runs. {cohort.groundTruth
        ? `Ground truth: ${cohort.groundTruth.labeledRunCount} labeled, ${cohort.groundTruth.correctRunCount} correct, ${cohort.groundTruth.incorrectRunCount} incorrect.`
        : "Ground-truth coverage unavailable."}</p>
      {(cohort.topContributingPaths ?? []).map((path) => (
        <div className="backprop-path" key={JSON.stringify(path.nodes)}>
          <ol aria-label="Recorded contribution path" className="backprop-path-nodes">
            {(path.nodes ?? []).map((node, index) => <li key={`${index}:${node}`}><code>{node}</code></li>)}
          </ol>
          <dl className="backprop-metrics">
            <div><dt>Signed score</dt><dd>{path.score === undefined
              ? "Unavailable (older attribution payload)"
              : `${path.score > 0 ? "+" : ""}${path.score.toFixed(3)}`}
              {path.score !== undefined && (
                <span aria-hidden="true" className="backprop-score-bar">
                  <span
                    className={path.score < 0 ? "backprop-score-negative" : "backprop-score-positive"}
                    style={{ width: `${Math.min(Math.abs(path.score), 1) * 50}%` }}
                  />
                </span>
              )}
            </dd></div>
            <div><dt>Responsibility</dt><dd>{percent(path.share)}</dd></div>
            <div><dt>Confidence</dt><dd>{percent(path.confidence)}</dd></div>
          </dl>
          <Evidence title="Supporting evidence" evidence={path.evidence} />
        </div>
      ))}
      <Evidence title="Counterevidence" evidence={cohort.counterEvidence} />
    </article>
  );
}

function Finding({ finding }: { finding: AttributionFaultFinding }) {
  return (
    <article className="backprop-cohort">
      <h3>{finding.domain}: {finding.signature}</h3>
      <p>{finding.rationale}</p>
      <p>Confidence {percent(finding.confidence)}; verification: {finding.verification}.</p>
      <p>Recommended owner: {finding.recommendedOwner || "Unavailable"}. Recommendation: {finding.recommendedAction || "Unavailable"}.</p>
      <p>Versions: {finding.effectiveVersions?.join(", ") || "Unavailable"}. Workflows: {finding.workflows?.join(", ") || "Unavailable"}.</p>
      <p>Environments: {finding.environments?.join(", ") || "None recorded"}.</p>
      {finding.nodePaths?.map((nodes, index) => <p key={index}>Recorded path: <code>{nodes.join(" / ")}</code></p>)}
      <ul aria-label="Finding runs">
        {finding.runIds?.map((id) => <li key={id}><a href={routeHash({ page: "run", id, tab: "journal" })}>Run {id}</a></li>)}
      </ul>
      <p>Alternative domains: {finding.alternativeDomains?.join(", ") || "None recorded"}. Filing threshold met: {finding.fileable ? "yes" : "no"}; not execution approval.</p>
      {finding.counterEvidence?.map((item, index) => <p key={index}>Counterevidence: {item}</p>)}
      <Evidence title="Finding evidence" evidence={finding.evidence} />
    </article>
  );
}

function Evidence({ title, evidence }: { title: string; evidence?: AttributionEvidenceLink[] }) {
  return (
    <details>
      <summary>{title} ({evidence?.length ?? 0} recorded references)</summary>
      {evidence?.length ? <ul>{evidence.map((item, index) => (
        <li key={index}>
          {item.runId ? <a href={routeHash({
            page: "run", id: item.runId, tab: "journal", seq: item.journalSequence,
          })}>Run {item.runId}{item.journalSequence !== undefined ? `, journal ${item.journalSequence}` : ""}</a> : "Run reference unavailable"}
          {item.source && <span> / {item.source}</span>}
          {item.detail && <p>{item.detail}</p>}
          {item.stage && <p>Stage: {item.stage}</p>}
          {item.nodeId && <p>Node: {item.nodeId}</p>}
          {item.journalPath && <p>Journal: <code>{item.journalPath}</code></p>}
          {item.artifactPath && <p>Artifact: <code>{item.artifactPath}</code></p>}
          {item.artifactDigest && <p>Recorded digest: <code>{item.artifactDigest}</code></p>}
          {item.artifactMediaType && <p>Media type: {item.artifactMediaType}</p>}
        </li>
      ))}</ul> : <p>No evidence references returned.</p>}
    </details>
  );
}

function percent(value: number) {
  return `${(value * 100).toFixed(1)}%`;
}
