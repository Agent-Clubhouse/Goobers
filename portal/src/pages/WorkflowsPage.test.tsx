import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
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

  it("copies a manual-run command and announces success", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText },
    });
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    fireEvent.click(
      (await screen.findAllByRole("button", { name: "Copy manual run command" }))[0],
    );

    await waitFor(() =>
      expect(writeText).toHaveBeenCalledWith(
        "goobers run core/implementation 'C:\\Goobers\\instances\\local-dev'",
      ),
    );
    expect(
      screen.getByText("Manual run command copied to the clipboard."),
    ).toBeInTheDocument();
  });

  it("announces clipboard failure without navigating to the workflow", async () => {
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText: vi.fn().mockRejectedValue(new Error("denied")) },
    });
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    fireEvent.click(
      (await screen.findAllByRole("button", { name: "Copy manual run command" }))[0],
    );

    await waitFor(() =>
      expect(
        screen.getByText(
          "Could not copy the manual run command. Copy the command from the workflow row.",
        ),
      ).toBeInTheDocument(),
    );
    expect(window.location.hash).toBe("#/workflows");
  });
});
