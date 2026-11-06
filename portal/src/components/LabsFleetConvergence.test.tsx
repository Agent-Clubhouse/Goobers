import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { LabsFleetConvergenceView } from "../api/types";
import { LiveDataProvider } from "../liveData";
import { LabsPage } from "../pages/LabsPage";
import { emptyDaemonFixtures } from "../test/daemonFixtures";

const fleetView: LabsFleetConvergenceView = {
  status: "clustered",
  featureVersion: "workflowcluster.structural/v1",
  workflows: 7,
  population: 3,
  minClusterSize: 3,
  instances: [
    {
      instanceId: "east",
      goobersVersion: "1.2.3",
      workflows: 4,
      shapes: 2,
      aligned: 3,
      private: 1,
      outliers: 0,
    },
    {
      instanceId: "west",
      workflows: 3,
      shapes: 2,
      aligned: 2,
      private: 0,
      outliers: 1,
    },
  ],
  excluded: [
    { instanceId: "old", reason: "feature version v0; fleet clustered v1" },
    { instanceId: "offline", reason: "offline" },
    { instanceId: "restricted", reason: "not granted" },
  ],
  shapes: [
    { shape: "shared-shape", cluster: "c0", workflows: 5, instances: ["east", "west"] },
    { shape: "east-shape", cluster: "c1", workflows: 1, instances: ["east"] },
    { shape: "west-outlier", workflows: 1, instances: ["west"] },
  ],
  clusters: [
    {
      id: "c0",
      medoidShape: "shared-shape",
      shapes: 1,
      workflows: 5,
      instances: 2,
      stability: 0.9,
      stable: true,
    },
    {
      id: "c1",
      medoidShape: "east-shape",
      shapes: 1,
      workflows: 1,
      instances: 1,
      stability: 0.8,
      stable: true,
    },
  ],
  map: [
    { id: "shared-shape", x: 0, y: 0, cluster: "c0" },
    { id: "east-shape", x: 1, y: 0, cluster: "c1" },
    { id: "west-outlier", x: -1, y: 0 },
  ],
};

function renderPage(fleet: boolean) {
  const client = new FixtureDaemonClient({
    ...emptyDaemonFixtures(),
    labsFleetConvergence: fleetView,
  });
  return render(
    <LiveDataProvider client={client}>
      <LabsPage client={client} feature="workflow-clustering" fleet={fleet} />
    </LiveDataProvider>,
  );
}

describe("LabsFleetConvergence", () => {
  it("renders fleet clusters, exclusions, and selected-instance differences", async () => {
    renderPage(true);

    expect(await screen.findByRole("heading", { name: "Fleet convergence" })).toBeInTheDocument();
    const table = screen.getByRole("table", { name: "Fleet clusters" });
    expect(within(table).getByText("shared-shape")).toBeInTheDocument();
    expect(within(table).getByText("5")).toBeInTheDocument();
    expect(within(table).getByText("2")).toBeInTheDocument();

    const excluded = screen.getByRole("region", { name: "Excluded instances" });
    expect(excluded).toHaveTextContent("old: feature version v0; fleet clustered v1");
    expect(excluded).toHaveTextContent("offline: offline");
    expect(excluded).toHaveTextContent("restricted: not granted");

    const detail = screen.getByRole("region", { name: "How this instance differs from the fleet" });
    expect(within(detail).getByText("Aligned").nextElementSibling).toHaveTextContent("3");
    expect(within(detail).getByText("Private").nextElementSibling).toHaveTextContent("1");
    expect(within(detail).getByText("Outliers").nextElementSibling).toHaveTextContent("0");
    expect(within(detail).getByText("Clusters only this instance contributes to").nextElementSibling).toHaveTextContent("c1");
  });

  it("dims map points that do not match the instance filter", async () => {
    const user = userEvent.setup();
    renderPage(true);

    const map = await screen.findByRole("img", { name: "Scatter map of 3 workflows in 2 clusters" });
    const westOutlier = [...map.querySelectorAll("a.labs-map-dot")].find(
      (dot) => dot.querySelector("title")?.textContent === "west-outlier — noise",
    );
    expect(westOutlier).not.toHaveClass("labs-map-dot-dimmed");

    await user.click(screen.getByRole("checkbox", { name: "west" }));

    expect(westOutlier).toHaveClass("labs-map-dot-dimmed");
    const shared = [...map.querySelectorAll("a.labs-map-dot")].find(
      (dot) => dot.querySelector("title")?.textContent?.startsWith("shared-shape"),
    );
    expect(shared).not.toHaveClass("labs-map-dot-dimmed");
  });

  it("leaves daemon mode on the single-instance Labs view", async () => {
    renderPage(false);

    expect(await screen.findByRole("heading", { name: "Not enough workflows to cluster" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Fleet convergence" })).not.toBeInTheDocument();
  });
});
