import type { InstanceSnapshot } from "../api/types";
import { EmptyState } from "./EmptyState";

interface RosterViewProps {
  snapshot: InstanceSnapshot;
}

export function RosterView({ snapshot }: RosterViewProps) {
  if (snapshot.gaggles.length === 0) {
    return <EmptyState />;
  }

  return (
    <section className="panel" aria-labelledby="roster-title">
      <div className="panel-heading">
        <p className="eyebrow">Workforce roster</p>
        <h2 id="roster-title">Gaggles, goobers, and workflows</h2>
      </div>
      <div className="gaggle-grid">
        {snapshot.gaggles.map((gaggle) => (
          <article className="gaggle-card" key={gaggle.id}>
            <div>
              <h3>{gaggle.name}</h3>
              <p>{gaggle.description}</p>
            </div>
            <span className={`status status-${gaggle.health}`}>{gaggle.health}</span>

            <div className="roster-columns">
              <div>
                <h4>Goobers</h4>
                <ul>
                  {gaggle.goobers.map((goober) => (
                    <li key={goober.id}>
                      <strong>{goober.name}</strong>
                      <span>
                        {goober.role} · scale {goober.scale} · {goober.status}
                      </span>
                      <small>{goober.skills.join(", ")}</small>
                    </li>
                  ))}
                </ul>
              </div>
              <div>
                <h4>Workflows</h4>
                <ul>
                  {gaggle.workflows.map((workflow) => (
                    <li key={workflow.id}>
                      <strong>{workflow.name}</strong>
                      <span>
                        {workflow.trigger} trigger · {workflow.status}
                      </span>
                      <small>{workflow.steps.map((step) => step.name).join(" -> ")}</small>
                    </li>
                  ))}
                </ul>
              </div>
            </div>
          </article>
        ))}
      </div>
    </section>
  );
}
