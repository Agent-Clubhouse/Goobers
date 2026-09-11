import type { UpdateAvailability } from "../api/types";

export interface UpdateNoticeProps {
  update: UpdateAvailability;
  onDismiss(): void;
}

/**
 * App-chrome strip announcing that a newer release exists (#4920).
 *
 * Deliberately NOT styled as, or announced as, the adjacent
 * `admission-degraded` alert. That one means the daemon is degraded; an
 * available update is informational, and borrowing an alert's urgency for it
 * devalues the real thing. Hence `role="status"` and its own class.
 *
 * It names no command: which command is correct depends on whether the
 * instance is supervised, and the daemon — which knows — already says so on
 * its own surfaces. Claiming here that `goobers self-update` will work could
 * walk an unsupervised operator into a refusal.
 */
export function UpdateNotice({ update, onDismiss }: UpdateNoticeProps) {
  return (
    <div className="update-notice" role="status">
      <span>
        <strong>Update available:</strong> {update.latestVersion}
        {update.channel ? ` on the ${update.channel} channel` : ""}. This instance is still
        running an older build.
      </span>
      <button
        aria-label={`Dismiss the ${update.latestVersion} update notice`}
        className="update-notice-dismiss"
        onClick={onDismiss}
        type="button"
      >
        Dismiss
      </button>
    </div>
  );
}
