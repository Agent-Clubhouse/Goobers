import type { ChildActivity } from "../api/types";
import type { Navigate } from "../routing";
import { Action } from "../ui/Action";
import { Timestamp } from "../ui/Timestamp";

export function ChildActivityPanel({ activity, navigate }: { activity?: ChildActivity; navigate: Navigate }) {
  if (!activity) return null;
  return (
    <section aria-labelledby="child-activity-heading" className="run-lineage child-activity">
      <h2 id="child-activity-heading">Child workflows</h2>
      {activity.status === "unavailable" ? (
        <p role="status">Recorded child relationships are unavailable. Check the run journal before intervening.</p>
      ) : (
        <>
          {activity.parent && (
            <p>
              Created by <Action onClick={() => navigate({ page: "run", id: activity.parent!.runId })}>{activity.parent.workflow}</Action>
              {" · "}<span className="mono">{activity.parent.stageOccurrence}</span>
            </p>
          )}
          {activity.waits.length > 0 && (
            <>
              <p>{activity.parked ? "This run is waiting on child workflows." : "These stages are waiting on child workflows."}</p>
              <ul>
                {activity.waits.map((wait) => (
                  <li key={`${wait.branch}:${wait.sequence}`}>
                    <strong>{wait.stage}</strong>{wait.branch > 0 ? ` · Branch ${wait.branch}` : ""}
                    {" · "}{wait.action === "wait" ? "Awaiting child outcome" : `Awaiting ${wait.action} of child changes`}
                    {" · "}<Action aria-label={`Open child ${wait.runId}`} title={wait.runId} onClick={() => navigate({ page: "run", id: wait.runId })}>Open child run</Action>
                    {" · Since "}<Timestamp value={wait.since} />
                  </li>
                ))}
              </ul>
              <p>Links identify recorded runs. A child run may be queued or no longer available.</p>
            </>
          )}
        </>
      )}
    </section>
  );
}
