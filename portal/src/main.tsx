import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { bootstrapPortalTheme } from "./cobrand";
import "./tokens.css";
import "./styles.css";
import "./tables.css";

bootstrapPortalTheme();

const searchParams = new URLSearchParams(window.location.search);
const headerTarget =
  searchParams.get("host") === "fleet"
    ? document.getElementById("portal-header-host") ?? undefined
    : undefined;
const hostProvidesMobileNavigation =
  headerTarget !== undefined && searchParams.get("hostNavigation") === "true";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App
      headerHost={
        headerTarget
          ? { target: headerTarget, providesMobileNavigation: hostProvidesMobileNavigation }
          : undefined
      }
    />
    {hostProvidesMobileNavigation && (
      <nav aria-label="Host mobile primary" className="mobile-primary-nav">
        <button className="mobile-nav-item mobile-nav-item-active" type="button">
          <span>Portal</span>
        </button>
      </nav>
    )}
  </StrictMode>,
);
