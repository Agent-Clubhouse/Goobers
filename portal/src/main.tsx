import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { plantProbeRequested } from "./plantProbeSink";
import "./tokens.css";
import "./styles.css";

function renderPortal() {
  createRoot(document.getElementById("root")!).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}

if (plantProbeRequested(window.location.search)) {
  void import("./plantProbe")
    .then(({ installPlantProbe }) => {
      installPlantProbe();
    })
    .catch(() => {
      // The measurement probe is optional; its chunk must never gate the portal.
    })
    .finally(renderPortal);
} else {
  renderPortal();
}
