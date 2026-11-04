import {
  findLabsFeature,
  labsFeatures,
  labsDocsBaseUrl,
  labsIssueBaseUrl,
  labsMaturityLabel,
  type LabsFeature,
  type LabsSurface,
} from "../labs";
import { routeHash } from "../routing";
import { PageHeading } from "../ui/Heading";

export function LabsPage({ feature }: { feature?: string }) {
  const selected = findLabsFeature(feature);
  if (selected) {
    return <LabsFeatureDetail feature={selected} />;
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

function LabsFeatureDetail({ feature }: { feature: LabsFeature }) {
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
      <section className="content-section labs-detail">
        <p>{feature.description}</p>
        {feature.surfaces && feature.surfaces.length > 0 && (
          <>
            <h2>Where to find it</h2>
            <ul className="labs-surfaces">
              {feature.surfaces.map((surface) => (
                <li key={labsSurfaceKey(surface)}>
                  <LabsSurfaceItem surface={surface} />
                </li>
              ))}
            </ul>
          </>
        )}
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
          {feature.surfaces && feature.surfaces.length > 0 ? (
            <div className="empty-state">
              <h2>Portal view coming soon</h2>
              <p>
                The Portal view is still being built. Use the surfaces listed above today, and follow the
                tracking issues for progress.
              </p>
            </div>
          ) : (
            <div className="empty-state">
              <h2>Not available yet</h2>
              <p>This feature is still being built. Follow the tracking issues above for progress.</p>
            </div>
          )}
        </section>
      )}
    </>
  );
}

function labsSurfaceKey(surface: LabsSurface): string {
  switch (surface.kind) {
    case "portal":
      return `portal:${surface.label}`;
    case "cli":
      return `cli:${surface.command}`;
    case "docs":
      return `docs:${surface.path}`;
  }
}

function LabsSurfaceItem({ surface }: { surface: LabsSurface }) {
  switch (surface.kind) {
    case "portal":
      return (
        <>
          Portal: <a href={routeHash(surface.route)}>{surface.label}</a>
        </>
      );
    case "cli":
      return (
        <>
          CLI: <code>{surface.command}</code> — {surface.description}
        </>
      );
    case "docs":
      return (
        <>
          Docs:{" "}
          <a href={`${labsDocsBaseUrl}${surface.path}`} rel="noreferrer" target="_blank">
            {surface.label}
          </a>
        </>
      );
  }
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
