import { render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient } from "../api/fixtureClient";
import { populatedDaemonFixtures } from "../test/daemonFixtures";

beforeEach(() => {
  window.location.hash = "#/workflows";
});

describe("workflows page", () => {
  it("renders fixture-driven workflow inventories for every gaggle", async () => {
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    expect(await screen.findByRole("heading", { name: "Workflows" })).toBeInTheDocument();
    for (const gaggle of ["Core product", "Developer tools"]) {
      const inventory = screen.getByRole("region", { name: `${gaggle} workflow definitions` });
      expect(
        within(inventory).getByRole("link", {
          name: `Open workflow Implementation for gaggle ${gaggle}`,
        }),
      ).toBeInTheDocument();
    }
  });
});
