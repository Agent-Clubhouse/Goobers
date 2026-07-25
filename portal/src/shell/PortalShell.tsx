import { useRef } from "react";
import { useCobrand } from "../cobrand";
import { useLiveData, type LiveFreshness } from "../liveData";
import type { Navigate, PrimaryArea } from "../routing";
import type { Theme } from "../theme";
import { Icon } from "../ui/Icon";
import { SupportFooter } from "./SupportFooter";

interface PortalShellProps {
  activeArea: PrimaryArea;
  children: React.ReactNode;
  navigate: Navigate;
  standalone: boolean;
  theme: Theme;
  toggleTheme: () => void;
}

export function PortalShell({
  activeArea,
  children,
  navigate,
  standalone,
  theme,
  toggleTheme,
}: PortalShellProps) {
  const { config } = useCobrand();
  const { freshness } = useLiveData();
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
            aria-current={activeArea === "runs" ? "page" : undefined}
            aria-label="Runs"
            className={activeArea === "runs" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "runs" })}
            type="button"
          >
            <Icon name="run" />
            <span className="nav-label">Runs</span>
          </button>
          <button
            aria-current={activeArea === "insight" ? "page" : undefined}
            aria-label="Insight"
            className={activeArea === "insight" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "insight" })}
            type="button"
          >
            <Icon name="insight" />
            <span className="nav-label">Insight</span>
          </button>
        </nav>

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
            <span
              aria-live="polite"
              className={`freshness-status freshness-status-${freshness}`}
              data-state={freshness}
              role="status"
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
