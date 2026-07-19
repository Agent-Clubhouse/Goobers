import { useEffect, useRef, useState } from "react";
import { HttpDaemonClient } from "./api/httpClient";
import type { DaemonClient } from "./api/types";
import { LiveDataProvider } from "./liveData";
import { OverviewPage } from "./pages/OverviewPage";
import { RunPage } from "./pages/RunPage";
import { RunsPage } from "./pages/RunsPage";
import { WorkflowPage } from "./pages/WorkflowPage";
import { WorkflowsPage } from "./pages/WorkflowsPage";
import { runs, workflows } from "./prototypeData";
import { activeArea, parseRoute, routeHash, type Route } from "./routing";
import { PortalShell } from "./shell/PortalShell";

const daemonClient = new HttpDaemonClient();

export function App({ client = daemonClient }: { client?: DaemonClient }) {
  return (
    <LiveDataProvider client={client}>
      <Portal client={client} />
    </LiveDataProvider>
  );
}

function Portal({ client }: { client: DaemonClient }) {
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

  const navigate = (nextRoute: Route) => {
    const nextHash = routeHash(nextRoute);
    if (window.location.hash === nextHash) {
      setRoute(nextRoute);
    } else {
      window.location.hash = nextHash;
    }
  };

  const run =
    route.page === "run" ? runs.find((candidate) => candidate.id === route.id) : undefined;
  const workflow =
    route.page === "workflow"
      ? workflows.find((candidate) => candidate.id === route.id)
      : undefined;

  return (
    <PortalShell activeArea={activeArea(route)} navigate={navigate}>
      {route.page === "overview" && <OverviewPage client={client} />}
      {route.page === "workflows" && <WorkflowsPage client={client} />}
      {route.page === "runs" && <RunsPage navigate={navigate} />}
      {route.page === "workflow" && workflow && (
        <WorkflowPage navigate={navigate} workflow={workflow} />
      )}
      {route.page === "run" && run && <RunPage key={run.id} navigate={navigate} run={run} />}
      {route.page === "workflow" && !workflow && <p role="alert">Workflow not found.</p>}
      {route.page === "run" && !run && <p role="alert">Run not found.</p>}
    </PortalShell>
  );
}
