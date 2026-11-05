import type { ReactNode } from "react";
import type { DaemonClient } from "../api/types";
import { LabsWorkflowClusters } from "../components/LabsWorkflowClusters";
import {
  findLabsFeature,
  labsFeatures,
  labsIssueBaseUrl,
  labsMaturityLabel,
  type LabsFeature,
} from "../labs";
import { routeHash } from "../routing";
import { PageHeading } from "../ui/Heading";

// Views for features whose availability is "available", keyed by feature id.
const labsViews: Record<string, (client: DaemonClient) => ReactNode> = {
  "workflow-clustering": (client) => <LabsWorkflowClusters client={client} />,
};

export function LabsPage({ client, feature }: { client: DaemonClient; feature?: string }) {
  const selected = findLabsFeature(feature);
  if (selected) {
    return <LabsFeatureDetail client={client} feature={selected} />;
  }
  return <LabsCatalog unknownFeature={feature} />;
}

function LabsCatalog({ unknownFeature }: { unknownFeature?: string }) {
  return (
    <>
      <PageHeading
        title="Labs"
        description="Experimental Goobers features. Labs results are advisory, may change or disappear, and never modify configuration or bypass gates."
      />
      {unknownFeature && (
        <p className="labs-notice" role="status">
          No Labs feature named <span className="mono">{unknownFeature}</span>. Showing all Labs features.
        </p>
      )}
      <section aria-label="Labs features" className="content-section">
        <ul className="labs-catalog">
          {labsFeatures.map((entry) => (
            <li className="labs-card" key={entry.id}>
              <div className="labs-card-heading">
                <h2>
                  <a href={routeHash({ page: "labs", feature: entry.id })}>{entry.name}</a>
                </h2>
                <LabsBadges feature={entry} />
              </div>
              <p>{entry.summary}</p>
            </li>
          ))}
        </ul>
      </section>
    </>
  );
}

function LabsFeatureDetail({ client, feature }: { client: DaemonClient; feature: LabsFeature }) {
  const view = feature.availability === "available" ? labsViews[feature.id] : undefined;
  return (
    <>
      <PageHeading
        beforeTitle={
          <a className="labs-back" href={routeHash({ page: "labs" })}>
            All Labs features
          </a>
        }
        title={feature.name}
        titleActions={<LabsBadges feature={feature} />}
        description={feature.summary}
      />
      <p className="labs-banner" role="note">
        <strong>Labs: experimental.</strong> This feature is advisory and may change or be removed.
        It never writes configuration or bypasses validation, promotion, or approval gates.
      </p>
      {view?.(client)}
      <section className="content-section labs-detail">
        <p>{feature.description}</p>
        <h2>Prerequisites</h2>
        <ul>
          {feature.prerequisites.map((prerequisite) => (
            <li key={prerequisite}>{prerequisite}</li>
          ))}
        </ul>
        <h2>Tracking issues</h2>
        <ul className="labs-issues">
          {feature.issues.map((issue) => (
            <li key={issue}>
              <a href={`${labsIssueBaseUrl}${issue}`} rel="noreferrer" target="_blank">
                #{issue}
              </a>
            </li>
          ))}
        </ul>
      </section>
      {feature.availability === "coming-soon" && (
        <section className="content-section">
          <div className="empty-state">
            <h2>Not available yet</h2>
            <p>This feature is still being built. Follow the tracking issues above for progress.</p>
          </div>
        </section>
      )}
    </>
  );
}

function LabsBadges({ feature }: { feature: LabsFeature }) {
  return (
    <span className="labs-badges">
      <span className={`labs-badge labs-badge-${feature.maturity}`}>
        {labsMaturityLabel(feature.maturity)}
      </span>
      {feature.availability === "coming-soon" && (
        <span className="labs-badge labs-badge-pending">Coming soon</span>
      )}
    </span>
  );
}
