import { runStatusLabel, type RunStatus } from "../prototypeData";

export function StatusBadge({ status }: { status: RunStatus }) {
  return (
    <span className={`status-badge status-${status}`}>
      <span aria-hidden="true" className="status-dot" />
      {runStatusLabel(status)}
    </span>
  );
}
