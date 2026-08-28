import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient, fixtureKey } from "../api/fixtureClient";
import type { WorkflowSummary } from "../api/types";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import {
  GaggleWorkflowExplorer,
  WorkflowPicker,
} from "./GaggleWorkflowExplorer";

describe("GaggleWorkflowExplorer", () => {
  it("loads the selected workflow graph and inspects stages", async () => {
    const fixtures = populatedDaemonFixtures();
    const workflows = fixtures.workflows?.core?.items;
    if (!workflows) {
      throw new Error("Core workflow fixtures are required.");
    }

    render(
      <GaggleWorkflowExplorer
        client={new FixtureDaemonClient(fixtures)}
        gaggleDisplayName="Core product"
        runs={fixtures.runs?.runs ?? []}
        workflows={workflows}
      />,
    );

    expect(
      await screen.findByRole("group", { name: "implementation execution graph" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "query stage details" })).toHaveTextContent(
      "Runtime",
    );

    await userEvent.click(
      screen.getByRole("button", {
        name: /implement, agentic task, owned by core\/implementer, configured/i,
      }),
    );

    const stage = screen.getByRole("region", {
      name: "implement stage details",
    });
    expect(within(stage).getByText("Core product / implementer")).toBeInTheDocument();
    expect(within(stage).getByText("repo:push")).toBeInTheDocument();
    expect(within(stage).getByText("Next → review")).toBeInTheDocument();
  });

  it("retries a failed workflow definition request", async () => {
    const fixtures = populatedDaemonFixtures();
    const workflows = fixtures.workflows?.core?.items;
    const detail = fixtures.workflowDetails?.[fixtureKey("core", "implementation")];
    if (!workflows || !detail) {
      throw new Error("Core workflow fixtures are required.");
    }
    const client = new FixtureDaemonClient(fixtures);
    const getWorkflow = vi
      .spyOn(client, "getWorkflow")
      .mockRejectedValueOnce(new Error("Definition service unavailable."))
      .mockResolvedValueOnce(detail);

    render(
      <GaggleWorkflowExplorer
        client={client}
        gaggleDisplayName="Core product"
        runs={fixtures.runs?.runs ?? []}
        workflows={workflows}
      />,
    );

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Definition service unavailable.",
    );
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));

    expect(
      await screen.findByRole("group", { name: "implementation execution graph" }),
    ).toBeInTheDocument();
    expect(getWorkflow).toHaveBeenCalledTimes(2);
  });

  it("rejects a mismatched workflow response", async () => {
    const fixtures = populatedDaemonFixtures();
    const workflows = fixtures.workflows?.core?.items;
    const detail = fixtures.workflowDetails?.[fixtureKey("core", "implementation")];
    if (!workflows || !detail) {
      throw new Error("Core workflow fixtures are required.");
    }
    const client = new FixtureDaemonClient(fixtures);
    vi.spyOn(client, "getWorkflow").mockResolvedValue({
      ...detail,
      identity: { ...detail.identity, name: "other" },
    });

    render(
      <GaggleWorkflowExplorer
        client={client}
        gaggleDisplayName="Core product"
        runs={[]}
        workflows={workflows}
      />,
    );

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "mismatched workflow detail",
    );
  });

  it("rejects inconsistent workflow metadata", async () => {
    const fixtures = populatedDaemonFixtures();
    const workflows = fixtures.workflows?.core?.items;
    const detail = fixtures.workflowDetails?.[fixtureKey("core", "implementation")];
    if (!workflows || !detail) {
      throw new Error("Core workflow fixtures are required.");
    }
    const client = new FixtureDaemonClient(fixtures);
    vi.spyOn(client, "getWorkflow").mockResolvedValue({
      ...detail,
      graph: { ...detail.graph, digest: "sha256:stale" },
    });

    render(
      <GaggleWorkflowExplorer
        client={client}
        gaggleDisplayName="Core product"
        runs={[]}
        workflows={workflows}
      />,
    );

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "inconsistent workflow definition metadata",
    );
  });
});

describe("WorkflowPicker", () => {
  it("exposes workflows as keyboard-selectable tabs", async () => {
    const onSelect = vi.fn();
    const workflows: WorkflowSummary[] = [
      workflowSummary("implementation", "Implementation"),
      workflowSummary("curation", "Backlog curation"),
    ];

    render(
      <WorkflowPicker
        gaggleDisplayName="Core product"
        onSelect={onSelect}
        runs={[]}
        selectedWorkflowName="implementation"
        workflows={workflows}
      />,
    );

    expect(screen.getByRole("tab", { name: /Implementation/ })).toHaveAttribute(
      "aria-selected",
      "true",
    );
    const implementation = screen.getByRole("tab", { name: /Implementation/ });
    const curation = screen.getByRole("tab", { name: /Backlog curation/ });
    expect(implementation).toHaveAttribute("tabindex", "0");
    expect(curation).toHaveAttribute("tabindex", "-1");

    implementation.focus();
    await userEvent.keyboard("{ArrowRight}");

    expect(onSelect).toHaveBeenCalledWith("curation");
    expect(curation).toHaveFocus();
    expect(
      screen.getByRole("link", {
        name: "Open workflow Backlog curation for gaggle Core product",
      }),
    ).toHaveAttribute("href", "#/workflow/core/curation");
  });
});

function workflowSummary(name: string, displayName: string): WorkflowSummary {
  return {
    identity: { gaggle: "core", name },
    displayName,
    purpose: `${displayName} purpose`,
    triggers: [{ type: "manual" }],
    readiness: {},
    concurrency: { activeRuns: 0, maxConcurrentRuns: 1 },
    owners: [],
    stageCount: 1,
    definition: { version: 1, digest: `sha256:${name}` },
    warnings: [],
  };
}
