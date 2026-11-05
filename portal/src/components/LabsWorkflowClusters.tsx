import type {
  DaemonClient,
  LabsConfoundReport,
  LabsWorkflowCluster,
  LabsWorkflowClusterView,
} from "../api/types";
import { MalformedResponseError, isRecord } from "../api/errors";
import { dataCacheKey } from "../dataCache";
import { useLiveQuery } from "../liveQuery";
import { routeHash } from "../routing";
import { Action } from "../ui/Action";
import { SectionHeading } from "../ui/Heading";
import { LabsClusterMap } from "./LabsClusterMap";

// The Labs workflow-cluster view (#8309). It renders only workflow ids,
// structural feature names, and numbers; descriptors never carry prompt text.
export function LabsWorkflowClusters({ client }: { client: DaemonClient }) {
  const query = useLiveQuery<LabsWorkflowClusterView>({
    cacheKey: dataCacheKey("labs-workflow-clusters"),
    dependencies: [{ model: "inventory" }],
    models: ["workflow"],
    load: async (signal) => normalizeClusterView(await client.getLabsWorkflowClusters({ signal })),
    errorMessage: "Unable to read workflow clusters.",
  });
  const view =
    query.state.status === "ready" || query.state.status === "stale" ? query.state.data : undefined;
  return (
    <section aria-label="Workflow cluster map" className="content-section labs-clusters">
      <SectionHeading
        title="Cluster map"
        className=""
        actions={
          <Action variant="text" className="text-button" type="button" onClick={query.retry}>
            Refresh clusters
          </Action>
        }
      />
      <p className="labs-clusters-advisory">
        <strong>Advisory · observational.</strong> Configured workflows are grouped by structural
        shape only (node kinds, gates, retries, parallelism, graph topology); names and goal text never
        contribute. Clusters are not gym-controlled and never promote, block, or change a workflow.
      </p>
      {query.state.status === "loading" && <p role="status">Clustering configured workflows…</p>}
      {query.state.status === "error" && (
        <p role="alert">Workflow clusters unavailable: {query.state.error.message}</p>
      )}
      {query.state.status === "stale" && (
        <p role="status">Showing cached clusters; refresh has not completed.</p>
      )}
      {view && view.status === "insufficient-data" && (
        <div className="empty-state">
          <h3>Not enough workflows to cluster</h3>
          <p>
            This instance configures {view.population} workflow{view.population === 1 ? "" : "s"};
            clustering needs at least {view.minClusterSize}.
          </p>
        </div>
      )}
      {view?.model && <ClusterModel view={view} />}
    </section>
  );
}

function ClusterModel({ view }: { view: LabsWorkflowClusterView }) {
  const model = view.model!;
  return (
    <>
      <p>
        {model.population} workflows · {model.clusters.length} cluster
        {model.clusters.length === 1 ? "" : "s"} · {model.noise.length} noise · minimum cluster size{" "}
        {model.options.minClusterSize}
      </p>
      <p className="labs-clusters-meta">
        Model <span className="mono">{shortModelId(model.id)}</span> · features{" "}
        <span className="mono">{model.featureVersion}</span>
      </p>
      {view.confounds && <ConfoundSummary report={view.confounds} />}
      {view.trends && <TrendSummary report={view.trends} />}
      {model.clusters.length > 0 && (view.map ?? []).length > 0 && (
        <LabsClusterMap
          points={view.map ?? []}
          clusterIds={model.clusters.map((cluster) => cluster.id)}
          medoids={new Set(model.clusters.map((cluster) => cluster.medoid.id))}
        />
      )}
      {model.clusters.length === 0 ? (
        <div className="empty-state">
          <h3>No clusters found</h3>
          <p>No dense group of similar workflows was found; every workflow is reported as noise.</p>
        </div>
      ) : (
        <ul className="labs-cluster-list">
          {model.clusters.map((cluster) => (
            <ClusterCard cluster={cluster} key={cluster.id} />
          ))}
        </ul>
      )}
      {model.noise.length > 0 && (
        <section aria-label="Noise" className="labs-cluster-noise">
          <h3>Noise</h3>
          <p>Workflows that fit no dense group. They may be novel shapes worth a closer look.</p>
          <WorkflowLinks ids={model.noise} />
        </section>
      )}
    </>
  );
}

function TrendSummary({ report }: { report: NonNullable<LabsWorkflowClusterView["trends"]> }) {
  if (report.samples === 0) {
    return (
      <div className="empty-state">
        <h3>No retained child-workflow history</h3>
        <p>Run agent-authored child workflows to track shape drift across authoring versions.</p>
      </div>
    );
  }
  return (
    <section aria-label="Child workflow shape trends" className="labs-cluster-trends">
      <h3>Agent-authored shape trends</h3>
      <p>
        {report.samples} child workflow{report.samples === 1 ? "" : "s"} assigned against frozen model{" "}
        <span className="mono">{shortModelId(report.modelId)}</span>
        {report.excluded > 0 ? ` · ${report.excluded} excluded` : ""}. Signals are observational, not
        null-baseline or selection claims.
      </p>
      {report.axes.map((axis) => (
        <div key={axis.axis}>
          <h4>By {axis.axis.replace("-", " ")}</h4>
          {axis.cohorts.length === 0 ? (
            <p>No complete provenance for this axis.</p>
          ) : (
            <table>
              <thead>
                <tr>
                  <th>Version</th>
                  <th>Samples</th>
                  <th>Cluster occupancy</th>
                  <th>Entropy</th>
                  <th>Shape distance</th>
                  <th>Fitness</th>
                  <th>Drift</th>
                </tr>
              </thead>
              <tbody>
                {axis.cohorts.map((cohort) => (
                  <tr key={cohort.version}>
                    <td className="mono">{shortModelId(cohort.version)}</td>
                    <td>{cohort.samples}</td>
                    <td>
                      {cohort.occupancy
                        .map(
                          (cluster) =>
                            `${cluster.cluster} ${(cluster.share * 100).toFixed(0)}% (n=${cluster.samples}, fitness ${cluster.meanFitness.toFixed(2)})`,
                        )
                        .join(", ")}
                    </td>
                    <td>{cohort.entropy.toFixed(2)}</td>
                    <td>{cohort.meanPairwiseDistance.toFixed(2)}</td>
                    <td>{cohort.meanFitness.toFixed(2)}</td>
                    <td>{cohort.drift?.signal ?? "baseline"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      ))}
    </section>
  );
}

function ConfoundSummary({ report }: { report: LabsConfoundReport }) {
  const stats = `Cramér's V ${report.association.toFixed(2)}, p ${report.pValue.toFixed(3)}, ${report.evidence}`;
  return (
    <div className={report.confounded ? "labs-confound labs-confound-warning" : "labs-confound"} role="status">
      {report.confounded ? (
        <p>
          <strong>Confound warning:</strong> clusters track gaggle boundaries ({stats}). They may
          reflect which gaggle a workflow belongs to rather than a shared shape.
        </p>
      ) : (
        <p>
          No gaggle confound detected ({stats}; {report.tested} tested).
        </p>
      )}
      {report.undersized.length > 0 && (
        <p>
          Gaggles below {report.options.minStratumSize} workflows were not tested:{" "}
          {report.undersized.join(", ")}.
        </p>
      )}
    </div>
  );
}

function ClusterCard({ cluster }: { cluster: LabsWorkflowCluster }) {
  return (
    <li className="labs-card labs-cluster" aria-label={`Cluster ${cluster.id}`}>
      <div className="labs-card-heading">
        <h3>Cluster {cluster.id}</h3>
        <span className={cluster.stable ? "labs-badge labs-badge-stable" : "labs-badge labs-badge-pending"}>
          {cluster.stable ? "Stable" : "Unstable"} {cluster.stability.toFixed(2)}
        </span>
      </div>
      <dl className="labs-cluster-facts">
        <dt>Medoid</dt>
        <dd>
          <WorkflowLink id={cluster.medoid.id} />
        </dd>
        <dt>Size</dt>
        <dd>{cluster.members.length}</dd>
        <dt>Radius</dt>
        <dd>{cluster.radius.toFixed(2)}</dd>
      </dl>
      <h4>Members</h4>
      <WorkflowLinks ids={cluster.members} />
      {(cluster.distinguishing ?? []).length > 0 && (
        <>
          <h4>Distinguishing features</h4>
          <ul className="labs-cluster-features">
            {(cluster.distinguishing ?? []).map((feature) => (
              <li key={`${feature.feature}=${feature.value ?? ""}`}>
                <span className="mono">
                  {feature.value ? `${feature.feature}=${feature.value}` : feature.feature}
                </span>{" "}
                <span className={feature.effect >= 0 ? "labs-effect-up" : "labs-effect-down"}>
                  {feature.effect >= 0 ? "+" : ""}
                  {feature.effect.toFixed(2)}
                </span>
              </li>
            ))}
          </ul>
        </>
      )}
    </li>
  );
}

function WorkflowLinks({ ids }: { ids: readonly string[] }) {
  return (
    <ul className="labs-cluster-members">
      {[...ids].sort().map((id) => (
        <li key={id}>
          <WorkflowLink id={id} />
        </li>
      ))}
    </ul>
  );
}

function WorkflowLink({ id }: { id: string }) {
  const separator = id.indexOf("/");
  if (separator <= 0) {
    return <span className="mono">{id}</span>;
  }
  const gaggle = id.slice(0, separator);
  const workflow = id.slice(separator + 1);
  return (
    <a className="mono" href={routeHash({ page: "workflow", id: workflow, gaggle })}>
      {id}
    </a>
  );
}

function shortModelId(id: string): string {
  const digest = id.startsWith("sha256:") ? id.slice("sha256:".length) : id;
  return digest.slice(0, 12);
}

// The daemon encodes empty Go slices as null; normalize so rendering never
// has to guess, and reject shapes this view cannot render honestly.
function normalizeClusterView(value: unknown): LabsWorkflowClusterView {
  if (
    !isRecord(value) ||
    (value.status !== "clustered" && value.status !== "insufficient-data") ||
    typeof value.population !== "number" ||
    typeof value.minClusterSize !== "number"
  ) {
    throw new MalformedResponseError("The daemon returned an invalid workflow cluster view.");
  }
  const view = value as unknown as LabsWorkflowClusterView;
  if (view.status === "insufficient-data") {
    return { status: view.status, population: view.population, minClusterSize: view.minClusterSize };
  }
  if (!isRecord(view.model)) {
    throw new MalformedResponseError("The daemon returned workflow clusters without a model.");
  }
  return {
    ...view,
    model: {
      ...view.model,
      clusters: (view.model.clusters ?? []).map((cluster) => ({
        ...cluster,
        members: cluster.members ?? [],
        distinguishing: cluster.distinguishing ?? [],
      })),
      noise: view.model.noise ?? [],
    },
    map: Array.isArray(view.map) ? view.map : [],
    confounds: view.confounds && {
      ...view.confounds,
      undersized: view.confounds.undersized ?? [],
      strata: view.confounds.strata ?? [],
    },
    trends: view.trends && {
      ...view.trends,
      axes: (view.trends.axes ?? []).map((axis) => ({
        ...axis,
        cohorts: (axis.cohorts ?? []).map((cohort) => ({
          ...cohort,
          occupancy: cohort.occupancy ?? [],
        })),
      })),
    },
  };
}