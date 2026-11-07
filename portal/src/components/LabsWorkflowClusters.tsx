import { useState } from "react";
import type {
  DaemonClient,
  LabsConfoundReport,
  LabsClusterFitness,
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
      {view && view.status === "insufficient-data" && view.templates && (
        <WorkflowTemplateProvenance report={view.templates} />
      )}
      {view?.model && <ClusterModel view={view} />}
    </section>
  );
}

function ClusterModel({ view }: { view: LabsWorkflowClusterView }) {
  const model = view.model!;
  const fitness = new Map((view.fitness?.clusters ?? []).map((item) => [item.clusterId, item]));
  const [confoundAxis, setConfoundAxis] = useState("gaggle");
  const confounds =
    view.confoundsByAxis?.[confoundAxis as keyof NonNullable<typeof view.confoundsByAxis>] ??
    (confoundAxis === "gaggle" ? view.confounds : undefined);
  const [cohort, setCohort] = useState("");
  const map = (view.map ?? []).filter((point) => !cohort || point.cohort === cohort);
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
      {view.confoundsByAxis && (
        <label>
          Confound axis{" "}
          <select
            aria-label="Confound axis"
            value={confoundAxis}
            onChange={(event) => setConfoundAxis(event.target.value)}
          >
            {Object.keys(view.confoundsByAxis).map((axis) => (
              <option value={axis} key={axis}>
                {axis === "cohort" ? "task cohort" : axis}
              </option>
            ))}
          </select>
        </label>
      )}
      {confounds && <ConfoundSummary axis={confoundAxis} report={confounds} />}
      {(view.cohorts ?? []).length > 1 && (
        <label className="labs-cohort-filter">
          Task cohort{" "}
          <select value={cohort} onChange={(event) => setCohort(event.target.value)}>
            <option value="">All cohorts</option>
            {(view.cohorts ?? []).map((label) => (
              <option value={label} key={label}>
                {label}
              </option>
            ))}
          </select>
        </label>
      )}
      {view.trends && <TrendSummary report={view.trends} />}
      {!view.trends && view.trendsUnavailable && <p role="status">{view.trendsUnavailable}.</p>}
      {!view.fitness && view.fitnessUnavailable && <p role="status">{view.fitnessUnavailable}.</p>}
      {view.templates && <TemplateConvergence report={view.templates} />}
      {model.clusters.length > 0 && map.length > 0 && (
        <LabsClusterMap
          points={map}
          clusterIds={model.clusters.map((cluster) => cluster.id)}
          medoids={new Set(model.clusters.map((cluster) => cluster.medoid.id))}
          fitness={fitness}
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
            <ClusterCard
              cluster={cluster}
              fitness={fitness.get(cluster.id)}
              provenance={view.templates?.workflows}
              key={cluster.id}
            />
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
        {report.excluded > 0 ? ` · ${report.excluded} excluded` : ""}
        {report.overLimit > 0 ? ` · ${report.overLimit} older runs not scanned` : ""}. Signals are observational, not
        null-baseline or selection claims.
      </p>
      {report.axes.map((axis) => (
        <div key={axis.axis}>
          <h4>By {axis.axis.replace("-", " ")}</h4>
          {axis.missingProvenance > 0 && (
            <p>
              {axis.missingProvenance} of {report.samples} sample{report.samples === 1 ? "" : "s"} lack recorded{" "}
              {axis.axis.replace("-", " ")} provenance and are in no cohort.
              {axis.axis === "authoring-skill" &&
                " Journals do not yet record a digest scoped to the authoring goober's skills, so this axis is not inferred from the run-wide goober digest."}
            </p>
          )}
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

function ConfoundSummary({ axis, report }: { axis: string; report: LabsConfoundReport }) {
  const stats = `Cramér's V ${report.association.toFixed(2)}, p ${report.pValue.toFixed(3)}, ${report.evidence}`;
  const label =
    axis === "cohort" ? "task cohort" : axis === "skills" || axis === "tools" ? `${axis} digest` : axis;
  return (
    <div className={report.confounded ? "labs-confound labs-confound-warning" : "labs-confound"} role="status">
      {report.confounded ? (
        <p>
          <strong>Confound warning:</strong> clusters track {label} boundaries ({stats}). They may
          reflect {label} provenance rather than a shared shape.
        </p>
      ) : (
        <p>
          No {label} confound detected ({stats}; {report.tested} tested, {report.unlabeled} undefined).
        </p>
      )}
      {report.undersized.length > 0 && (
        <p>
          Strata below {report.options.minStratumSize} workflows were not tested:{" "}
          {report.undersized.join(", ")}.
        </p>
      )}
    </div>
  );
}

function TemplateConvergence({ report }: { report: NonNullable<LabsWorkflowClusterView["templates"]> }) {
  return (
    <section aria-label="Convergence beyond templates">
      <h3>Convergence beyond templates</h3>
      <p>
        {report.beyondTemplates.length} cluster{report.beyondTemplates.length === 1 ? "" : "s"}{" "}
        {report.beyondTemplates.length === 1 ? "has" : "have"} a medoid farther than{" "}
        {report.distanceThreshold.toFixed(2)} from every shipped template.
      </p>
      {report.beyondTemplates.length > 0 && (
        <ul>
          {report.beyondTemplates.map((cluster) => (
            <li key={cluster.clusterId}>
              Cluster {cluster.clusterId}: <WorkflowLink id={cluster.medoidId} /> is {cluster.distance.toFixed(2)} from{" "}
              <span className="mono">{cluster.nearestTemplate}</span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function ClusterCard({
  cluster,
  fitness,
  provenance,
}: {
  cluster: LabsWorkflowCluster;
  fitness?: LabsClusterFitness;
  provenance?: NonNullable<LabsWorkflowClusterView["templates"]>["workflows"];
}) {
  const medoidProvenance = provenance?.[cluster.medoid.id];
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
          {medoidProvenance && <TemplateLabel provenance={medoidProvenance} />}
        </dd>
        <dt>Size</dt>
        <dd>{cluster.members.length}</dd>
        <dt>Radius</dt>
        <dd>{cluster.radius.toFixed(2)}</dd>
      </dl>
      {fitness && <ClusterFitness fitness={fitness} />}
      <h4>Members</h4>
      <WorkflowLinks ids={cluster.members} />
      {provenance && (
        <ul className="labs-cluster-features" aria-label="Member template provenance">
          {cluster.members.map((id) => (
            <li key={id}>
              <span className="mono">{id}</span> <TemplateLabel provenance={provenance[id]} />
            </li>
          ))}
        </ul>
      )}
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

function WorkflowTemplateProvenance({ report }: { report: NonNullable<LabsWorkflowClusterView["templates"]> }) {
  const ids = Object.keys(report.workflows).sort();
  if (ids.length === 0) return null;
  return (
    <section aria-label="Workflow template provenance">
      <h3>Template provenance</h3>
      <ul className="labs-cluster-features">
        {ids.map((id) => (
          <li key={id}>
            <WorkflowLink id={id} /> <TemplateLabel provenance={report.workflows[id]} />
          </li>
        ))}
      </ul>
    </section>
  );
}

function TemplateLabel({
  provenance,
}: {
  provenance?: NonNullable<LabsWorkflowClusterView["templates"]>["workflows"][string];
}) {
  if (!provenance) return null;
  const declaredMatches = provenance.declaredTemplateId === provenance.templateId;
  const relation = !provenance.derived ? "nearest" : declaredMatches ? "declared from" : "derived from";
  return (
    <span className={provenance.derived ? "labs-badge labs-badge-template" : "labs-template-nearest"}>
      {relation} {provenance.templateId} · {provenance.distance.toFixed(2)}
      {provenance.explicit && !declaredMatches ? ` · declared ${provenance.declaredTemplateId}, diverged` : null}
    </span>
  );
}

function ClusterFitness({ fitness }: { fitness: LabsClusterFitness }) {
  if (fitness.insufficientRuns) {
    return (
      <section aria-label="Fitness">
        <h4>Fitness</h4>
        <p>Insufficient runs ({fitness.runCount}/{fitness.minimumRuns}); this cluster is not ranked.</p>
      </section>
    );
  }
  return (
    <section aria-label="Fitness">
      <h4>Fitness · {fitness.runCount} runs</h4>
      <dl className="labs-cluster-facts">
        <dt>Success</dt>
        <dd>
          {percent(fitness.successRate)} (95% Wilson {percent(fitness.successInterval.lower)}–
          {percent(fitness.successInterval.upper)})
        </dd>
        <dt>Wall time p50 / p90</dt>
        <dd>{duration(fitness.medianWallTimeSeconds)} / {duration(fitness.p90WallTimeSeconds)}</dd>
        <dt>Cost p50 / p90</dt>
        <dd>${fitness.medianCostUsd.toFixed(2)} / ${fitness.p90CostUsd.toFixed(2)}</dd>
        <dt>Human gate</dt>
        <dd>{percent(fitness.humanGateRate)}</dd>
        <dt>Escalation</dt>
        <dd>{percent(fitness.escalationRate)}</dd>
        <dt>Retry / loop</dt>
        <dd>{percent(fitness.retryLoopRate)}</dd>
      </dl>
    </section>
  );
}

function percent(value: number): string {
  return `${(value * 100).toFixed(0)}%`;
}

function duration(seconds: number): string {
  return seconds < 60 ? `${seconds.toFixed(0)}s` : `${(seconds / 60).toFixed(1)}m`;
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
    return {
      status: view.status,
      population: view.population,
      minClusterSize: view.minClusterSize,
      templates: view.templates && {
        ...view.templates,
        catalog: view.templates.catalog ?? [],
        workflows: view.templates.workflows ?? {},
        beyondTemplates: view.templates.beyondTemplates ?? [],
      },
    };
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
    confoundsByAxis:
      view.confoundsByAxis &&
      Object.fromEntries(
        Object.entries(view.confoundsByAxis).map(([axis, report]) => [
          axis,
          {
            ...report,
            undersized: report.undersized ?? [],
            strata: report.strata ?? [],
          },
        ]),
      ),
    cohorts: view.cohorts ?? [],
    trends: view.trends && {
      ...view.trends,
      candidates: view.trends.candidates ?? 0,
      overLimit: view.trends.overLimit ?? 0,
      axes: (view.trends.axes ?? []).map((axis) => ({
        ...axis,
        missingProvenance: axis.missingProvenance ?? 0,
        cohorts: (axis.cohorts ?? []).map((cohort) => ({
          ...cohort,
          occupancy: cohort.occupancy ?? [],
        })),
      })),
    },
    fitness: view.fitness && {
      ...view.fitness,
      clusters: view.fitness.clusters ?? [],
    },
    templates: view.templates && {
      ...view.templates,
      catalog: view.templates.catalog ?? [],
      workflows: view.templates.workflows ?? {},
      beyondTemplates: view.templates.beyondTemplates ?? [],
    },
  };
}