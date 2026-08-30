import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient } from "../api/fixtureClient";
import { populatedDaemonFixtures } from "../test/daemonFixtures";

beforeEach(() => {
  window.location.hash = "#/overview";
});

describe("gaggle reachability (#2531)", () => {
  it("reaches a gaggle directly from the sidebar without landing on Workflows first", async () => {
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    const sidebarGaggles = await screen.findByRole("navigation", { name: "Gaggles" });
    const coreLink = within(sidebarGaggles).getByRole("link", {
      name: "Open gaggle Core product",
    });
    expect(coreLink).toHaveAttribute("href", "#/gaggle/core");

    await userEvent.click(coreLink);
    await waitFor(() => expect(window.location.hash).toBe("#/gaggle/core"));
    expect(await screen.findByRole("heading", { name: "Core product" })).toBeInTheDocument();
  });

  it("highlights the current gaggle in the sidebar nav", async () => {
    window.location.hash = "#/gaggle/tools";
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    const sidebarGaggles = await screen.findByRole("navigation", { name: "Gaggles" });
    const toolsLink = within(sidebarGaggles).getByRole("link", {
      name: "Open gaggle Developer tools",
    });
    expect(toolsLink).toHaveAttribute("aria-current", "page");
    const coreLink = within(sidebarGaggles).getByRole("link", {
      name: "Open gaggle Core product",
    });
    expect(coreLink).not.toHaveAttribute("aria-current");
  });
});

describe("gaggle view summary (#2531)", () => {
  it("surfaces goobers, active runs, and recent outcomes alongside the topology", async () => {
    window.location.hash = "#/gaggle/core";
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    expect(await screen.findByRole("heading", { name: "Core product" })).toBeInTheDocument();

    const goobers = screen.getByRole("list", { name: "Core product goobers" });
    expect(within(goobers).getByText("Core implementer")).toBeInTheDocument();
    expect(
      within(goobers).getByText("Implements claimed backlog items end to end."),
    ).toBeInTheDocument();

    const active = screen.getByRole("region", { name: "Core product active runs" });
    expect(within(active).getByText(/01JZ441DAEMONAPI/)).toBeInTheDocument();

    const recent = screen.getByRole("region", { name: "Core product recent outcomes" });
    expect(within(recent).getByText(/01JZ402DASHBOARD/)).toBeInTheDocument();
    expect(within(recent).getByText(/01JZ400FAILED/)).toBeInTheDocument();
    expect(within(recent).getByText(/01JZ455ESCALATE/)).toBeInTheDocument();
    // The other gaggle's run must not leak into this gaggle's activity.
    expect(within(recent).queryByText(/01JZ300ABORTED/)).not.toBeInTheDocument();
  });

  it("reports no active runs for a gaggle with none in flight", async () => {
    window.location.hash = "#/gaggle/tools";
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    expect(await screen.findByRole("heading", { name: "Developer tools" })).toBeInTheDocument();
    expect(screen.getByText("No runs are active for Developer tools.")).toBeInTheDocument();
    const recent = screen.getByRole("region", { name: "Developer tools recent outcomes" });
    expect(within(recent).getByText(/01JZ300ABORTED/)).toBeInTheDocument();
  });
});

describe("gaggle switcher (#2531)", () => {
  it("lets the gaggle view itself switch between gaggles on a multi-gaggle instance", async () => {
    window.location.hash = "#/gaggle/core";
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    const switcher = await screen.findByLabelText("Switch gaggle");
    await userEvent.selectOptions(switcher, "tools");

    await waitFor(() => expect(window.location.hash).toBe("#/gaggle/tools"));
    expect(await screen.findByRole("heading", { name: "Developer tools" })).toBeInTheDocument();
  });

  it("omits the switcher on a single-gaggle instance", async () => {
    const fixtures = populatedDaemonFixtures();
    fixtures.gaggles.items = fixtures.gaggles.items.filter((gaggle) => gaggle.name === "core");
    window.location.hash = "#/gaggle/core";
    render(<App client={new FixtureDaemonClient(fixtures)} />);

    expect(await screen.findByRole("heading", { name: "Core product" })).toBeInTheDocument();
    expect(screen.queryByLabelText("Switch gaggle")).not.toBeInTheDocument();
  });
});

// #3658: a per-phase failure used to be swallowed into an empty group, so the
// gaggle read as idle when a phase was merely unreadable.
describe("gaggle partial run-phase failures (#3658)", () => {
  it("warns that the activity sections are incomplete when one phase query fails", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const real = client.listRuns.bind(client);
    vi.spyOn(client, "listRuns").mockImplementation(async (request, options) => {
      if (request?.phase === "completed") {
        throw new Error("The daemon request timed out after 10000ms.");
      }
      return real(request, options);
    });
    window.location.hash = "#/gaggle/core";

    render(<App client={client} />);

    expect(await screen.findByRole("heading", { name: "Core product" })).toBeInTheDocument();
    const warning = await screen.findByRole("alert");
    expect(warning).toHaveTextContent(/Run activity for the completed phase could not be read/);
    const active = screen.getByRole("region", { name: "Core product active runs" });
    expect(within(active).getByText(/01JZ441DAEMONAPI/)).toBeInTheDocument();
  });

  it("shows no incomplete-data warning when every phase reads successfully", async () => {
    window.location.hash = "#/gaggle/core";
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    expect(await screen.findByRole("heading", { name: "Core product" })).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});
