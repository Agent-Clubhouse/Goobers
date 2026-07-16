import { useEffect, useRef, type ReactNode } from "react";
import { Icon } from "../foundation/Icon";
import {
  activeArea,
  routeHash,
  type Navigate,
  type PrimaryArea,
  type Route,
} from "../foundation/navigation";
import type { Theme } from "../foundation/theme";
import { runs, workflows } from "../prototypeData";

const destinations: Array<{
  area: PrimaryArea;
  icon: "overview" | "workflow" | "run";
  label: string;
}> = [
  { area: "overview", icon: "overview", label: "Overview" },
  { area: "workflows", icon: "workflow", label: "Workflows" },
  { area: "runs", icon: "run", label: "Runs" },
];

export function AppShell({
  children,
  navigate,
  route,
  theme,
  toggleTheme,
}: {
  children: ReactNode;
  navigate: Navigate;
  route: Route;
  theme: Theme;
  toggleTheme: () => void;
}) {
  const area = activeArea(route);
  const mainRef = useRef<HTMLElement>(null);
  const previousRoute = useRef(routeHash(route));

  useEffect(() => {
    const currentRoute = routeHash(route);
    if (previousRoute.current !== currentRoute) {
      mainRef.current?.focus();
    }
    previousRoute.current = currentRoute;
  }, [route]);

  return (
    <div className="portal-frame">
      <button className="skip-link" onClick={() => mainRef.current?.focus()} type="button">
        Skip to content
      </button>
      <aside className="sidebar">
        <button className="brand" onClick={() => navigate({ page: "overview" })} type="button">
          <img alt="" src="/goober-mascot.png" />
          <span>
            <strong>goobers</strong>
            <small>local operations</small>
          </span>
        </button>

        <nav className="primary-nav" aria-label="Primary">
          {destinations.map((destination) => (
            <button
              aria-current={area === destination.area ? "page" : undefined}
              className={area === destination.area ? "nav-item nav-item-active" : "nav-item"}
              key={destination.area}
              onClick={() => navigate({ page: destination.area })}
              type="button"
            >
              <Icon name={destination.icon} />
              {destination.label}
              {destination.area !== "overview" && (
                <span className="nav-count">
                  {destination.area === "workflows" ? workflows.length : runs.length}
                </span>
              )}
            </button>
          ))}
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

        <main className="page-content" id="portal-content" ref={mainRef} tabIndex={-1}>
          {children}
        </main>
      </div>
    </div>
  );
}
