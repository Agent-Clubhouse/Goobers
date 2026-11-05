import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { LabsWorkflowClusterView } from "../api/types";
import { LiveDataProvider } from "../liveData";
import { emptyDaemonFixtures } from "../test/daemonFixtures";
import { LabsPage } from "./LabsPage";

function fixtureClient(labsWorkflowClusters?: LabsWorkflowClusterView) {
  return new FixtureDaemonClient({ ...emptyDaemonFixtures(), labsWorkflowClusters });
}

function renderLabs(client: FixtureDaemonClient, feature?: string) {
  return render(
    <LiveDataProvider client={client}>
      <LabsPage client={client} feature={feature} />
    </LiveDataProvider>,
  );
}

const clustered: LabsWorkflowClusterView = {
  status: "clustered",
  population: 5,
  minClusterSize: 3,
  model: {
    version: "workflowcluster.model/v1",
    id: "sha256:0123456789abcdef0123",
    featureVersion: "workflowcluster.structural/v1",
    options: { minClusterSize: 3, resamples: 50, stabilityThreshold: 0.6 },
    population: 5,
    clusters: [
      {
        id: "c0",
        medoid: { id: "core/implement" },
        members: ["core/review", "core/implement", "ops/triage"],
        radius: 0.12,
        stability: 0.91,
        stable: true,
        distinguishing: [
          { feature: "nodes.gate", effect: 0.75 },
          { feature: "capabilities", value: "git", effect: -0.4 },
        ],
      },
    ],
    noise: ["ops/odd-one", "core/novel"],
  },
  confounds: {
    evidence: "observational",
    statistic: "cramersV",
    association: 0.82,
    pValue: 0.004,
    confounded: true,
    tested: 5,
    unlabeled: 0,
    strata: [{ key: "core", size: 3 }, { key: "ops", size: 2 }],
    undersized: ["ops"],
    options: { minStratumSize: 3, threshold: 0.5, alpha: 0.05, permutations: 999 },
  },
  map: [
    { id: "core/implement", x: 0, y: 0, cluster: "c0" },
    { id: "core/review", x: 0, y: 0, cluster: "c0" },
    { id: "ops/triage", x: 0.1, y: 0.05, cluster: "c0" },
    { id: "core/novel", x: 1, y: -0.4 },
    { id: "ops/odd-one", x: -0.8, y: 0.6 },
  ],
};

describe("LabsPage", () => {
  beforeEach(() => {
    window.history.replaceState(null, "", "/");
  });

  it("lists registered features with maturity and links to each feature page", () => {
    renderLabs(fixtureClient());

    expect(screen.getByRole("heading", { name: "Labs" })).toBeInTheDocument();
    const link = screen.getByRole("link", { name: "Workflow clustering" });
    expect(link).toHaveAttribute("href", "#/labs/workflow-clustering");
    expect(screen.getByText("Experimental")).toBeInTheDocument();
    expect(screen.queryByText("Coming soon")).not.toBeInTheDocument();
  });

  it("shows the experimental banner, prerequisites, and tracking issues for a feature", () => {
    renderLabs(fixtureClient(), "workflow-clustering");

    expect(screen.getByRole("heading", { name: "Workflow clustering" })).toBeInTheDocument();
    expect(screen.getByRole("note")).toHaveTextContent("Labs: experimental.");
    expect(screen.getByRole("heading", { name: "Prerequisites" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "#8302" })).toHaveAttribute(
      "href",
      "https://github.com/Agent-Clubhouse/Goobers/issues/8302",
    );
    expect(screen.getByRole("link", { name: "All Labs features" })).toHaveAttribute(
      "href",
      "#/labs",
    );
  });

  it("renders clusters, medoids, stability, distinguishing features, noise, and confound warnings", async () => {
    renderLabs(fixtureClient(clustered), "workflow-clustering");

    const card = await screen.findByRole("listitem", { name: "Cluster c0" });
    expect(screen.getByText(/Advisory · observational\./)).toBeInTheDocument();
    expect(within(card).getByText("Stable 0.91")).toBeInTheDocument();
    expect(within(card).getAllByRole("link", { name: "core/implement" })[0]).toHaveAttribute(
      "href",
      "#/workflow/core/implement",
    );
    expect(within(card).getByRole("link", { name: "ops/triage" })).toBeInTheDocument();
    expect(within(card).getByText("nodes.gate")).toBeInTheDocument();
    expect(within(card).getByText("+0.75")).toBeInTheDocument();
    expect(within(card).getByText("capabilities=git")).toBeInTheDocument();
    expect(within(card).getByText("-0.40")).toBeInTheDocument();
    const noise = screen.getByRole("region", { name: "Noise" });
    expect(within(noise).getByRole("link", { name: "core/novel" })).toBeInTheDocument();
    expect(screen.getByText(/Confound warning:/).closest("[role=status]")).toHaveTextContent(
      "clusters track gaggle boundaries",
    );
    expect(screen.getByText(/were not tested: ops\./)).toBeInTheDocument();
    expect(screen.getByText("0123456789ab")).toBeInTheDocument();
  });

  it("plots every workflow on a scatter map, merging identical shapes and marking medoids", async () => {
    renderLabs(fixtureClient(clustered), "workflow-clustering");

    const map = await screen.findByRole("img", { name: "Scatter map of 5 workflows in 1 cluster" });
    const dots = map.querySelectorAll("a.labs-map-dot");
    expect(dots).toHaveLength(4);
    const merged = [...dots].find((dot) => dot.textContent?.includes("implement +1"));
    expect(merged).toHaveAttribute("href", "#/workflow/core/implement");
    expect(merged?.querySelector("title")?.textContent).toBe(
      "core/implement, core/review — cluster c0 (medoid)",
    );
    expect(merged?.querySelector(".labs-map-medoid")).not.toBeNull();
    const noise = [...dots].filter((dot) => dot.classList.contains("labs-map-noise"));
    expect(noise.map((dot) => dot.querySelector("title")?.textContent)).toEqual([
      "core/novel — noise",
      "ops/odd-one — noise",
    ]);
  });

  it("explains insufficient data and an all-noise model", async () => {
    const { unmount } = renderLabs(fixtureClient(), "workflow-clustering");
    expect(await screen.findByRole("heading", { name: "Not enough workflows to cluster" })).toBeInTheDocument();
    expect(screen.getByText(/configures 0 workflows; clustering needs at least 3/)).toBeInTheDocument();
    unmount();

    const allNoise = {
      ...clustered,
      model: { ...clustered.model!, clusters: null, noise: ["a/x", "a/y", "a/z"] },
      confounds: { ...clustered.confounds!, confounded: false, undersized: null },
    } as unknown as LabsWorkflowClusterView;
    renderLabs(fixtureClient(allNoise), "workflow-clustering");
    expect(await screen.findByRole("heading", { name: "No clusters found" })).toBeInTheDocument();
    expect(screen.getByText(/No gaggle confound detected/)).toBeInTheDocument();
  });

  it("reports a failed cluster read", async () => {
    const client = fixtureClient();
    vi.spyOn(client, "getLabsWorkflowClusters").mockRejectedValue(new Error("config invalid"));
    renderLabs(client, "workflow-clustering");

    expect(await screen.findByRole("alert")).toHaveTextContent("Workflow clusters unavailable: config invalid");
  });

  it("falls back to the catalog for an unknown feature", () => {
    renderLabs(fixtureClient(), "does-not-exist");

    expect(screen.getByRole("heading", { name: "Labs" })).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("No Labs feature named does-not-exist");
  });

  it("is reachable from the primary navigation", async () => {
    window.location.hash = "#/overview";
    const user = userEvent.setup();
    render(<App client={fixtureClient()} />);

    await user.click(await screen.findByRole("button", { name: "Labs" }));

    expect(await screen.findByRole("heading", { name: "Labs" })).toBeInTheDocument();
    expect(window.location.hash).toBe("#/labs");
  });
});