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

export function StatusBadge({ status }: { status: RunPhase }) {
  return (
    <span className={`status-badge status-${status}`} data-status={status}>
      <span className="status-symbol">
        <Icon name={statusIcons[status]} size={12} />
      </span>
      {statusLabels[status]}
    </span>
  );
}
