import { runStatusLabel, type RunStatus } from "../prototypeData";
import { Icon, type IconName } from "./Icon";

const statusIcons: Record<RunStatus, IconName> = {
  aborted: "close",
  completed: "check",
  escalated: "alert",
  failed: "alert",
  running: "run",
};

export function StatusBadge({ status }: { status: RunStatus }) {
  return (
    <span className={`status-badge status-${status}`} data-status={status}>
      <span className="status-symbol">
        <Icon name={statusIcons[status]} size={12} />
      </span>
      {runStatusLabel(status)}
    </span>
  );
}
