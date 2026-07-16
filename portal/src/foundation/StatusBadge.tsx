import { runStatusLabel, type RunStatus } from "../fixtures";
import { Icon, type IconName } from "./Icon";

const statusIcons: Record<RunStatus, IconName> = {
  running: "clock",
  completed: "check",
  failed: "alert",
  aborted: "close",
  escalated: "alert",
};

export function StatusBadge({ status }: { status: RunStatus }) {
  return (
    <span className={`status-badge status-${status}`}>
      <span className="status-symbol">
        <Icon name={statusIcons[status]} size={12} />
      </span>
      {runStatusLabel(status)}
    </span>
  );
}
