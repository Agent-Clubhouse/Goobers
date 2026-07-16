import type { PrimaryArea, Route } from "../routes";
import { Icon } from "./Icon";
import type { Theme } from "./theme";
import { useMediaQuery } from "./useMediaQuery";

export function AppShell({
  area,
  children,
  navigate,
  onToggleTheme,
  runCount,
  theme,
  workflowCount,
}: {
  area: PrimaryArea;
  children: React.ReactNode;
  navigate: (route: Route) => void;
  onToggleTheme: () => void;
  runCount: number;
  theme: Theme;
  workflowCount: number;
}) {
  const compact = useMediaQuery("(max-width: 820px)");

  return (
    <div className="portal-frame" data-layout={compact ? "compact" : "desktop"}>
      <a
        className="skip-link"
        href="#portal-content"
        onClick={(event) => {
          event.preventDefault();
          document.getElementById("portal-content")?.focus();
        }}
      >
        Skip to content
      </a>
      <aside className="sidebar">
        <button
          aria-label="Go to overview"
          className="brand"
          onClick={() => navigate({ page: "overview" })}
          type="button"
        >
          <img alt="" src="/goober-mascot.png" />
          <span>
            <strong>goobers</strong>
            <small>local operations</small>
          </span>
        </button>

        <nav className="primary-nav" aria-label="Primary">
          <button
            aria-label="Overview"
            aria-current={area === "overview" ? "page" : undefined}
            className={area === "overview" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "overview" })}
            type="button"
          >
            <Icon name="overview" />
            <span className="nav-label">Overview</span>
          </button>
          <button
            aria-label="Workflows"
            aria-current={area === "workflows" ? "page" : undefined}
            className={area === "workflows" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "workflows" })}
            type="button"
          >
            <Icon name="workflow" />
            <span className="nav-label">Workflows</span>
            <span className="nav-count">{workflowCount}</span>
          </button>
          <button
            aria-label="Runs"
            aria-current={area === "runs" ? "page" : undefined}
            className={area === "runs" ? "nav-item nav-item-active" : "nav-item"}
            onClick={() => navigate({ page: "runs" })}
            type="button"
          >
            <Icon name="run" />
            <span className="nav-label">Runs</span>
            <span className="nav-count">{runCount}</span>
          </button>
        </nav>

        <div className="sidebar-status">
          <div>
            <span aria-hidden="true" className="live-mark" />
            <span>
              <strong>local-dev</strong>
              <small>127.0.0.1 · connected</small>
            </span>
          </div>
          <span className="version">v0.6</span>
        </div>
      </aside>

      <div className="portal-main">
        <header className="topbar">
          <div className="topbar-context">
            <span className="scope-mark">G</span>
            <span>
              <strong>goobers</strong>
              <small>1 gaggle · 4 goobers</small>
            </span>
          </div>
          <div className="topbar-actions">
            <span className="fixture-label">Fixture data</span>
            <button
              aria-label={`Use ${theme === "light" ? "dark" : "light"} theme`}
              className="theme-button"
              onClick={onToggleTheme}
              type="button"
            >
              <Icon name={theme === "light" ? "moon" : "sun"} size={17} />
            </button>
          </div>
        </header>

        <main className="page-content" id="portal-content" tabIndex={-1}>
          {children}
        </main>
      </div>
    </div>
  );
}
