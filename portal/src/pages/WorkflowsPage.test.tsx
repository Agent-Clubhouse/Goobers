import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
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
      await userEvent.click(screen.getByRole("button", { name: new RegExp(gaggle) }));
      const inventory = screen.getByRole("region", { name: `${gaggle} workflow definitions` });
      expect(
        within(inventory).getByRole("link", {
          name: `Open workflow Implementation for gaggle ${gaggle}`,
        }),
      ).toBeInTheDocument();
    }
  });

  it("uses compact persona summaries and collapses large gaggle inventories", async () => {
    const fixtures = populatedDaemonFixtures();
    const workflow = fixtures.workflows?.core?.items[0];
    if (!workflow || !fixtures.workflows?.core) {
      throw new Error("Core workflow fixture is required.");
    }
    fixtures.workflows.core.items = Array.from({ length: 4 }, (_, index) => ({
      ...workflow,
      identity: { gaggle: "core", name: `workflow-${index}` },
      displayName: `Workflow ${index}`,
      definition: { ...workflow.definition, digest: `sha256:workflow-${index}` },
    }));
    render(<App client={new FixtureDaemonClient(fixtures)} />);

    const toggle = await screen.findByRole("button", { name: /Core product/ });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByText("Workflow 0")).not.toBeInTheDocument();

    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    const section = toggle.closest("section");
    if (!section) {
      throw new Error("Expected gaggle inventory section.");
    }
    expect(within(section).getByText("Workflow 0")).toBeInTheDocument();
    expect(within(section).getByText(/1 configured persona/)).toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: "View Core product Goobers" }),
    ).toHaveAttribute("href", "#/goobers?gaggle=core");
    expect(within(section).queryByText("Workflow ownership")).not.toBeInTheDocument();
  });

  it("copies a manual-run command and announces success", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText },
    });
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    await userEvent.click(await screen.findByRole("button", { name: /Core product/ }));
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
    expect(
      screen.getByRole("button", { name: "Manual run command copied to the clipboard." }),
    ).toBeInTheDocument();
  });

  it("announces clipboard failure without navigating to the workflow", async () => {
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText: vi.fn().mockRejectedValue(new Error("denied")) },
    });
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    await userEvent.click(await screen.findByRole("button", { name: /Core product/ }));
    fireEvent.click(
      (await screen.findAllByRole("button", { name: "Copy manual run command" }))[0],
    );

    await waitFor(() =>
      expect(
        screen.getByText(
          "Could not copy the manual run command. Select and copy the command from the workflow details.",
        ),
      ).toBeInTheDocument(),
    );
    expect(
      screen.getByRole("button", { name: "Retry copy manual run command" }),
    ).toBeInTheDocument();
    expect(window.location.hash).toBe("#/workflows");
  });
});
