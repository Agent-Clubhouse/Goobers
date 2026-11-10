import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "./App";
import { FixtureDaemonClient } from "./api/fixtureClient";
import type { FluidUICustomization, FluidUICustomizationState } from "./api/types";
import {
  arrangeFluidUIPanels,
  sortRunsForFluidUI,
  validateFluidUICustomization,
} from "./fluidUI";
import { largeJournalFixtures, populatedDaemonFixtures } from "./test/daemonFixtures";

function saved(customization: FluidUICustomization | Record<string, unknown>, extra: Partial<FluidUICustomizationState> = {}) {
  return {
    customization: customization as FluidUICustomization,
    revision: 3,
    undoDepth: 0,
    ...extra,
  } satisfies FluidUICustomizationState;
}

function renderApp(fluidUICustomization: FluidUICustomizationState, fixtures = populatedDaemonFixtures()) {
  const client = new FixtureDaemonClient({ ...fixtures, fluidUICustomization });
  const view = render(<App client={client} />);
  return { client, view };
}

const frame = () => document.querySelector(".portal-frame");

beforeEach(() => {
  window.location.hash = "#/";
  Object.defineProperty(window, "innerWidth", { configurable: true, value: 1280 });
});

describe("Fluid UI customization", () => {
  it("applies density and hides and orders overview panels", async () => {
    renderApp(
      saved({
        version: "portal.customization/v1",
        density: "compact",
        pages: { overview: { hidden: ["summary"], order: ["warnings", "activity"] } },
        savedViews: [{ name: "Failed runs", route: "#/runs?status=all" }],
      }),
    );

    await waitFor(() => expect(frame()).toHaveAttribute("data-density", "compact"));
    const page = await waitFor(() => {
      const element = document.querySelector<HTMLElement>(".overview-page");
      if (!element?.querySelector(".overview-activity")) throw new Error("overview not loaded");
      return element;
    });
    expect(page.querySelector(".instance-summary-card")).toBeNull();
    expect(page).toHaveAttribute("data-panel-order", "custom");
    const warnings = page.querySelector(".overview-configuration")!;
    const activity = page.querySelector(".overview-activity")!;
    expect(warnings.compareDocumentPosition(activity) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByText(/Hidden by your Fluid UI customization: Instance summary/)).toBeInTheDocument();
    expect(
      within(screen.getByRole("navigation", { name: "Saved views" })).getByRole("link", { name: "Failed runs" }),
    ).toHaveAttribute("href", "#/runs?status=all");
  });

  it("falls back to the standard Portal with a notice for an unknown version", async () => {
    renderApp(saved({ version: "portal.customization/v9", density: "compact" }));

    expect(await screen.findByText(/could not be applied \(version: /)).toBeInTheDocument();
    expect(frame()).not.toHaveAttribute("data-density");
    expect(await screen.findByLabelText(/instance counts|status and counts/)).toBeInTheDocument();
  });

  it("falls back cleanly when the daemon marks the stored document incompatible", async () => {
    renderApp(saved({ version: "portal.customization/v1", density: "compact" }, { incompatible: true }));

    expect(await screen.findByText(/not compatible with this Portal version/)).toBeInTheDocument();
    expect(frame()).not.toHaveAttribute("data-density");
  });

  it("ignores unknown pages and panel IDs, lists them, and still applies the rest", async () => {
    const user = userEvent.setup();
    renderApp(
      saved({
        version: "portal.customization/v1",
        density: "compact",
        pages: { overview: { hidden: ["sparkline"] }, insights: { hidden: ["cost"] } },
      }),
    );

    const notice = await screen.findByText(/they are ignored: overview panel sparkline, page insights\./);
    expect(frame()).toHaveAttribute("data-density", "compact");
    expect(document.querySelector(".instance-summary-card")).not.toBeNull();
    await user.click(within(notice.closest("[role=status]")!).getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText(/they are ignored/)).not.toBeInTheDocument();
  });

  it("uses run-list sort and phase defaults unless the URL chooses a status", async () => {
    window.location.hash = "#/runs";
    const user = userEvent.setup();
    renderApp(
      saved({ version: "portal.customization/v1", runList: { sort: "failing-first", phases: ["completed", "failed"] } }),
      largeJournalFixtures({ completed: 2, running: 1, failed: 1, escalated: 0, aborted: 0 }),
    );

    const history = await screen.findByRole("region", { name: "Run history" });
    await waitFor(() =>
      expect(
        within(history)
          .getAllByRole("link", { name: /^Open run / })
          .map((link) => link.getAttribute("aria-label")),
      ).toEqual(["Open run 01JZTEST000000003", "Open run 01JZTEST000000001", "Open run 01JZTEST000000000"]),
    );
    const defaults = screen.getByLabelText("Fluid UI run-list defaults");
    expect(defaults).toHaveTextContent("Sort: Failing First");
    expect(defaults).toHaveTextContent("Phases: completed, failed");

    await user.click(within(defaults).getByRole("button", { name: "Show all phases" }));
    expect(within(history).getByRole("link", { name: "Open run 01JZTEST000000002" })).toBeInTheDocument();
    await user.click(within(defaults).getByRole("button", { name: "Use newest first" }));
    expect(screen.queryByLabelText("Fluid UI run-list defaults")).not.toBeInTheDocument();
    expect(within(history).getAllByRole("link", { name: /^Open run / })[0]).toHaveAccessibleName("Open run 01JZTEST000000003");
  });

  it("lets an explicit status in the URL win over the phase default", async () => {
    window.location.hash = "#/runs?status=all";
    renderApp(
      saved({ version: "portal.customization/v1", runList: { phases: ["completed"] } }),
      largeJournalFixtures({ completed: 2, running: 0, failed: 1, escalated: 0, aborted: 0 }),
    );

    const history = await screen.findByRole("region", { name: "Run history" });
    expect(await within(history).findByRole("link", { name: "Open run 01JZTEST000000002" })).toBeInTheDocument();
    expect(screen.queryByLabelText("Fluid UI run-list defaults")).not.toBeInTheDocument();
  });

  it("validates documents like the daemon and arranges panels and runs", () => {
    expect(validateFluidUICustomization({ version: "portal.customization/v1", savedViews: [{ name: "x", route: "/runs" }] })).toMatchObject({
      ok: false,
      problem: expect.stringMatching(/^savedViews\[0\]\.route/),
    });
    expect(validateFluidUICustomization({ version: "portal.customization/v1", extra: true })).toMatchObject({ ok: false });

    const panels = [{ id: "a" }, { id: "b" }, { id: "c" }];
    expect(arrangeFluidUIPanels(panels, { hidden: ["b", "zzz"], order: ["c", "zzz"] }).map(({ id }) => id)).toEqual(["c", "a"]);

    const run = (id: string, phase: "completed" | "failed", startedAt: string) =>
      ({ id, phase, startedAt }) as Parameters<typeof sortRunsForFluidUI>[0][number];
    const runs = [run("new", "completed", "2026-01-03T00:00:00Z"), run("mid", "failed", "2026-01-02T00:00:00Z"), run("old", "completed", "2026-01-01T00:00:00Z")];
    expect(sortRunsForFluidUI(runs, "oldest").map(({ id }) => id)).toEqual(["old", "mid", "new"]);
    expect(sortRunsForFluidUI(runs, "failing-first").map(({ id }) => id)).toEqual(["mid", "new", "old"]);
  });
});
