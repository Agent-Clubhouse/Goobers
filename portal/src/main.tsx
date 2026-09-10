import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { bootstrapPortalTheme } from "./cobrand";
import "./tokens.css";
import "./styles.css";
import "./tables.css";

bootstrapPortalTheme();

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
