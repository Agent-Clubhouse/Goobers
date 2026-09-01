import { useRef } from "react";
import type { DaemonClient } from "../api/types";
import { useCobrand } from "../cobrand";
import { useLiveData, type DataFreshness, type LiveFreshness } from "../liveData";
import { useGaggleList } from "../operationalData";
import { routeHash, type Navigate, type PrimaryArea } from "../routing";
import { hasScopeIdentity, type ScopeFilters } from "../scope";
import type { Theme } from "../theme";
import { Icon } from "../ui/Icon";
import { SupportFooter } from "./SupportFooter";

interface PortalShellProps {
  activeArea: PrimaryArea;
  activeGaggle?: string;
  children: React.ReactNode;
  client: DaemonClient;
  currentScope: Pick<ScopeFilters, "gaggle" | "workflow" | "stage">;
  navigate: Navigate;
  standalone: boolean;
  theme: Theme;
  toggleTheme: () => void;
}

export function PortalShell({
  activeArea,
  activeGaggle,
  children,
  client,
  currentScope,
  navigate,
  standalone,
  theme,
  toggleTheme,
}: PortalShellProps) {
  // Navigating between Runs and Insight while a gaggle/workflow/stage scope
  // is active preserves it instead of resetting to "all" (#2528 acceptance
  // criterion 4) — outcome/population/window are page-specific refinements
  // and intentionally do not carry across views.
  const scopedFilters = hasScopeIdentity(currentScope) ? currentScope : undefined;
  const { config } = useCobrand();
  const { dataFreshness, freshness } = useLiveData();
  const mainContent = useRef<HTMLElement>(null);

  const skipToMainContent = (event: React.MouseEvent<HTMLAnchorElement>) => {
    event.preventDefault();
    mainContent.current?.focus();
  };

  return (
    <div className="portal-frame">
      <a className="skip-link" href="#main-content" onClick={skipToMainContent}>
        Skip to main content
      </a>
      <aside className="sidebar">
        <button
          aria-label="Go to overview"
          className="brand"
          onClick={() => navigate({ page: "overview" })}
          type="button"
        >
          <img alt="" src={config.brand.logoUrl ?? "/goober-mascot.png"} />
          <span>
            <strong>{config.brand.name}</strong>
            <small>{config.brand.tagline}</small>
          </span>
        </button>

        <nav className="primary-nav" aria-label="Primary">
          <button
            aria-current={activeArea === "overview" ? "page" : undefined}
            aria-label="Overview"
            className={activeArea === "overview" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "overview" })}
            type="button"
          >
            <Icon name="overview" />
            <span className="nav-label">Overview</span>
          </button>
          <button
            aria-current={activeArea === "workflows" ? "page" : undefined}
            aria-label="Workflows"
            className={activeArea === "workflows" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "workflows" })}
            type="button"
          >
            <Icon name="workflow" />
            <span className="nav-label">Workflows</span>
          </button>
          <button
            aria-current={activeArea === "goobers" ? "page" : undefined}
            aria-label="Goobers"
            className={activeArea === "goobers" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "goobers" })}
            type="button"
          >
            <Icon name="goober" />
            <span className="nav-label">Goobers</span>
          </button>
          <button
            aria-current={activeArea === "runs" ? "page" : undefined}
            aria-label="Runs"
            className={activeArea === "runs" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "runs", filters: scopedFilters })}
            type="button"
          >
            <Icon name="run" />
            <span className="nav-label">Runs</span>
          </button>
          <button
            aria-current={activeArea === "insight" ? "page" : undefined}
            aria-label="Insight"
            className={activeArea === "insight" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "insight", filters: scopedFilters })}
            type="button"
          >
            <Icon name="insight" />
            <span className="nav-label">Insight</span>
          </button>
        </nav>

        <GaggleNav activeGaggle={activeGaggle} client={client} navigate={navigate} />

        <div className="sidebar-status">
          <div>
            <span aria-hidden="true" className={`live-mark live-mark-${freshness}`} />
            <span>
              <strong>{standalone ? "Standalone read-only" : "Daemon API"}</strong>
              <small>
                {standalone
                  ? "Daemon not running; reading this instance locally"
                  : freshnessCopy[freshness]}
              </small>
            </span>
          </div>
        </div>
        <SupportFooter />
      </aside>

      <div className="portal-main">
        <header className="topbar">
          <div className="topbar-context">
            <span className="scope-mark">{config.brand.scopeMark}</span>
            <span>
              <strong>{config.brand.name}</strong>
              <small>operations workbench</small>
            </span>
          </div>
          <div className="topbar-actions">
            {/*
              Two indicators, deliberately: how current the DATA is, and whether
              the CONNECTION is up (#1928). Conflating them is why an operator
              could not tell "slow" from "broken" — a stream can be perfectly
              connected to a projector ten minutes behind, and can be
              reconnecting over data current to the second.

              The data indicator comes first because it answers the question the
              user actually has. The connection indicator stays, because a
              dropped stream means the next change will arrive late even if what
              is on screen is current.
            */}
            <DataFreshnessIndicator state={dataFreshness} />
            <span
              aria-live="polite"
              className={`freshness-status freshness-status-${freshness}`}
              data-state={freshness}
              role="status"
              title="Live update connection"
            >
              <span aria-hidden="true" className={`live-mark live-mark-${freshness}`} />
              {freshnessLabel[freshness]}
            </span>
            <button
              aria-label={`Use ${theme === "light" ? "dark" : "light"} theme`}
              className="theme-button"
              onClick={toggleTheme}
              type="button"
            >
              <Icon name={theme === "light" ? "moon" : "sun"} size={17} />
            </button>
          </div>
        </header>

        <main className="page-content" id="main-content" ref={mainContent} tabIndex={-1}>
          {children}
        </main>
      </div>
    </div>
  );
}

/**
 * Persistent gaggle affordance (#2531): a gaggle must be reachable directly
 * from anywhere in the app, not only by drilling down through Workflows. It
 * doubles as the switcher on multi-gaggle instances since the sidebar
 * carries it on every page, including the gaggle view itself.
 */
function GaggleNav({
  activeGaggle,
  client,
  navigate,
}: {
  activeGaggle?: string;
  client: DaemonClient;
  navigate: Navigate;
}) {
  const { state } = useGaggleList(client);
  const gaggles =
    state.status === "ready" || state.status === "stale" ? state.data : undefined;

  if (!gaggles || gaggles.length === 0) {
    return null;
  }

  return (
    <nav aria-label="Gaggles" className="sidebar-gaggle-nav">
      <span className="sidebar-section-label">Gaggles</span>
      <ul>
        {gaggles.map((gaggle) => (
          <li key={gaggle.name}>
            <a
              aria-current={activeGaggle === gaggle.name ? "page" : undefined}
              aria-label={`Open gaggle ${gaggle.displayName}`}
              className={
                activeGaggle === gaggle.name ? "nav-item nav-item-active" : "nav-item"
              }
              href={routeHash({ page: "gaggle", id: gaggle.name })}
              onClick={(event) => {
                event.preventDefault();
                navigate({ page: "gaggle", id: gaggle.name });
              }}
            >
              <Icon name="gaggle" />
              <span className="nav-label">{gaggle.displayName}</span>
            </a>
          </li>
        ))}
      </ul>
    </nav>
  );
}

const freshnessLabel: Record<LiveFreshness, string> = {
  connected: "Live updates connected",
  reconnecting: "Reconnecting",
  stale: "Data stale",
  offline: "Offline",
  "polling-fallback": "Polling fallback",
};

const freshnessCopy: Record<LiveFreshness, string> = {
  connected: "Live updates connected",
  reconnecting: "Reconnecting; showing stale data",
  stale: "Refreshing a full snapshot",
  offline: "Offline; showing stale data",
  "polling-fallback": "SSE unavailable; polling",
};

/**
 * Renders how current the data is.
 *
 * Accessibility: the state is never conveyed by colour alone (§11A). Each state
 * carries distinct TEXT, and the mark is decorative (`aria-hidden`) so a screen
 * reader announces the words rather than a dot. `role="status"` with
 * `aria-live="polite"` announces transitions without interrupting.
 */
export function DataFreshnessIndicator({ state }: { state: DataFreshness }) {
  if (state.kind === "unknown") {
    // No envelope: a standalone read, or a daemon with no read model attached.
    // Rendering "current" here would be a claim nobody made.
    return null;
  }
  return (
    <span
      aria-live="polite"
      className={`data-freshness data-freshness-${state.kind}`}
      data-state={state.kind}
      role="status"
      title={dataFreshnessTitle(state)}
    >
      <span aria-hidden="true" className={`data-mark data-mark-${state.kind}`} />
      {dataFreshnessLabel(state)}
    </span>
  );
}

function dataFreshnessLabel(state: DataFreshness): string {
  switch (state.kind) {
    case "current":
      return "Data current";
    case "lagging":
      return `Data stale by ${formatLag(state.lagSeconds)}`;
    case "partial":
      return `Partial — ${state.missing.map((entry) => entry.name).join(", ")}`;
    default:
      return "";
  }
}

function dataFreshnessTitle(state: DataFreshness): string {
  switch (state.kind) {
    case "current":
      return `Read model is current (within ${formatLag(state.lagSeconds)})`;
    case "lagging":
      return state.degraded.length > 0
        ? `Behind by up to ${formatLag(state.lagSeconds)}: ${state.degraded.join(", ")}`
        : `Behind by up to ${formatLag(state.lagSeconds)}`;
    case "partial":
      // The expiry is the difference between a useful partial and wallpaper: it
      // tells the user whether to wait or to investigate.
      return state.missing
        .map((entry) => `${entry.name}: ${entry.reason} (expected by ${entry.expectedBy})`)
        .join("; ");
    default:
      return "";
  }
}

/** Whole seconds under a minute, whole minutes above — a lag readout does not
 *  need sub-second precision and reads worse with it. */
function formatLag(seconds: number): string {
  if (seconds < 60) {
    return `${Math.max(0, Math.round(seconds))}s`;
  }
  const minutes = Math.round(seconds / 60);
  return minutes < 60 ? `${minutes}m` : `${Math.round(minutes / 60)}h`;
}
