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
  confoundsByAxis: {
    gaggle: {
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
    model: {
      evidence: "observational",
      statistic: "cramersV",
      association: 0,
      pValue: 1,
      confounded: false,
      tested: 3,
      unlabeled: 2,
      strata: [{ key: "gpt-5.6-sol", size: 3 }],
      undersized: [],
      options: { minStratumSize: 3, threshold: 0.5, alpha: 0.05, permutations: 999 },
    },
    cohort: {
      evidence: "observational",
      statistic: "cramersV",
      association: 0.1,
      pValue: 0.8,
      confounded: false,
      tested: 5,
      unlabeled: 0,
      strata: [{ key: "review", size: 3 }, { key: "deploy", size: 2 }],
      undersized: ["deploy"],
      options: { minStratumSize: 3, threshold: 0.5, alpha: 0.05, permutations: 999 },
    },
    template: {
      evidence: "observational",
      statistic: "cramersV",
      association: 0.7,
      pValue: 0.02,
      confounded: true,
      tested: 5,
      unlabeled: 0,
      strata: [{ key: "reference/implementation", size: 3 }, { key: "reference/novel", size: 2 }],
      undersized: ["reference/novel"],
      options: { minStratumSize: 3, threshold: 0.5, alpha: 0.05, permutations: 999 },
    },
  },
  cohorts: ["deploy", "review"],
  map: [
    {
      id: "core/implement",
      x: 0,
      y: 0,
      cluster: "c0",
      cohort: "review",
      templateProvenance: {
        templateId: "reference/implementation",
        fingerprint: "sha256:abc",
        distance: 0.04,
        explicit: false,
        derived: true,
      },
    },
    {
      id: "core/review",
      x: 0,
      y: 0,
      cluster: "c0",
      cohort: "review",
      templateProvenance: {
        templateId: "reference/implementation",
        fingerprint: "sha256:abc",
        distance: 0.08,
        explicit: false,
        derived: true,
      },
    },
    { id: "ops/triage", x: 0.1, y: 0.05, cluster: "c0", cohort: "review" },
    { id: "core/novel", x: 1, y: -0.4, cohort: "deploy" },
    { id: "ops/odd-one", x: -0.8, y: 0.6, cohort: "deploy" },
  ],
  fitness: {
    scan: {
      candidates: 8,
      overLimit: 0,
      oversized: 0,
      beforeSince: 0,
      nonTerminal: 0,
      undefinedProvenance: 0,
      unreadable: 0,
    },
    clusters: [
      {
        clusterId: "c0",
        runCount: 8,
        minimumRuns: 5,
        insufficientRuns: false,
        successRate: 0.75,
        successInterval: { lower: 0.41, upper: 0.93 },
        medianWallTimeSeconds: 90,
        p90WallTimeSeconds: 240,
        medianCostUsd: 1.25,
        p90CostUsd: 3.5,
        humanGateRate: 0.25,
        escalationRate: 0.125,
        retryLoopRate: 0.5,
      },
    ],
  },
  templates: {
    distanceThreshold: 0.35,
    catalog: [{ id: "reference/implementation", fingerprint: "sha256:abc" }],
    workflows: {
      "core/implement": {
        templateId: "reference/implementation",
        fingerprint: "sha256:abc",
        distance: 0.04,
        explicit: false,
        derived: true,
      },
      "core/review": {
        templateId: "reference/implementation",
        fingerprint: "sha256:abc",
        distance: 0.08,
        explicit: false,
        derived: true,
      },
      "ops/triage": {
        templateId: "reference/implementation",
        fingerprint: "sha256:abc",
        distance: 0.6,
        explicit: true,
        derived: false,
        declaredTemplateId: "scaffold/workflow",
      },
    },
    beyondTemplates: [
      {
        clusterId: "c1",
        medoidId: "core/novel",
        nearestTemplate: "reference/implementation",
        distance: 0.72,
      },
    ],
  },
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
    for (const name of ["Backprop", "System One decisions", "Workflow Gym", "R-SZZ review evaluation", "Fluid UI"]) {
      expect(screen.getByRole("link", { name })).toBeInTheDocument();
    }
    expect(screen.getAllByText("Experimental").length).toBeGreaterThan(0);
    expect(screen.getAllByText("Coming soon").length).toBeGreaterThan(0);
  });

  it("points shipped features at their portal, CLI, and docs surfaces", () => {
    renderLabs(fixtureClient(), "backprop");

    expect(screen.getByRole("heading", { name: "Where to find it" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Insights → Backprop" })).toHaveAttribute(
      "href",
      "#/insight?section=backprop",
    );
    expect(screen.getByText(/goobers telemetry shadow \[--gaggle/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Backprop guide" })).toHaveAttribute(
      "href",
      "https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/backprop.md",
    );
    expect(screen.queryByRole("heading", { name: "Not available yet" })).not.toBeInTheDocument();
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
    expect(within(card).getAllByText(/derived from reference\/implementation/).length).toBeGreaterThan(0);
    expect(
      within(card).getByText(/nearest reference\/implementation · 0\.60 · declared scaffold\/workflow, diverged/),
    ).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "Convergence beyond templates" })).toHaveTextContent(
      "core/novel is 0.72 from reference/implementation",
    );
    const noise = screen.getByRole("region", { name: "Noise" });
    expect(within(noise).getByRole("link", { name: "core/novel" })).toBeInTheDocument();
    expect(screen.getByText(/Confound warning:/).closest("[role=status]")).toHaveTextContent(
      "clusters track gaggle boundaries",
    );
    expect(screen.getByText(/were not tested: ops\./)).toBeInTheDocument();
    expect(screen.getByText("0123456789ab")).toBeInTheDocument();
  });

  it("selects each available provenance confound axis", async () => {
    const user = userEvent.setup();
    renderLabs(fixtureClient(clustered), "workflow-clustering");

    await user.selectOptions(await screen.findByRole("combobox", { name: "Confound axis" }), "model");
    expect(screen.getByText(/No model confound detected/)).toHaveTextContent("2 undefined");
    await user.selectOptions(screen.getByRole("combobox", { name: "Confound axis" }), "cohort");
    expect(screen.getByText(/No task cohort confound detected/)).toBeInTheDocument();
    await user.selectOptions(screen.getByRole("combobox", { name: "Confound axis" }), "template");
    expect(screen.getByText(/clusters track template boundaries/)).toBeInTheDocument();
  });

  it("filters the cluster map by task cohort", async () => {
    const user = userEvent.setup();
    renderLabs(fixtureClient(clustered), "workflow-clustering");

    await screen.findByRole("img", { name: "Scatter map of 5 workflows in 1 cluster" });
    await user.selectOptions(screen.getByRole("combobox", { name: "Task cohort" }), "deploy");
    expect(screen.getByRole("img", { name: "Scatter map of 2 workflows in 1 cluster" })).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Task cohort" })).toHaveValue("deploy");
  });

  it("plots every workflow on a scatter map, merging identical shapes and marking medoids", async () => {
    renderLabs(fixtureClient(clustered), "workflow-clustering");

    const map = await screen.findByRole("img", { name: "Scatter map of 5 workflows in 1 cluster" });
    const dots = map.querySelectorAll("a.labs-map-dot");
    expect(dots).toHaveLength(4);
    const merged = [...dots].find((dot) => dot.textContent?.includes("implement +1"));
    expect(merged).toHaveAttribute("href", "#/workflow/core/implement");
    expect(merged?.querySelector("title")?.textContent).toBe(
      "core/implement, core/review — cluster c0 (medoid) (template-derived)",
    );
    expect(merged).toHaveClass("labs-map-dot-template");
    expect(merged?.querySelector(".labs-map-medoid")).not.toBeNull();
    expect(merged?.querySelector(".labs-map-fitness")).not.toBeNull();
    const noise = [...dots].filter((dot) => dot.classList.contains("labs-map-noise"));
    expect(noise.map((dot) => dot.querySelector("title")?.textContent)).toEqual([
      "core/novel — noise",
      "ops/odd-one — noise",
    ]);
  });

  it("maps retained child proposals per gaggle with lineage, outcomes, and novelty", async () => {
    const user = userEvent.setup();
    const proposals: LabsWorkflowClusterView = {
      ...clustered,
      proposals: {
        candidates: 3,
        overLimit: 0,
        excluded: 0,
        gaggles: [
          {
            gaggle: "core",
            status: "clustered",
            population: 2,
            minClusterSize: 2,
            model: {
              ...clustered.model!,
              population: 2,
              clusters: [{ ...clustered.model!.clusters[0], medoid: { id: "child-a" }, members: ["child-a", "child-b"] }],
              noise: [],
            },
            map: [
              { id: "child-a", x: 0, y: 0, cluster: "c0" },
              { id: "child-b", x: 1, y: 1, cluster: "c0" },
            ],
            proposals: [{
              id: "child-a", gaggle: "core", sourceDigest: "sha256:source", parentRunId: "parent-a",
              stageOccurrence: "author/0/visit/1", effectiveVersion: "sha256:effective", outcome: "completed",
              clusterId: "c0", stableClusterMedoid: true,
            }, {
              id: "child-b", gaggle: "core", sourceDigest: "sha256:other", parentRunId: "parent-b",
              stageOccurrence: "author/0/visit/1", effectiveVersion: "sha256:other", outcome: "failed",
              clusterId: "c0", stableClusterMedoid: false,
              novelty: { scored: true, modelId: "model", modelVersion: "v1", featureVersion: "f1", distance: 0.9, outOfDistribution: true },
            }],
          },
          {
            gaggle: "ops", status: "insufficient-data", population: 1, minClusterSize: 2, map: [],
            proposals: [{
              id: "child-ops", gaggle: "ops", sourceDigest: "sha256:ops", parentRunId: "parent-ops",
              stageOccurrence: "author/0/visit/1", effectiveVersion: "sha256:ops", outcome: "completed",
              stableClusterMedoid: false,
            }],
          },
        ],
      },
    };
    renderLabs(fixtureClient(proposals), "workflow-clustering");

    expect(await screen.findByRole("img", { name: "Scatter map of 2 child proposals in 1 cluster" })).toBeInTheDocument();
    expect(screen.getAllByRole("link", { name: "child-a" })[0]).toHaveAttribute("href", "#/run/child-a");
    expect(screen.getByRole("link", { name: "parent-a" })).toHaveAttribute("href", "#/run/parent-a");
    expect(screen.getByText("Stable-cluster medoid")).toBeInTheDocument();
    expect(screen.getByText("Novel / out of distribution")).toBeInTheDocument();
    expect(screen.getByRole("table", { name: "Child proposal outcomes by cluster" })).toHaveTextContent("1 (50%)");
    await user.selectOptions(screen.getByRole("combobox", { name: "Gaggle" }), "ops");
    expect(screen.getByRole("link", { name: "child-ops" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "child-a" })).not.toBeInTheDocument();
  });

  it("renders a clustered view whose proposal gaggle fit is all noise with null clusters", async () => {
    const allNoiseGaggle = {
      ...clustered,
      proposals: {
        candidates: 2, overLimit: 0, excluded: 0,
        gaggles: [{
          gaggle: "core", status: "clustered", population: 2, minClusterSize: 2,
          model: { ...clustered.model!, population: 2, clusters: null, noise: ["child-a", "child-b"] },
          map: [{ id: "child-a", x: 0, y: 0 }, { id: "child-b", x: 1, y: 1 }],
          proposals: [{
            id: "child-a", gaggle: "core", sourceDigest: "sha256:source", parentRunId: "parent-a",
            stageOccurrence: "author/0/visit/1", effectiveVersion: "sha256:effective", outcome: "failed",
            stableClusterMedoid: false,
          }],
        }],
      },
    } as unknown as LabsWorkflowClusterView;
    renderLabs(fixtureClient(allNoiseGaggle), "workflow-clustering");

    const map = await screen.findByRole("region", { name: "Child workflow pattern map" });
    expect(map).toHaveTextContent("2 retained proposals · 0 clusters");
    expect(within(map).getAllByRole("link", { name: "child-a" }).length).toBeGreaterThan(0);
    expect(within(map).queryByText("Stable-cluster medoid")).not.toBeInTheDocument();
  });

  it("shows advisory fitness and sample sizes on cluster cards", async () => {
    renderLabs(fixtureClient(clustered), "workflow-clustering");

    const card = await screen.findByRole("listitem", { name: "Cluster c0" });
    const fitness = within(card).getByRole("region", { name: "Fitness" });
    expect(fitness).toHaveTextContent("Fitness · 8 runs");
    expect(fitness).toHaveTextContent("75% (95% Wilson 41%–93%)");
    expect(fitness).toHaveTextContent("$1.25 / $3.50");
    expect(fitness).toHaveTextContent("50%");
  });

  it("explains insufficient data and an all-noise model", async () => {
    const { unmount } = renderLabs(fixtureClient(), "workflow-clustering");
    expect(await screen.findByRole("heading", { name: "Not enough workflows to cluster" })).toBeInTheDocument();
    expect(screen.getByText(/configures 0 workflows; clustering needs at least 3/)).toBeInTheDocument();
    unmount();

    const starter = {
      status: "insufficient-data",
      population: 1,
      minClusterSize: 3,
      templates: {
        distanceThreshold: 0.35,
        catalog: [{ id: "starter/default-implement", fingerprint: "sha256:def" }],
        workflows: {
          "example/default-implement": {
            templateId: "starter/default-implement",
            fingerprint: "sha256:def",
            distance: 0,
            explicit: true,
            derived: true,
            declaredTemplateId: "starter/default-implement",
          },
        },
        beyondTemplates: null,
      },
    } as unknown as LabsWorkflowClusterView;
    const { unmount: unmountStarter } = renderLabs(fixtureClient(starter), "workflow-clustering");
    const provenance = await screen.findByRole("region", { name: "Workflow template provenance" });
    expect(within(provenance).getByRole("link", { name: "example/default-implement" })).toBeInTheDocument();
    expect(provenance).toHaveTextContent("declared from starter/default-implement · 0.00");
    unmountStarter();

    const allNoise = {
      ...clustered,
      model: { ...clustered.model!, clusters: null, noise: ["a/x", "a/y", "a/z"] },
      confounds: { ...clustered.confounds!, confounded: false, undersized: null },
      confoundsByAxis: {
        ...clustered.confoundsByAxis,
        gaggle: { ...clustered.confoundsByAxis!.gaggle!, confounded: false, undersized: null },
      },
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

  it("shows Not available yet for a coming-soon feature with no surfaces", () => {
    renderLabs(fixtureClient(), "workflow-gym");
    expect(screen.queryByRole("heading", { name: "Where to find it" })).not.toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Not available yet" })).toBeInTheDocument();
    expect(screen.getByText(/This feature is not implemented yet\./)).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Get ready" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Try it" })).not.toBeInTheDocument();
  });

  it("walks users through trying a shipped feature and wiring it into workflows", () => {
    renderLabs(fixtureClient(), "workflow-clustering");

    expect(screen.getByText(/works on your instance today/)).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Try it" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Use it in your workflows" })).toBeInTheDocument();
    expect(screen.getByText("goobers workflow clusters --stratify gaggle ./instance")).toBeInTheDocument();
    expect(screen.getByText(/--cluster-model clusters\.json proposal\.yaml/)).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "What's next" })).toBeInTheDocument();
    expect(screen.getByText("Which shapes actually work")).toBeInTheDocument();
    expect(screen.getAllByRole("link", { name: "#8345" })[0]).toHaveAttribute(
      "href",
      "https://github.com/Agent-Clubhouse/Goobers/issues/8345",
    );
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