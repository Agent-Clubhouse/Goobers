import type { DaemonClient, LabsFluidUICandidate, LabsFluidUIReport as Report } from "../api/types";
import { MalformedResponseError, isRecord } from "../api/errors";
import { dataCacheKey } from "../dataCache";
import { useLiveQuery } from "../liveQuery";
import { Action } from "../ui/Action";
import { SectionHeading } from "../ui/Heading";

// Aggregate, identity-free promotion candidates (#8419). The report carries
// only control-level shapes and counts, never who customized what.
export function LabsFluidUIReport({ client }: { client: DaemonClient }) {
  const query = useLiveQuery<Report>({
    cacheKey: dataCacheKey("labs-fluid-ui-report"),
    dependencies: [{ model: "instance" }],
    models: ["instance"],
    load: async (signal) => {
      if (!client.getLabsFluidUIReport) {
        throw new Error("Fluid UI promotion candidates are not available from this host.");
      }
      return normalizeReport(await client.getLabsFluidUIReport({ signal }));
    },
    errorMessage: "Unable to read Fluid UI promotion candidates.",
  });
  const report =
    query.state.status === "ready" || query.state.status === "stale" ? query.state.data : undefined;

  return (
    <section aria-label="Fluid UI promotion candidates" className="content-section labs-clusters">
      <SectionHeading
        title="Promotion candidates"
        className=""
        actions={
          <Action variant="text" className="text-button" type="button" onClick={query.retry}>
            Refresh candidates
          </Action>
        }
      />
      <p className="labs-clusters-advisory">
        <strong>Advisory product proposals.</strong> Candidates never change global defaults or any
        user's customization.
      </p>
      {query.state.status === "loading" && <p role="status">Ranking customization shapes…</p>}
      {query.state.status === "error" && (
        <p role="alert">Promotion candidates unavailable: {query.state.error.message}</p>
      )}
      {query.state.status === "stale" && (
        <p role="status">Showing cached candidates; refresh has not completed.</p>
      )}
      {report && <ReportBody report={report} />}
    </section>
  );
}

function ReportBody({ report }: { report: Report }) {
  return (
    <>
      <p>
        {report.users} distinct user{report.users === 1 ? "" : "s"}. Sustained means kept applied for at
        least {report.sustainedAfterDays} days.
      </p>
      {report.candidates.length === 0 ? (
        <div className="empty-state">
          <h3>No customization shapes recorded yet</h3>
          <p>Candidates appear once people apply Fluid UI customizations.</p>
        </div>
      ) : (
        <table aria-label="Ranked customization shapes">
          <thead>
            <tr>
              <th>Rank</th>
              <th>Shape</th>
              <th>Users</th>
              <th>Active</th>
              <th>Sustained</th>
              <th>Reversion rate</th>
            </tr>
          </thead>
          <tbody>
            {report.candidates.map((candidate) => (
              <CandidateRow candidate={candidate} key={candidate.shape.join(",")} />
            ))}
          </tbody>
        </table>
      )}
      <h3>Unmet needs</h3>
      {report.unmetNeeds.length === 0 ? (
        <p>None recorded.</p>
      ) : (
        <table aria-label="Unmet customization needs">
          <thead>
            <tr>
              <th>Need</th>
              <th>Users</th>
            </tr>
          </thead>
          <tbody>
            {report.unmetNeeds.map((need) => (
              <tr key={need.label}>
                <td className="mono">{need.label}</td>
                <td>{need.users}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}

function CandidateRow({ candidate }: { candidate: LabsFluidUICandidate }) {
  return (
    <tr>
      <td>{candidate.rank}</td>
      <td className="mono">{candidate.shape.join(", ")}</td>
      <td>{candidate.users}</td>
      <td>{candidate.activeUsers}</td>
      <td>{candidate.sustainedUsers}</td>
      <td>{Math.round(candidate.reversionRate * 100)}%</td>
    </tr>
  );
}

function normalizeReport(value: unknown): Report {
  if (!isRecord(value) || typeof value.users !== "number" || typeof value.sustainedAfterDays !== "number") {
    throw new MalformedResponseError("The host returned an invalid Fluid UI report.");
  }
  const report = value as unknown as Report;
  return {
    ...report,
    candidates: (Array.isArray(report.candidates) ? report.candidates : []).map((candidate) => ({
      ...candidate,
      shape: Array.isArray(candidate.shape) ? candidate.shape : [],
    })),
    unmetNeeds: Array.isArray(report.unmetNeeds) ? report.unmetNeeds : [],
  };
}
