import type { RunNodeState } from "../runDetailData";

// The full RunNodeState vocabulary, in the order shown — running states
// first, then the three terminal-node outcomes, then the two states a node
// reaches without ever being entered.
const NODE_STATES: { state: RunNodeState; note?: string }[] = [
  { state: "pending" },
  { state: "running" },
  { state: "completed" },
  { state: "failed" },
  { state: "blocked" },
  { state: "aborted" },
  { state: "escalated" },
  { state: "skipped", note: "no-work node the run ended without visiting" },
];

function nodeStateLabel(state: RunNodeState): string {
  return state.charAt(0).toUpperCase() + state.slice(1);
}

// WorkflowGraphLegend is #1431's accessible key for the run graph's visual
// encoding: line style (repass vs. forward) is independent from execution
// emphasis (traversed vs. declared-only), and neither is color alone — every
// sample pairs a shape/style cue with its own text label. A native
// <details>/<summary> gives collapsible behavior and keyboard reachability
// for free, matching ReplayScrubber's existing "Chapter key" legend.
export function WorkflowGraphLegend() {
  return (
    <details className="graph-legend-details">
      <summary>Graph legend</summary>
      <div className="graph-legend-panel">
        <div className="graph-legend-section">
          <h3>Edges</h3>
          <ul aria-label="Edge state key" className="graph-legend-list">
            <li>
              <svg
                aria-hidden="true"
                className="graph-legend-edge-sample"
                height="12"
                viewBox="0 0 28 12"
                width="28"
              >
                <path className="workflow-graph-edge workflow-graph-edge-declared" d="M2 6 H26" />
              </svg>
              <span>
                Declared, not yet traversed as of the selected sequence (dim)
              </span>
            </li>
            <li>
              <svg
                aria-hidden="true"
                className="graph-legend-edge-sample"
                height="12"
                viewBox="0 0 28 12"
                width="28"
              >
                <path
                  className="workflow-graph-edge workflow-graph-edge-declared workflow-graph-edge-traversed"
                  d="M2 6 H26"
                />
              </svg>
              <span>Traversed as of the selected sequence (emphasized)</span>
            </li>
            <li>
              <svg
                aria-hidden="true"
                className="graph-legend-edge-sample"
                height="12"
                viewBox="0 0 28 12"
                width="28"
              >
                <path className="workflow-graph-edge workflow-graph-edge-repass" d="M2 6 H26" />
              </svg>
              <span>
                Repass / back-edge — a dashed line means this route is a
                configured repass, independent of whether it was ever taken;
                an untaken repass stays dashed and dim
              </span>
            </li>
          </ul>
        </div>
        <div className="graph-legend-section">
          <h3>Nodes</h3>
          <ul aria-label="Node state key" className="graph-legend-list">
            {NODE_STATES.map(({ state, note }) => (
              <li key={state}>
                <span
                  aria-hidden="true"
                  className={`graph-legend-node-sample graph-legend-node-${state}`}
                />
                <span>
                  {nodeStateLabel(state)}
                  {note ? ` — ${note}` : ""}
                </span>
              </li>
            ))}
          </ul>
        </div>
      </div>
    </details>
  );
}
