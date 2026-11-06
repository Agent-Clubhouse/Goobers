import type { LabsClusterFitness, LabsClusterMapPoint } from "../api/types";
import { routeHash } from "../routing";

const width = 640;
const height = 360;
const padding = 48;
const paletteSize = 8;

interface Placed {
  key: string;
  x: number;
  y: number;
  cluster: string;
  ids: string[];
}

// A 2-D scatter of every workflow. Positions come from classical MDS of the
// model's own distances, so nearby dots are workflows the model considers
// similar; axes have no units. Identical workflows share one dot.
// With unit "shape" (fleet mode) points are workflow-shape digests, not
// gaggle/workflow IDs, so dots are labelled as shapes and are not links.
export function LabsClusterMap({
  points,
  clusterIds,
  medoids,
  fitness,
  highlightedIds,
  unit = "workflow",
}: {
  points: readonly LabsClusterMapPoint[];
  clusterIds: readonly string[];
  medoids: ReadonlySet<string>;
  fitness?: ReadonlyMap<string, LabsClusterFitness>;
  highlightedIds?: ReadonlySet<string>;
  unit?: "workflow" | "shape";
}) {
  if (points.length === 0) {
    return null;
  }
  const placed = place(points);
  const colorOf = (cluster: string) =>
    cluster ? `labs-map-c${clusterIds.indexOf(cluster) % paletteSize}` : "labs-map-noise";
  const showLabels = placed.length <= 40;
  const noun = unit === "shape" ? "workflow shape" : "workflow";
  return (
    <figure className="labs-map">
      <svg
        role="img"
        aria-label={`Scatter map of ${points.length} ${noun}${points.length === 1 ? "" : "s"} in ${clusterIds.length} cluster${clusterIds.length === 1 ? "" : "s"}`}
        viewBox={`0 0 ${width} ${height}`}
        preserveAspectRatio="xMidYMid meet"
      >
        <rect className="labs-map-frame" x={1} y={1} width={width - 2} height={height - 2} rx={8} />
        {placed.map((dot) => {
          const isMedoid = dot.ids.some((id) => medoids.has(id));
          const clusterFitness = fitness?.get(dot.cluster);
          const radius = clusterFitness ? Math.min(12, 6 + Math.sqrt(clusterFitness.runCount)) : dot.ids.length > 1 ? 9 : 7;
          const isHighlighted = highlightedIds === undefined || dot.ids.some((id) => highlightedIds.has(id));
          const label = dot.ids.length > 1 ? `${shortName(dot.ids[0])} +${dot.ids.length - 1}` : shortName(dot.ids[0]);
          const title = `${dot.ids.join(", ")} — ${dot.cluster ? `cluster ${dot.cluster}` : "noise"}${isMedoid ? " (medoid)" : ""}`;
          const className = `labs-map-dot ${colorOf(dot.cluster)}${isHighlighted ? "" : " labs-map-dot-dimmed"}`;
          const content = (
            <>
              <title>{title}</title>
              <circle cx={dot.x} cy={dot.y} r={radius} />
              {clusterFitness && !clusterFitness.insufficientRuns && (
                <circle className="labs-map-fitness" cx={dot.x} cy={dot.y} r={radius + 4} />
              )}
              {isMedoid && <circle className="labs-map-medoid" cx={dot.x} cy={dot.y} r={13} />}
              {showLabels && (
                <text x={dot.x + 15} y={dot.y + 4}>
                  {label}
                </text>
              )}
            </>
          );
          return unit === "shape" ? (
            <g key={dot.key} className={className}>
              {content}
            </g>
          ) : (
            <a key={dot.key} href={workflowHref(dot.ids[0])} className={className}>
              {content}
            </a>
          );
        })}
      </svg>
      <figcaption>
        <ul className="labs-map-legend">
          {clusterIds.map((id) => (
            <li key={id}>
              <span className={`labs-map-swatch ${colorOf(id)}`} aria-hidden="true" /> Cluster {id}
            </li>
          ))}
          <li>
            <span className="labs-map-swatch labs-map-noise" aria-hidden="true" /> Noise
          </li>
          <li>
            <span className="labs-map-swatch labs-map-swatch-medoid" aria-hidden="true" /> Medoid
          </li>
        </ul>
        {unit === "shape" ? (
          <p>
            Closer dots are more similar workflow shapes. The layout is a 2-D approximation of the clustering, so
            distances are indicative only; the axes have no units.
          </p>
        ) : (
          <p>
            Closer dots are more similar workflows. The layout is a 2-D approximation of the clustering, so distances
            are indicative only; the axes have no units. Click a dot to open the workflow.
          </p>
        )}
      </figcaption>
    </figure>
  );
}

function place(points: readonly LabsClusterMapPoint[]): Placed[] {
  const xs = points.map((p) => p.x);
  const ys = points.map((p) => p.y);
  const minX = Math.min(...xs);
  const minY = Math.min(...ys);
  const spanX = Math.max(...xs) - minX;
  const spanY = Math.max(...ys) - minY;
  // One scale for both axes keeps distances honest; labels need right margin.
  const scale = Math.min(
    spanX > 0 ? (width - 2 * padding - 80) / spanX : Infinity,
    spanY > 0 ? (height - 2 * padding) / spanY : Infinity,
  );
  const s = Number.isFinite(scale) ? scale : 0;
  const offsetX = (width - 80 - spanX * s) / 2;
  const offsetY = (height - spanY * s) / 2;
  const byPosition = new Map<string, Placed>();
  for (const point of [...points].sort((a, b) => a.id.localeCompare(b.id))) {
    const x = Math.round(offsetX + (point.x - minX) * s);
    const y = Math.round(offsetY + (point.y - minY) * s);
    const cluster = point.cluster ?? "";
    const key = `${x},${y},${cluster}`;
    const existing = byPosition.get(key);
    if (existing) {
      existing.ids.push(point.id);
    } else {
      byPosition.set(key, { key, x, y, cluster, ids: [point.id] });
    }
  }
  return [...byPosition.values()];
}

function shortName(id: string): string {
  const separator = id.indexOf("/");
  return separator > 0 ? id.slice(separator + 1) : id;
}

function workflowHref(id: string): string {
  const separator = id.indexOf("/");
  if (separator <= 0) {
    return routeHash({ page: "labs", feature: "workflow-clustering" });
  }
  return routeHash({ page: "workflow", id: id.slice(separator + 1), gaggle: id.slice(0, separator) });
}
