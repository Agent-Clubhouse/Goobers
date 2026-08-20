import type { RunPhase } from "../api/types";
import { Icon, type IconName } from "./Icon";

const statusIcons: Record<RunPhase, IconName> = {
  aborted: "close",
  completed: "check",
  escalated: "alert",
  failed: "alert",
  running: "run",
};

const statusLabels: Record<RunPhase, string> = {
  aborted: "Aborted",
  completed: "Completed",
  escalated: "Escalated",
  failed: "Failed",
  running: "Running",
};

export function StatusBadge({ stale = false, status }: { stale?: boolean; status: RunPhase }) {
  const effectiveStatus = stale ? "stale" : status;
  return (
    <span className={`status-badge status-${effectiveStatus}`} data-status={effectiveStatus}>
      <span className="status-symbol">
        <Icon name={stale ? "alert" : statusIcons[status]} size={12} />
      </span>
      {stale ? "Stale / unmonitored" : statusLabels[status]}
    </span>
  );
}
