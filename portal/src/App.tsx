import { useEffect, useRef, useState } from "react";
import { HttpDaemonClient } from "./api/httpClient";
import { bindUIActions } from "./api/surfaceActions";
import type { DaemonClient, ValidationWarning } from "./api/types";
import {
  type ConfigurationWarningClient,
  type ConfigurationWarningSource,
  useConfigurationWarnings,
} from "./configurationWarnings";
import { OverviewPage } from "./pages/OverviewPage";
import { RunPage } from "./pages/RunPage";
import { RunsPage } from "./pages/RunsPage";
import { WorkflowPage } from "./pages/WorkflowPage";
import { WorkflowsPage } from "./pages/WorkflowsPage";
import { instanceWarnings, runs, workflowWarnings, workflows } from "./prototypeData";
import { activeArea, parseRoute, routeHash, type Route } from "./routing";
import { PortalShell } from "./shell/PortalShell";

const daemonClient = new HttpDaemonClient();
const noWarnings: readonly ValidationWarning[] = [];

// Warning reads are their own seam, defaulting to the daemon client: in
// production both read the same daemon, but keeping them separate means a
// failed warning read degrades only the warning surface instead of blanking
// the operational page, and tests can drive either independently.
export function App({
  client = daemonClient,
  warningClient = client,
}: { client?: DaemonClient; warningClient?: ConfigurationWarningClient } = {}) {
  const [route, setRoute] = useState<Route>(() => parseRoute());
  const initialRoute = useRef(true);

  useEffect(() => {
    const onHashChange = () => setRoute(parseRoute());
    window.addEventListener("hashchange", onHashChange);
    return () => window.removeEventListener("hashchange", onHashChange);
  }, []);

  useEffect(() => {
    if (initialRoute.current) {
      initialRoute.current = false;
      return;
    }
    document.getElementById("main-content")?.focus();
  }, [route]);

  const { navigate } = bindUIActions({
    navigate: (nextRoute: Route) => {
      const nextHash = routeHash(nextRoute);
      if (window.location.hash === nextHash) {
        setRoute(nextRoute);
      } else {
        window.location.hash = nextHash;
      }
    },
  });

  const run =
    route.page === "run" ? runs.find((candidate) => candidate.id === route.id) : undefined;
  const workflow =
    route.page === "workflow"
      ? workflows.find((candidate) => candidate.id === route.id)
      : undefined;
  let warningSource: ConfigurationWarningSource = { kind: "none" };
  let warningFixtures = noWarnings;
  if (route.page === "overview") {
    warningSource = { kind: "instance" };
    warningFixtures = instanceWarnings;
  } else if (route.page === "workflow" && workflow) {
    warningSource = {
      kind: "workflow",
      gaggle: workflow.gaggle,
      workflow: workflow.id,
    };
    warningFixtures = workflowWarnings[workflow.id] ?? noWarnings;
  }
  const configurationWarnings = useConfigurationWarnings(
    warningClient,
    warningSource,
    warningFixtures,
  );

  return (
    <PortalShell activeArea={activeArea(route)} navigate={navigate}>
      {route.page === "overview" && (
        <OverviewPage client={client} configurationWarnings={configurationWarnings} />
      )}
      {route.page === "workflows" && <WorkflowsPage client={client} />}
      {route.page === "runs" && <RunsPage navigate={navigate} />}
      {route.page === "workflow" && workflow && (
        <WorkflowPage
          configurationWarnings={configurationWarnings}
          navigate={navigate}
          workflow={workflow}
        />
      )}
      {route.page === "run" && run && <RunPage key={run.id} navigate={navigate} run={run} />}
      {route.page === "workflow" && !workflow && <p role="alert">Workflow not found.</p>}
      {route.page === "run" && !run && <p role="alert">Run not found.</p>}
    </PortalShell>
  );
}
