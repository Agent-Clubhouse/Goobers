import { useMemo, useState } from "react";
import type {
  DaemonClient,
  LabsFleetConvergenceView,
  LabsFleetInstance,
} from "../api/types";
import { MalformedResponseError, isRecord } from "../api/errors";
import { dataCacheKey } from "../dataCache";
import { useLiveQuery } from "../liveQuery";
import { Action } from "../ui/Action";
import { SectionHeading } from "../ui/Heading";
import { LabsClusterMap } from "./LabsClusterMap";

export function LabsFleetConvergence({ client }: { client: DaemonClient }) {
  const query = useLiveQuery<LabsFleetConvergenceView>({
    cacheKey: dataCacheKey("labs-fleet-convergence"),
    dependencies: [{ model: "inventory" }],
    models: ["workflow"],
    load: async (signal) => {
      if (!client.getLabsFleetConvergence) {
        throw new Error("Fleet convergence data is not available from this host.");
      }
      return normalizeFleetView(await client.getLabsFleetConvergence({ signal }));
    },
    errorMessage: "Unable to read fleet convergence.",
  });
  const view =
    query.state.status === "ready" || query.state.status === "stale" ? query.state.data : undefined;

  return (
    <section aria-label="Fleet workflow convergence" className="content-section labs-clusters">
      <SectionHeading
        title="Fleet convergence"
        className=""
        actions={
          <Action variant="text" className="text-button" type="button" onClick={query.retry}>
            Refresh convergence
          </Action>
        }
      />
      <p className="labs-clusters-advisory">
        <strong>Advisory · observational.</strong> Distinct structural shapes are compared across
        enrolled instances. Fleet convergence never promotes, blocks, or changes a workflow.
      </p>
      {query.state.status === "loading" && <p role="status">Clustering fleet workflow shapes…</p>}
      {query.state.status === "error" && (
        <p role="alert">Fleet convergence unavailable: {query.state.error.message}</p>
      )}
      {query.state.status === "stale" && (
        <p role="status">Showing cached fleet convergence; refresh has not completed.</p>
      )}
      {view?.status === "insufficient-data" && (
        <div className="empty-state">
          <h3>Not enough fleet shapes to cluster</h3>
          <p>
            The fleet has {view.population} distinct shape{view.population === 1 ? "" : "s"};
            clustering needs at least {view.minClusterSize}.
          </p>
        </div>
      )}
      {view?.status === "clustered" && <FleetModel view={view} />}
      {view && <ExcludedInstances view={view} />}
    </section>
  );
}

function FleetModel({ view }: { view: LabsFleetConvergenceView }) {
  const instanceIds = view.instances.map((instance) => instance.instanceId);
  const [selectedIds, setSelectedIds] = useState<Set<string>>(() => new Set(instanceIds));
  const [detailId, setDetailId] = useState(instanceIds[0] ?? "");
  const highlightedShapes = useMemo(() => {
    const selected = selectedIds;
    return new Set(
      view.shapes
        .filter((shape) => shape.instances.some((instance) => selected.has(instance)))
        .map((shape) => shape.shape),
    );
  }, [selectedIds, view.shapes]);
  const selectedInstance = view.instances.find((instance) => instance.instanceId === detailId);

  const toggleInstance = (instanceId: string) => {
    setSelectedIds((current) => {
      const next = new Set(current);
      if (next.has(instanceId)) {
        next.delete(instanceId);
      } else {
        next.add(instanceId);
      }
      return next;
    });
  };

  return (
    <>
      <p className="labs-clusters-meta">
        {view.workflows} deployments · {view.population} distinct shapes · {view.instances.length} instances ·
        features <span className="mono">{view.featureVersion}</span>
      </p>
      <fieldset className="labs-fleet-filters">
        <legend>Highlight instances</legend>
        {instanceIds.map((instanceId) => (
          <label key={instanceId}>
            <input
              type="checkbox"
              checked={selectedIds.has(instanceId)}
              onChange={() => toggleInstance(instanceId)}
            />
            {instanceId}
          </label>
        ))}
      </fieldset>
      {view.map && view.map.length > 0 && (
        <LabsClusterMap
          points={view.map}
          clusterIds={view.clusters.map((cluster) => cluster.id)}
          medoids={new Set(view.clusters.map((cluster) => cluster.medoidShape))}
          highlightedIds={highlightedShapes}
          unit="shape"
        />
      )}
      {view.clusters.length === 0 ? (
        <div className="empty-state">
          <h3>No fleet clusters found</h3>
          <p>No dense group of similar workflow shapes was found.</p>
        </div>
      ) : (
        <table className="labs-fleet-table">
          <caption>Fleet clusters</caption>
          <thead>
            <tr>
              <th>Cluster</th>
              <th>Medoid shape</th>
              <th>Distinct shapes</th>
              <th>Deployments</th>
              <th>Instances</th>
            </tr>
          </thead>
          <tbody>
            {view.clusters.map((cluster) => (
              <tr key={cluster.id}>
                <td>{cluster.id}</td>
                <td className="mono">{cluster.medoidShape}</td>
                <td>{cluster.shapes}</td>
                <td>{cluster.workflows}</td>
                <td>{cluster.instances}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {selectedInstance && (
        <InstanceDetail
          instance={selectedInstance}
          instances={view.instances}
          shapes={view.shapes}
          clusters={view.clusters}
          onSelect={setDetailId}
        />
      )}
    </>
  );
}

function InstanceDetail({
  instance,
  instances,
  shapes,
  clusters,
  onSelect,
}: {
  instance: LabsFleetInstance;
  instances: LabsFleetInstance[];
  shapes: LabsFleetConvergenceView["shapes"];
  clusters: LabsFleetConvergenceView["clusters"];
  onSelect: (id: string) => void;
}) {
  const privateClusters = clusters.filter((cluster) => {
    const contributors = new Set(
      shapes
        .filter((shape) => shape.cluster === cluster.id)
        .flatMap((shape) => shape.instances),
    );
    return contributors.size === 1 && contributors.has(instance.instanceId);
  });
  return (
    <section aria-label="How this instance differs from the fleet" className="labs-fleet-instance">
      <h3>How this instance differs from the fleet</h3>
      <label>
        Instance{" "}
        <select value={instance.instanceId} onChange={(event) => onSelect(event.target.value)}>
          {instances.map((option) => (
            <option key={option.instanceId} value={option.instanceId}>
              {option.instanceId}
            </option>
          ))}
        </select>
      </label>
      <dl className="labs-cluster-facts">
        <dt>Aligned</dt>
        <dd>{instance.aligned}</dd>
        <dt>Private</dt>
        <dd>{instance.private}</dd>
        <dt>Outliers</dt>
        <dd>{instance.outliers}</dd>
        <dt>Clusters only this instance contributes to</dt>
        <dd>{privateClusters.length > 0 ? privateClusters.map((cluster) => cluster.id).join(", ") : "None"}</dd>
      </dl>
    </section>
  );
}

function ExcludedInstances({ view }: { view: LabsFleetConvergenceView }) {
  if (view.excluded.length === 0) {
    return null;
  }
  return (
    <section aria-label="Excluded instances">
      <h3>Excluded instances</h3>
      <ul>
        {view.excluded.map((excluded, index) => (
          <li key={`${excluded.instanceId}:${index}`}>
            <strong>{excluded.instanceId}</strong>: {excluded.reason}
          </li>
        ))}
      </ul>
    </section>
  );
}

function normalizeFleetView(value: unknown): LabsFleetConvergenceView {
  if (
    !isRecord(value) ||
    (value.status !== "clustered" && value.status !== "insufficient-data") ||
    typeof value.workflows !== "number" ||
    typeof value.population !== "number" ||
    typeof value.minClusterSize !== "number"
  ) {
    throw new MalformedResponseError("The fleet host returned an invalid convergence view.");
  }
  const view = value as unknown as LabsFleetConvergenceView;
  return {
    ...view,
    instances: Array.isArray(view.instances) ? view.instances : [],
    excluded: Array.isArray(view.excluded) ? view.excluded : [],
    shapes: (Array.isArray(view.shapes) ? view.shapes : []).map((shape) => ({
      ...shape,
      instances: Array.isArray(shape.instances) ? shape.instances : [],
    })),
    clusters: Array.isArray(view.clusters) ? view.clusters : [],
    map: Array.isArray(view.map) ? view.map : [],
  };
}
