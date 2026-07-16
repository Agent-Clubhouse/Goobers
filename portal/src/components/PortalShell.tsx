import type { PrimaryArea, Route } from "../routes";
import { useTheme } from "../theme";
import { Icon } from "./Icon";
import { PrimaryNavigation } from "./Navigation";

export function PortalShell({
  activeArea,
  children,
  navigate,
  runCount,
  workflowCount,
}: {
  activeArea: PrimaryArea;
  children: React.ReactNode;
  navigate: (route: Route) => void;
  runCount: number;
  workflowCount: number;
}) {
  const { theme, toggleTheme } = useTheme();

  return (
    <div className="portal-frame">
      <a className="skip-link" href="#main-content">Skip to content</a>
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

        <PrimaryNavigation
          area={activeArea}
          navigate={navigate}
          runCount={runCount}
          workflowCount={workflowCount}
        />

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
            <span className="fixture-label">Static fixtures</span>
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

        <main className="page-content" id="main-content" tabIndex={-1}>
          {children}
        </main>
      </div>
    </div>
  );
}
