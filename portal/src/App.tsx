import { useEffect, useState } from "react";
import { AppShell } from "./foundation/AppShell";
import { useTheme } from "./foundation/theme";
import { OverviewPage } from "./pages/OverviewPage";
import { RunPage } from "./pages/RunPage";
import { RunsPage } from "./pages/RunsPage";
import { WorkflowPage } from "./pages/WorkflowPage";
import { WorkflowsPage } from "./pages/WorkflowsPage";
import { runs, workflows } from "./fixtures";
import { activeArea, parseRoute, routeHash, type Route } from "./routes";

export function App() {
  const [route, setRoute] = useState<Route>(() => parseRoute());
  const [theme, setTheme] = useTheme();

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
      area={activeArea(route)}
      navigate={navigate}
      onToggleTheme={() => setTheme((current) => current === "light" ? "dark" : "light")}
      runCount={runs.length}
      theme={theme}
      workflowCount={workflows.length}
    >
      {route.page === "overview" && <OverviewPage navigate={navigate} />}
      {route.page === "workflows" && <WorkflowsPage navigate={navigate} />}
      {route.page === "runs" && <RunsPage navigate={navigate} />}
      {route.page === "workflow" && workflow && <WorkflowPage navigate={navigate} workflow={workflow} />}
      {route.page === "run" && run && <RunPage key={run.id} navigate={navigate} run={run} />}
      {route.page === "workflow" && !workflow && <h1>Workflow not found.</h1>}
      {route.page === "run" && !run && <h1>Run not found.</h1>}
    </AppShell>
  );
}
