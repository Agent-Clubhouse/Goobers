import { useRef } from "react";
import type { Navigate, PrimaryArea } from "../routing";
import { useTheme } from "../theme";
import { Icon } from "../ui/Icon";

interface PortalShellProps {
  activeArea: PrimaryArea;
  children: React.ReactNode;
  navigate: Navigate;
  standalone: boolean;
}

export function PortalShell({ activeArea, children, navigate, standalone }: PortalShellProps) {
  const { theme, toggleTheme } = useTheme();
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
          <img alt="" src="/goober-mascot.png" />
          <span>
            <strong>goobers</strong>
            <small>local operations</small>
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
        </nav>

        <div className="sidebar-status">
          <div>
            <span>
              <strong>{standalone ? "Standalone read-only" : "Daemon API"}</strong>
              <small>
                {standalone
                  ? "Daemon not running; reading this instance locally"
                  : "Connection state appears in each live view"}
              </small>
            </span>
          </div>
        </div>
      </aside>

      <div className="portal-main">
        <header className="topbar">
          <div className="topbar-context">
            <span className="scope-mark">G</span>
            <span>
              <strong>goobers</strong>
              <small>operations workbench</small>
            </span>
          </div>
          <div className="topbar-actions">
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
