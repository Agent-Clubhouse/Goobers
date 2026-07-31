import { useEffect, useRef, useState } from "react";
import { publishReadState } from "./liveData";
import { HttpDaemonClient } from "./api/httpClient";
import { bindUIActions } from "./api/surfaceActions";
import type { DaemonClient, PortalConfig, ValidationWarning } from "./api/types";
import { applyThemeOverrides, CobrandContext, defaultPortalConfig } from "./cobrand";
import {
  type ConfigurationWarningClient,
  type ConfigurationWarningSource,
  useConfigurationWarnings,
} from "./configurationWarnings";
import { LiveDataProvider } from "./liveData";
import {
  createPortalDiagnostics,
  type PortalDiagnostics,
} from "./portalDiagnostics";
import { ErrorsPage } from "./pages/ErrorsPage";
import { GagglePage } from "./pages/GagglePage";
import { OverviewPage } from "./pages/OverviewPage";
import { InsightPage } from "./pages/InsightPage";
import { RunPage } from "./pages/RunPage";
import { RunsPage } from "./pages/RunsPage";
import { WorkflowPage } from "./pages/WorkflowPage";
import { WorkflowsPage } from "./pages/WorkflowsPage";
import { instanceWarnings } from "./prototypeData";
import { activeArea, parseRoute, routeHash, type Route } from "./routing";
import { PortalShell } from "./shell/PortalShell";
import { useTheme } from "./theme";

const portalDiagnostics = createPortalDiagnostics();
const daemonClient = new HttpDaemonClient({
  diagnostics: portalDiagnostics,
  onReadState: publishReadState,
});
const noWarnings: readonly ValidationWarning[] = [];

// Warning reads are their own seam, defaulting to the daemon client: in
// production both read the same daemon, but keeping them separate means a
// failed warning read degrades only the warning surface instead of blanking
// the operational page, and tests can drive either independently.
export function App({
  client = daemonClient,
  warningClient = client,
  diagnostics = portalDiagnostics,
}: {
  client?: DaemonClient;
  warningClient?: ConfigurationWarningClient;
  diagnostics?: PortalDiagnostics;
} = {}) {
  const standalone =
    document
      .querySelector('meta[name="goobers-dashboard-mode"]')
      ?.getAttribute("content") === "standalone";

  return (
    <LiveDataProvider client={client} diagnostics={diagnostics}>
      <Portal client={client} standalone={standalone} warningClient={warningClient} />
    </LiveDataProvider>
  );
}

function Portal({
  client,
  standalone,
  warningClient,
}: {
  client: DaemonClient;
  standalone: boolean;
  warningClient: ConfigurationWarningClient;
}) {
  const { theme, toggleTheme } = useTheme();
  const [route, setRoute] = useState<Route>(() => parseRoute());
  const [config, setConfig] = useState<PortalConfig>(defaultPortalConfig);
  const [loading, setLoading] = useState(true);
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

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    void client
      .getPortalConfig()
      .then((nextConfig) => {
        if (cancelled) return;
        setConfig(nextConfig);
      })
      .catch(() => {
        if (cancelled) return;
        setConfig(defaultPortalConfig);
      })
      .finally(() => {
        if (cancelled) return;
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [client]);

  useEffect(() => {
    applyThemeOverrides(config, theme);
    document.title = config.brand.name;

    let icon = document.querySelector('link[rel~="icon"]') as HTMLLinkElement | null;
    if (config.brand.faviconUrl) {
      if (!icon) {
        icon = document.createElement("link");
        icon.rel = "icon";
        document.head.appendChild(icon);
      }
      icon.dataset.cobrand = "true";
      icon.href = config.brand.faviconUrl;
      return;
    }
    if (icon?.dataset.cobrand === "true") {
      icon.remove();
    }
  }, [config, theme]);

  // approve/override/rerun (HITL-7/#469): registered now so the UI surface
  // participates in the CLI/API/UI runtime-mutation parity check alongside
  // the daemon's stub routes, but each handler is itself a stub — no
  // intervention UI exists yet, and the underlying gate-resolution/rerun
  // wiring is #466/#468's scope, not this seam's.
  const { navigate } = bindUIActions({
    navigate: (nextRoute: Route) => {
      const nextHash = routeHash(nextRoute);
      if (window.location.hash === nextHash) {
        setRoute(nextRoute);
      } else {
        window.location.hash = nextHash;
      }
    },
    approve: () => {
      console.warn("goobers: approve is not implemented yet (HITL-7/#469)");
    },
    override: () => {
      console.warn("goobers: override is not implemented yet (HITL-7/#469)");
    },
    rerun: () => {
      console.warn("goobers: rerun is not implemented yet (HITL-7/#469)");
    },
  });

  let warningSource: ConfigurationWarningSource = { kind: "none" };
  let warningFixtures = noWarnings;
  if (route.page === "overview") {
    warningSource = { kind: "instance" };
    warningFixtures = instanceWarnings;
  } else if (route.page === "workflow" && route.gaggle) {
    warningSource = {
      kind: "workflow",
      gaggle: route.gaggle,
      workflow: route.id,
    };
  }
  const configurationWarnings = useConfigurationWarnings(
    warningClient,
    warningSource,
    warningFixtures,
  );

  return (
    <CobrandContext.Provider value={{ config, loading }}>
      <PortalShell
        activeArea={activeArea(route)}
        navigate={navigate}
        standalone={standalone}
        theme={theme}
        toggleTheme={toggleTheme}
      >
        {route.page === "overview" && (
          <OverviewPage
            client={client}
            configurationWarnings={configurationWarnings}
            standalone={standalone}
          />
        )}
        {route.page === "workflows" && <WorkflowsPage client={client} standalone={standalone} />}
        {route.page === "gaggle" && (
          <GagglePage
            client={client}
            gaggleName={route.id}
            navigate={navigate}
            standalone={standalone}
          />
        )}
        {route.page === "runs" && (
          <RunsPage client={client} filters={route.filters} standalone={standalone} />
        )}
        {route.page === "insight" && <InsightPage client={client} standalone={standalone} />}
        {route.page === "errors" && (
          <ErrorsPage
            client={client}
            filters={route.filters}
            key={routeHash(route)}
            standalone={standalone}
          />
        )}
        {route.page === "workflow" && route.gaggle && (
          <WorkflowPage
            client={client}
            configurationWarnings={configurationWarnings}
            gaggle={route.gaggle}
            key={`${route.gaggle}/${route.id}`}
            navigate={navigate}
            standalone={standalone}
            workflowName={route.id}
          />
        )}
        {route.page === "run" && (
          <RunPage
            client={client}
            key={route.id}
            navigate={navigate}
            runId={route.id}
            standalone={standalone}
          />
        )}
        {route.page === "workflow" && !route.gaggle && (
          <p role="alert">Workflow routes require both a gaggle and workflow name.</p>
        )}
      </PortalShell>
    </CobrandContext.Provider>
  );
}
