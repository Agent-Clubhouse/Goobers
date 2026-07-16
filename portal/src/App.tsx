import { useEffect, useState } from "react";
import { AppShell } from "./components/AppShell";
import { parseRoute, routeHash, type Route } from "./foundation/navigation";
import { useTheme } from "./foundation/theme";
import { RunsPage, WorkflowsPage } from "./pages/InventoryPages";
import { OverviewPage } from "./pages/OverviewPage";
import { RunPage } from "./pages/RunPage";
import { WorkflowPage } from "./pages/WorkflowPage";
import { runs, workflows } from "./prototypeData";

export function App() {
  const [route, setRoute] = useState<Route>(() => parseRoute());
  const { theme, toggleTheme } = useTheme();

  useEffect(() => {
    const onHashChange = () => setRoute(parseRoute());
    window.addEventListener("hashchange", onHashChange);
    return () => window.removeEventListener("hashchange", onHashChange);
  }, []);

  const navigate = (nextRoute: Route) => {
    const nextHash = routeHash(nextRoute);
    if (window.location.hash === nextHash) {
      setRoute(nextRoute);
    } else {
      window.location.hash = nextHash;
    }
  };

  const run = route.page === "run" ? runs.find((candidate) => candidate.id === route.id) : undefined;
  const workflow =
    route.page === "workflow" ? workflows.find((candidate) => candidate.id === route.id) : undefined;

  return (
    <AppShell
      navigate={navigate}
      route={route}
      theme={theme}
      toggleTheme={toggleTheme}
    >
      {route.page === "overview" && <OverviewPage navigate={navigate} />}
      {route.page === "workflows" && <WorkflowsPage navigate={navigate} />}
      {route.page === "runs" && <RunsPage navigate={navigate} />}
      {route.page === "workflow" && workflow && <WorkflowPage navigate={navigate} workflow={workflow} />}
      {route.page === "run" && run && <RunPage key={run.id} navigate={navigate} run={run} />}
      {route.page === "workflow" && !workflow && <p role="alert">Workflow not found.</p>}
      {route.page === "run" && !run && <p role="alert">Run not found.</p>}
    </AppShell>
  );
}
