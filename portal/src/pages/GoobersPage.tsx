import { useState } from "react";
import type { DaemonClient, Gaggle, Goober } from "../api/types";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import { RecoveryCommand } from "../components/RecoveryAction";
import { type OperationalSnapshot, useOperationalSnapshot } from "../operationalData";
import { Icon } from "../ui/Icon";

export function GoobersPage({
  client,
  standalone,
}: {
  client: DaemonClient;
  standalone: boolean;
}) {
  const query = useOperationalSnapshot(client);

  if (query.state.status === "loading") {
    return <DaemonLoadingState standalone={standalone} />;
  }
  if (query.state.status === "error") {
    return <DaemonErrorState error={query.state.error} retry={query.retry} standalone={standalone} />;
  }
  if (query.state.status !== "ready" && query.state.status !== "stale") {
    return null;
  }

  return <GooberRoster snapshot={query.state.data} standalone={standalone} />;
}

interface RosterEntry {
  gaggle: Gaggle;
  goober: Goober;
}

function GooberRoster({
  snapshot,
  standalone,
}: {
  snapshot: OperationalSnapshot;
  standalone: boolean;
}) {
  const entries: RosterEntry[] = snapshot.inventories.flatMap((inventory) =>
    inventory.goobers.map((goober) => ({ gaggle: inventory.gaggle, goober })),
  );

  return (
    <>
      <header className="page-heading page-heading-row">
        <div>
          <p className="page-kicker">Roster</p>
          <h1>Goobers</h1>
          <p>
            {standalone
              ? "Every configured agent persona across gaggles, read from this instance."
              : "Every configured agent persona across gaggles, read from the daemon."}
          </p>
        </div>
        <div className="scope-chip">
          <span className="scope-mark">G</span>
          {entries.length} {entries.length === 1 ? "goober" : "goobers"}
        </div>
      </header>

      {entries.length === 0 ? (
        <section className="empty-state">
          <img alt="" src="/goober-mascot.png" />
          <div>
            <h2>No goobers configured</h2>
            <p>No goobers are provisioned in this instance yet. Initialize the instance to begin.</p>
            <RecoveryCommand command="goobers init --guided <instance>" />
          </div>
        </section>
      ) : (
        <div aria-label="Goober roster" className="goober-roster goober-roster-page">
          {entries.map(({ gaggle, goober }) => (
            <GooberRosterCard gaggle={gaggle} goober={goober} key={`${gaggle.name}/${goober.name}`} />
          ))}
        </div>
      )}
    </>
  );
}

function GooberRosterCard({ gaggle, goober }: RosterEntry) {
  const [expanded, setExpanded] = useState(false);
  const [view, setView] = useState<"fields" | "yaml">("fields");
  const headingId = `goober-${gaggle.name}-${goober.name}`;
  const detailId = `${headingId}-detail`;

  return (
    <article className="goober-card goober-roster-card" data-expanded={expanded}>
      <button
        aria-controls={detailId}
        aria-expanded={expanded}
        className="goober-card-toggle"
        onClick={() => setExpanded((value) => !value)}
        type="button"
      >
        <span className="goober-card-toggle-label">
          <h4 id={headingId}>{goober.displayName}</h4>
          <p>
            {gaggle.displayName} · {goober.role}
          </p>
        </span>
        <span className="goober-card-toggle-meta">
          <span className="definition-status">{goober.status}</span>
          <span aria-hidden="true" className="goober-card-chevron">
            <Icon name="chevron" size={14} />
          </span>
        </span>
      </button>

      <dl>
        <div>
          <dt>Harness</dt>
          <dd>{goober.harness}</dd>
        </div>
        <div>
          <dt>Skills</dt>
          <dd>{goober.skills.length > 0 ? goober.skills.join(", ") : "None declared"}</dd>
        </div>
        <div>
          <dt>Workflow ownership</dt>
          <dd>
            {goober.workflows.length > 0
              ? goober.workflows.map((workflow) => `${workflow.gaggle}/${workflow.name}`).join(", ")
              : "None declared"}
          </dd>
        </div>
      </dl>

      {expanded && (
        <div aria-labelledby={headingId} className="goober-detail definition-panel" id={detailId}>
          <div aria-label={`${goober.displayName} config view`} className="definition-view-toggle" role="tablist">
            <button
              aria-selected={view === "fields"}
              className={view === "fields" ? "active" : undefined}
              onClick={() => setView("fields")}
              role="tab"
              type="button"
            >
              Fields
            </button>
            <button
              aria-selected={view === "yaml"}
              className={view === "yaml" ? "active" : undefined}
              onClick={() => setView("yaml")}
              role="tab"
              type="button"
            >
              Raw YAML
            </button>
          </div>
          {view === "fields" ? (
            <dl className="property-list">
              <div>
                <dt>Persona</dt>
                <dd>{goober.role}</dd>
              </div>
              <div>
                <dt>Capabilities</dt>
                <dd>
                  {goober.capabilities.length > 0 ? goober.capabilities.join(", ") : "None declared"}
                </dd>
              </div>
              <div>
                <dt>Stage ownership</dt>
                <dd>
                  {goober.stages.length > 0
                    ? goober.stages
                        .map(
                          (stage) =>
                            `${stage.workflow.gaggle}/${stage.workflow.name}/${stage.stage} (${stage.kind})`,
                        )
                        .join(", ")
                    : "None declared"}
                </dd>
              </div>
              <div>
                <dt>Provisioning</dt>
                <dd>
                  {gaggle.name}/{goober.name} · {goober.harness}
                </dd>
              </div>
              <div>
                <dt>Warnings</dt>
                <dd>
                  {goober.warnings.length > 0
                    ? goober.warnings.map((warning) => warning.explanation).join("; ")
                    : "None"}
                </dd>
              </div>
            </dl>
          ) : (
            <pre className="code-block">{formatGooberYaml(gaggle, goober)}</pre>
          )}
        </div>
      )}
    </article>
  );
}

function formatGooberYaml(gaggle: Gaggle, goober: Goober): string {
  const lines = [
    `name: ${goober.name}`,
    `displayName: ${goober.displayName}`,
    `gaggle: ${gaggle.name}`,
    `role: ${goober.role}`,
    `status: ${goober.status}`,
    `harness: ${goober.harness}`,
    ...yamlList("skills", goober.skills),
    ...yamlList("capabilities", goober.capabilities),
    ...yamlList(
      "workflows",
      goober.workflows.map((workflow) => `${workflow.gaggle}/${workflow.name}`),
    ),
    ...yamlList(
      "stages",
      goober.stages.map(
        (stage) => `${stage.workflow.gaggle}/${stage.workflow.name}/${stage.stage} (${stage.kind})`,
      ),
    ),
  ];
  return `${lines.join("\n")}\n`;
}

function yamlList(key: string, values: string[]): string[] {
  if (values.length === 0) {
    return [`${key}: []`];
  }
  return [`${key}:`, ...values.map((value) => `- ${value}`)];
}
