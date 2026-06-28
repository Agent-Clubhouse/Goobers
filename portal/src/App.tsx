import { useEffect, useMemo, useState } from "react";
import { createMockGoobersApi } from "./api/mockClient";
import type { GoobersPortalApi, InstanceSnapshot } from "./api/types";
import { AuthGate } from "./components/AuthGate";
import { HumanGatePanel } from "./components/HumanGatePanel";
import { RosterView } from "./components/RosterView";
import { RunTraceViewer } from "./components/RunTraceViewer";

interface AppProps {
  api?: GoobersPortalApi;
}

export function App({ api }: AppProps) {
  const portalApi = useMemo(() => api ?? createMockGoobersApi(), [api]);
  const [snapshot, setSnapshot] = useState<InstanceSnapshot>();

  useEffect(() => {
    let active = true;
    void portalApi.getInstanceSnapshot().then((nextSnapshot) => {
      if (active) {
        setSnapshot(nextSnapshot);
      }
    });
    return () => {
      active = false;
    };
  }, [portalApi]);

  return (
    <AuthGate>
      <main className="app-shell">
        <header className="hero">
          <div>
            <p className="eyebrow">Goobers Portal</p>
            <h1>Observability window for your AI coding workforce</h1>
            <p>
              Runtime state, Goober-run traces, and human-gate decisions live here. Configuration stays in code.
            </p>
          </div>
          {snapshot && (
            <dl className="instance-card" aria-label="Instance summary">
              <div>
                <dt>Instance</dt>
                <dd>{snapshot.instance.name}</dd>
              </div>
              <div>
                <dt>Environment</dt>
                <dd>{snapshot.instance.environment}</dd>
              </div>
              <div>
                <dt>Health</dt>
                <dd>{snapshot.instance.health}</dd>
              </div>
            </dl>
          )}
        </header>

        {snapshot ? (
          <>
            <RosterView snapshot={snapshot} />
            <RunTraceViewer api={portalApi} />
            <HumanGatePanel api={portalApi} />
          </>
        ) : (
          <section className="panel">Loading portal snapshot...</section>
        )}
      </main>
    </AuthGate>
  );
}
