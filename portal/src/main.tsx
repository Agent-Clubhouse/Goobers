import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { bootstrapPortalTheme } from "./cobrand";
import "./tokens.css";
import "./styles.css";
import "./tables.css";

bootstrapPortalTheme();

const headerTarget =
  new URLSearchParams(window.location.search).get("host") === "fleet"
    ? document.getElementById("portal-header-host") ?? undefined
    : undefined;

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App headerHost={headerTarget ? { target: headerTarget } : undefined} />
  </StrictMode>,
);
