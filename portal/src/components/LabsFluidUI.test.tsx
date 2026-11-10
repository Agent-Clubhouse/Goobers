import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { DaemonApiError } from "../api/errors";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { FluidUICustomizationState } from "../api/types";
import { emptyDaemonFixtures } from "../test/daemonFixtures";

function fixtureClient(fluidUICustomization?: FluidUICustomizationState) {
  return new FixtureDaemonClient({ ...emptyDaemonFixtures(), fluidUICustomization });
}

const frame = () => document.querySelector(".portal-frame");

async function openEditor(client: FixtureDaemonClient) {
  render(<App client={client} />);
  return screen.findByRole("heading", { name: "Edit" });
}

beforeEach(() => {
  window.location.hash = "#/labs/fluid-ui";
});

describe("Labs Fluid UI editor", () => {
  it("shows the saved customization and its saved views", async () => {
    await openEditor(
      fixtureClient({
        customization: {
          version: "portal.customization/v1",
          density: "compact",
          savedViews: [{ name: "Active", route: "#/runs?status=active" }],
        },
        revision: 4,
        undoDepth: 2,
      }),
    );

    expect(screen.getByLabelText("Saved customization")).toHaveTextContent('"density": "compact"');
    expect(screen.getByText(/Revision 4/)).toHaveTextContent("2 undo steps");
    expect(
      within(screen.getByRole("navigation", { name: "Your saved views" })).getByRole("link", { name: "Active" }),
    ).toHaveAttribute("href", "#/runs?status=active");
    expect(screen.getByLabelText("Density")).toHaveValue("compact");
  });

  it("previews locally without saving and discards the preview on stop and reload", async () => {
    const client = fixtureClient();
    const put = vi.spyOn(client, "putLabsFluidUICustomization");
    const user = userEvent.setup();
    await openEditor(client);

    await user.selectOptions(screen.getByLabelText("Density"), "compact");
    await user.click(screen.getByRole("button", { name: "Preview" }));
    expect(frame()).toHaveAttribute("data-density", "compact");
    expect(screen.getByText("Previewing a Fluid UI customization.")).toBeInTheDocument();
    expect(put).not.toHaveBeenCalled();

    await user.click(screen.getAllByRole("button", { name: "Stop preview" })[0]);
    expect(frame()).not.toHaveAttribute("data-density");

    await user.click(screen.getByRole("button", { name: "Preview" }));
    expect(frame()).toHaveAttribute("data-density", "compact");
    cleanup();
    await openEditor(client);
    expect(frame()).not.toHaveAttribute("data-density");
    expect(put).not.toHaveBeenCalled();
  });

  it("applies every edited control with PUT and keeps it after a reload", async () => {
    const client = fixtureClient({
      customization: {
        version: "portal.customization/v1",
        savedViews: [{ name: "Old", route: "#/runs" }],
      },
      revision: 1,
      undoDepth: 0,
    });
    const put = vi.spyOn(client, "putLabsFluidUICustomization");
    const user = userEvent.setup();
    await openEditor(client);

    await user.selectOptions(screen.getByLabelText("Density"), "compact");
    await user.selectOptions(screen.getByLabelText("Run list sort"), "failing-first");
    await user.click(screen.getByRole("checkbox", { name: "failed" }));
    await user.click(screen.getByRole("checkbox", { name: "escalated" }));
    await user.click(screen.getByRole("checkbox", { name: "Show Configuration warnings" }));
    await user.click(screen.getByRole("button", { name: "Move Active runs and recent outcomes up" }));
    await user.click(screen.getByRole("button", { name: "Remove saved view Old" }));
    await user.type(screen.getByLabelText("Saved view name"), "Failing");
    await user.type(screen.getByLabelText("Saved view route"), "#/runs?status=attention");
    await user.click(screen.getByRole("button", { name: "Add view" }));
    await user.click(screen.getByRole("button", { name: "Apply" }));

    expect(await screen.findByText("Customization applied.")).toBeInTheDocument();
    expect(put).toHaveBeenCalledExactlyOnceWith({
      version: "portal.customization/v1",
      density: "compact",
      pages: { overview: { hidden: ["warnings"], order: ["summary", "activity", "attention", "warnings"] } },
      runList: { sort: "failing-first", phases: ["failed", "escalated"] },
      savedViews: [{ name: "Failing", route: "#/runs?status=attention" }],
    });
    expect(frame()).toHaveAttribute("data-density", "compact");

    cleanup();
    await openEditor(client);
    await waitFor(() => expect(frame()).toHaveAttribute("data-density", "compact"));
    expect(screen.getByLabelText("Run list sort")).toHaveValue("failing-first");
  });

  it("rejects an invalid saved view before it reaches the draft", async () => {
    const user = userEvent.setup();
    await openEditor(fixtureClient());

    await user.type(screen.getByLabelText("Saved view name"), "External");
    await user.type(screen.getByLabelText("Saved view route"), "https://example.test");
    await user.click(screen.getByRole("button", { name: "Add view" }));

    expect(screen.getByRole("alert")).toHaveTextContent(/route/);
    expect(screen.queryByRole("button", { name: "Remove saved view External" })).not.toBeInTheDocument();
  });

  it("undoes and resets through their routes and reports nothing to undo", async () => {
    const client = fixtureClient();
    const undo = vi.spyOn(client, "undoLabsFluidUICustomization");
    const reset = vi.spyOn(client, "resetLabsFluidUICustomization");
    const user = userEvent.setup();
    await openEditor(client);
    expect(screen.getByRole("button", { name: "Undo" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Reset" })).toBeDisabled();

    await user.selectOptions(screen.getByLabelText("Density"), "compact");
    await user.click(screen.getByRole("button", { name: "Apply" }));
    await screen.findByText("Customization applied.");

    await user.click(screen.getByRole("button", { name: "Reset" }));
    expect(await screen.findByText("Reset to the standard Portal.")).toBeInTheDocument();
    expect(reset).toHaveBeenCalledOnce();
    expect(frame()).not.toHaveAttribute("data-density");

    await user.click(screen.getByRole("button", { name: "Undo" }));
    expect(await screen.findByText("Undone.")).toBeInTheDocument();
    expect(undo).toHaveBeenCalledOnce();
    expect(frame()).toHaveAttribute("data-density", "compact");

    undo.mockRejectedValueOnce(
      new DaemonApiError(409, "nothing_to_undo", "nothing to undo"),
    );
    await user.click(screen.getByRole("button", { name: "Undo" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("There is nothing to undo.");
  });

  it("explains when this connection has no customization store", async () => {
    const client = fixtureClient();
    vi.spyOn(client, "getLabsFluidUICustomization").mockRejectedValue(
      new DaemonApiError(503, "labs_unavailable", "labs unavailable"),
    );
    render(<App client={client} />);

    expect(await screen.findByText(/not available on this connection/)).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Edit" })).not.toBeInTheDocument();
  });
});
