import { useEffect, useState } from "react";
import type { GateApprovalRequest, GoobersPortalApi } from "../api/types";

interface HumanGatePanelProps {
  api: GoobersPortalApi;
}

export function HumanGatePanel({ api }: HumanGatePanelProps) {
  const [approvals, setApprovals] = useState<GateApprovalRequest[]>([]);

  useEffect(() => {
    let active = true;
    void api.listGateApprovals().then((requests) => {
      if (active) {
        setApprovals(requests);
      }
    });
    return () => {
      active = false;
    };
  }, [api]);

  const decide = async (requestId: string, decision: "approve" | "reject") => {
    const updated = await api.decideGateApproval(requestId, {
      decision,
      comment: `Mock ${decision} from portal placeholder`,
    });
    setApprovals((current) => current.map((approval) => (approval.id === requestId ? updated : approval)));
  };

  return (
    <section className="panel" aria-labelledby="gates-title">
      <div className="panel-heading">
        <p className="eyebrow">Runtime gates</p>
        <h2 id="gates-title">Human-gate approvals</h2>
        <p>Placeholder approve/reject controls wired to the typed mock API.</p>
      </div>

      {approvals.length === 0 ? (
        <p className="muted">No human approvals are waiting.</p>
      ) : (
        <div className="approval-list">
          {approvals.map((approval) => (
            <article className="approval-card" key={approval.id}>
              <div>
                <span className={`status status-${approval.status}`}>{approval.status}</span>
                <h3>{approval.gateName}</h3>
                <p>{approval.summary}</p>
                <small>
                  Run {approval.runId} · requested by {approval.requestedBy}
                </small>
              </div>
              <div className="button-row">
                <button type="button" disabled={approval.status !== "pending"} onClick={() => void decide(approval.id, "approve")}>
                  Approve
                </button>
                <button
                  className="button-secondary"
                  type="button"
                  disabled={approval.status !== "pending"}
                  onClick={() => void decide(approval.id, "reject")}
                >
                  Reject
                </button>
              </div>
            </article>
          ))}
        </div>
      )}
    </section>
  );
}
