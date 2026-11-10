import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { LabsFluidUIReport } from "../api/types";
import { LiveDataProvider } from "../liveData";
import { LabsPage } from "../pages/LabsPage";
import { emptyDaemonFixtures } from "../test/daemonFixtures";

function renderFluidUI(labsFluidUIReport?: LabsFluidUIReport) {
  const client = new FixtureDaemonClient({ ...emptyDaemonFixtures(), labsFluidUIReport });
  return render(
    <LiveDataProvider client={client}>
      <LabsPage client={client} feature="fluid-ui" />
    </LiveDataProvider>,
  );
}

describe("LabsFluidUIReport", () => {
  it("shows ranked candidates with reversion rate and unmet needs", async () => {
    renderFluidUI({
      advisory: "advisory",
      generatedAt: "2026-09-08T00:00:00Z",
      sustainedAfterDays: 7,
      users: 4,
      candidates: [
        {
          rank: 1,
          shape: ["density:compact", "runs.sort:failing-first"],
          users: 3,
          activeUsers: 3,
          sustainedUsers: 2,
          revertedUsers: 0,
          reversionRate: 0,
        },
        {
          rank: 2,
          shape: ["overview.hidden"],
          users: 2,
          activeUsers: 0,
          sustainedUsers: 0,
          revertedUsers: 2,
          reversionRate: 1,
        },
      ],
      unmetNeeds: [{ label: "dark-mode", users: 2 }],
    });

    const region = await screen.findByRole("region", { name: "Fluid UI promotion candidates" });
    expect(region).toHaveTextContent("never change global defaults");
    const table = await within(region).findByRole("table", { name: "Ranked customization shapes" });
    const rows = within(table).getAllByRole("row").slice(1);
    expect(rows[0]).toHaveTextContent("density:compact, runs.sort:failing-first");
    expect(rows[0]).toHaveTextContent("0%");
    expect(rows[1]).toHaveTextContent("overview.hidden");
    expect(rows[1]).toHaveTextContent("100%");
    const needs = within(region).getByRole("table", { name: "Unmet customization needs" });
    expect(needs).toHaveTextContent("dark-mode");
  });

  it("explains when nothing has been recorded", async () => {
    renderFluidUI();

    const region = await screen.findByRole("region", { name: "Fluid UI promotion candidates" });
    expect(await within(region).findByText("No customization shapes recorded yet")).toBeInTheDocument();
    expect(within(region).getByText("None recorded.")).toBeInTheDocument();
  });
});
