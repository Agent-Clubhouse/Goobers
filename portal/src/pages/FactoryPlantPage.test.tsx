import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { act } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import portalStyles from "../styles.css?raw";
import { App } from "../App";
import { FixtureDaemonClient, fixtureKey } from "../api/fixtureClient";
import type {
  DaemonEventStream,
  DaemonUpdateEvent,
  EventStreamRequest,
  RequestOptions,
  RunList,
  RunListOptions,
  RunSummary,
} from "../api/types";
import { factoryFloorFixtures, populatedDaemonFixtures } from "../test/daemonFixtures";

/**
 * The plant layout is the second view of one floor model, so these tests hold
 * it to the promise the toggle makes: the same daemon facts, the same entities,
 * the same IDs, the same inspector, and no extra reads. Anything the line
 * layout says honestly, the plant must say honestly too.
 */

beforeEach(() => {
  window.location.hash = "#/factory";
});

const LINE_LAYOUT = "Factory floor";
const PLANT_LAYOUT = "Factory plant";

async function switchToPlant(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: "Plant" }));
  return screen.findByRole("group", { name: PLANT_LAYOUT });
}

describe("factory layout toggle", () => {
  it("offers both layouts in one control and starts on the line topology", async () => {
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    await screen.findByRole("group", { name: LINE_LAYOUT });
    const control = screen.getByRole("group", { name: "Floor layout" });
    expect(within(control).getByRole("button", { name: "Lines" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(within(control).getByRole("button", { name: "Plant" })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
    // The lens is a separate question and keeps its own control.
    expect(screen.getByRole("group", { name: "Floor lens" })).toBeInTheDocument();
    expect(screen.getByText("Layout")).toBeVisible();
    expect(screen.getByText("Lens")).toBeVisible();
    expect(window.location.hash).toBe("#/factory");
  });

  it("switches to the plant and records the layout in the route", async () => {
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    await screen.findByRole("group", { name: LINE_LAYOUT });
    await switchToPlant(user);

    expect(window.location.hash).toBe("#/factory?layout=plant");
    expect(screen.queryByRole("group", { name: LINE_LAYOUT })).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Lines" }));
    expect(await screen.findByRole("group", { name: LINE_LAYOUT })).toBeInTheDocument();
    expect(window.location.hash).toBe("#/factory");
  });

  it("opens straight into the plant when the route asks for it", async () => {
    window.location.hash = "#/factory?layout=plant";
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    expect(await screen.findByRole("group", { name: PLANT_LAYOUT })).toBeInTheDocument();
  });

  it("keeps gaggle, workflow and lens scope across a layout change", async () => {
    window.location.hash = "#/factory?gaggle=tools&lens=risk";
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    await screen.findByRole("group", { name: LINE_LAYOUT });
    const plant = await switchToPlant(user);

    expect(window.location.hash).toBe("#/factory?gaggle=tools&lens=risk&layout=plant");
    expect(plant).toHaveAttribute("data-lens", "risk");
    expect(screen.getByLabelText("Gaggle")).toHaveValue("tools");
    expect(
      within(plant).queryByRole("button", {
        name: /^Workflow Implementation, gaggle Core product/,
      }),
    ).not.toBeInTheDocument();
    expect(
      within(plant).getByRole("button", {
        name: /^Workflow Implementation, gaggle Developer tools/,
      }),
    ).toBeInTheDocument();

    // The lens control still works, and it does not disturb the layout.
    await user.click(screen.getByRole("button", { name: "Flow" }));
    await waitFor(() =>
      expect(window.location.hash).toBe("#/factory?gaggle=tools&lens=flow&layout=plant"),
    );
    expect(
      await screen.findByRole("group", { name: PLANT_LAYOUT }),
    ).toHaveAttribute("data-lens", "flow");
  });

  it("makes no further daemon reads when only the layout changes", async () => {
    const client = new FixtureDaemonClient(factoryFloorFixtures());
    const user = userEvent.setup();
    render(<App client={client} />);
    await screen.findByRole("group", { name: LINE_LAYOUT });
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 80));
    });

    const listRuns = vi.spyOn(client, "listRuns");
    const getWorkflow = vi.spyOn(client, "getWorkflow");
    const listGaggles = vi.spyOn(client, "listGaggles");
    const listRunEvents = vi.spyOn(client, "listRunEvents");
    const listStageAttempts = vi.spyOn(client, "listStageAttempts");

    await switchToPlant(user);
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 80));
    });

    expect(listRuns).not.toHaveBeenCalled();
    expect(getWorkflow).not.toHaveBeenCalled();
    expect(listGaggles).not.toHaveBeenCalled();
    expect(listRunEvents).not.toHaveBeenCalled();
    expect(listStageAttempts).not.toHaveBeenCalled();
  });
});

describe("plant layout entities", () => {
  it("draws the same gaggles, lines, stages, runs and goobers as the line layout", async () => {
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    const lines = await screen.findByRole("group", { name: LINE_LAYOUT });
    const names = (scope: HTMLElement, pattern: RegExp) =>
      within(scope)
        .getAllByRole("button", { name: pattern })
        .map((button) => button.getAttribute("aria-label"))
        .sort();
    const lineWorkflows = names(lines, /^Workflow /);
    const lineStages = names(lines, /^Stage /);
    const lineRuns = names(lines, /^Run /);
    const lineGoobers = names(lines, /^Goober /);

    const plant = await switchToPlant(user);

    expect(names(plant, /^Workflow /)).toEqual(lineWorkflows);
    expect(names(plant, /^Stage /)).toEqual(lineStages);
    expect(names(plant, /^Run /)).toEqual(lineRuns);
    expect(names(plant, /^Goober /)).toEqual(lineGoobers);
    expect(lineStages.length).toBeGreaterThan(0);
    expect(lineRuns.length).toBeGreaterThan(0);
  });

  it("selects a run, its stage and its goober into the same inspector", async () => {
    window.location.hash = "#/factory?layout=plant";
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    const plant = await screen.findByRole("group", { name: PLANT_LAYOUT });
    const rail = screen.getByRole("complementary", { name: "Factory inspector" });

    await user.click(
      within(plant).getByRole("button", { name: /^Run 01JZ500BLOCKED/ }),
    );
    expect(within(rail).getByRole("heading", { name: "01JZ500BLOCKED" })).toBeVisible();

    await user.click(
      within(plant).getByRole("button", {
        name: /^Stage implement\..*gaggle core/,
      }),
    );
    expect(within(rail).getByRole("heading", { name: "implement" })).toBeVisible();

    await user.click(
      within(plant).getByRole("button", { name: /^Goober Core implementer/ }),
    );
    expect(within(rail).getByRole("heading", { name: "Core implementer" })).toBeVisible();

    await user.click(
      within(plant).getByRole("button", {
        name: /^Workflow Implementation, gaggle Core product/,
      }),
    );
    expect(within(rail).getByRole("heading", { name: "Implementation" })).toBeVisible();
  });

  it("keeps a selected entity selected when the layout changes", async () => {
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    const lines = await screen.findByRole("group", { name: LINE_LAYOUT });
    await user.click(
      within(lines).getByRole("button", { name: /^Stage implement\..*gaggle core/ }),
    );
    const rail = screen.getByRole("complementary", { name: "Factory inspector" });
    expect(within(rail).getByRole("heading", { name: "implement" })).toBeVisible();

    const plant = await switchToPlant(user);

    expect(within(rail).getByRole("heading", { name: "implement" })).toBeVisible();
    expect(
      within(plant).getByRole("button", { name: /^Stage implement\..*gaggle core/ }),
    ).toHaveAttribute("aria-pressed", "true");
  });

  it("supports keyboard selection and keeps focus on the machine", async () => {
    window.location.hash = "#/factory?layout=plant";
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    const plant = await screen.findByRole("group", { name: PLANT_LAYOUT });
    const machine = within(plant).getByRole("button", {
      name: /^Stage query\. Deterministic stage\. workflow Implementation\. gaggle core/,
    });
    machine.focus();
    await user.keyboard("{Enter}");

    expect(machine).toHaveFocus();
    expect(machine).toHaveAttribute("aria-pressed", "true");
    expect(
      within(screen.getByRole("complementary", { name: "Factory inspector" }))
        .getByRole("heading", { name: "query" }),
    ).toBeVisible();
  });
});

describe("plant layout honesty", () => {
  it("does not redraw crossing topology wires over the factory illustration", async () => {
    window.location.hash = "#/factory?gaggle=core&layout=plant";
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    const plant = await screen.findByRole("group", { name: PLANT_LAYOUT });
    expect(plant.querySelector(".factory-plant-live-belts")).not.toBeInTheDocument();
    expect(plant.querySelector(".factory-plant-annotations")).not.toBeInTheDocument();
    expect(plant.querySelector(".plant-belt-line")).not.toBeInTheDocument();
  });

  it("marks real active work for localized operating animation", async () => {
    window.location.hash = "#/factory?layout=plant";
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    const plant = await screen.findByRole("group", { name: PLANT_LAYOUT });
    expect(plant).toHaveAttribute("data-working", "true");
    expect(within(plant).getByText("FACTORY WORKING")).toBeInTheDocument();
    expect(
      plant.querySelector('.factory-plant-machine[data-status="running"]'),
    ).toBeInTheDocument();
    expect(
      plant.querySelector('.factory-plant-staff[data-working="true"]'),
    ).toBeInTheDocument();
  });

  it("keeps blocked work still and labels it as attention, not working", async () => {
    window.location.hash = "#/factory?gaggle=core&layout=plant";
    const fixtures = factoryFloorFixtures();
    fixtures.runs = {
      runs: fixtures.runs.runs.filter((run) => run.id === "01JZ500BLOCKED"),
    };
    render(<App client={new FixtureDaemonClient(fixtures)} />);

    const plant = await screen.findByRole("group", { name: PLANT_LAYOUT });
    expect(plant).toHaveAttribute("data-working", "false");
    expect(within(plant).getByText("ATTENTION REQUIRED")).toBeInTheDocument();
    expect(
      plant.querySelector('.factory-plant-staff[data-active="true"]'),
    ).toBeInTheDocument();
    expect(
      plant.querySelector('.factory-plant-staff[data-working="true"]'),
    ).not.toBeInTheDocument();
  });

  it("uses truthful workflow identity for floor paint", async () => {
    window.location.hash = "#/factory?gaggle=core&layout=plant";
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    const plant = await screen.findByRole("group", { name: PLANT_LAYOUT });
    expect(within(plant).getByText("Core product · Implementation")).toBeInTheDocument();
    expect(within(plant).queryByText("LINE 01")).not.toBeInTheDocument();
  });

  it("raises a red block and an amber hold with text, not colour alone", async () => {
    window.location.hash = "#/factory?layout=plant";
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    const plant = await screen.findByRole("group", { name: PLANT_LAYOUT });
    const blocked = within(plant).getByRole("button", {
      name: /^Stage implement\..*gaggle core/,
    });
    expect(blocked).toHaveAttribute("data-alarm", "blocked");
    expect(blocked).toHaveAttribute("data-status", "blocked");
    expect(within(blocked).getByText("Alarm: stage blocked")).toBeInTheDocument();
    expect(blocked).toHaveAccessibleName(/blocked alarm/);
    expect(within(blocked).getByText("BLOCKED")).toBeInTheDocument();

    const held = within(plant).getByRole("button", {
      name: /^Stage review\..*gaggle tools/,
    });
    expect(held).toHaveAttribute("data-alarm", "hold");
    expect(within(held).getByText("Alarm: human gate hold")).toBeInTheDocument();
    expect(held).toHaveAccessibleName(/human hold alarm/);
    expect(within(held).getByText("HOLD")).toBeInTheDocument();
  });

  it("keeps the alarm off while any run signal at that stage is unread", async () => {
    window.location.hash = "#/factory?layout=plant";
    const fixtures = factoryFloorFixtures();
    const blocked = fixtures.runs.runs.find((run) => run.id === "01JZ500BLOCKED")!;
    const unread = {
      ...blocked,
      id: "01JZ510UNREAD",
      startedAt: "2026-07-18T06:35:00Z",
      lastActivityAt: "2026-07-18T06:36:00Z",
    };
    fixtures.runs.runs.push(unread);
    render(<App client={new UnreadSignalClient(fixtures, unread.id)} />);

    const plant = await screen.findByRole("group", { name: PLANT_LAYOUT });
    const machine = within(plant).getByRole("button", {
      name: /^Stage implement\..*gaggle core/,
    });
    expect(machine).toHaveAttribute("data-status", "unknown");
    expect(machine).toHaveAttribute("data-alarm", "off");
    expect(machine).toHaveAccessibleName(/1 run signals unread/);
    expect(within(machine).getByText("UNREAD")).toBeInTheDocument();
  });

  it("caps dense stage and inbound work behind truthful overflow controls", async () => {
    window.location.hash = "#/factory?layout=plant";
    const user = userEvent.setup();
    const { unmount } = render(
      <App client={new FixtureDaemonClient(denseStageFixtures(50))} />,
    );

    let plant = await screen.findByRole("group", { name: PLANT_LAYOUT });
    expect(within(plant).getAllByRole("button", { name: /^Run / })).toHaveLength(6);
    const stageOverflow = within(plant).getByRole("button", {
      name: /44 additional runs at stage implement/,
    });
    stageOverflow.focus();
    await user.keyboard("{Enter}");
    expect(
      within(screen.getByRole("complementary", { name: "Factory inspector" }))
        .getByRole("heading", { name: "implement" }),
    ).toBeVisible();
    unmount();

    window.location.hash = "#/factory?layout=plant";
    render(<App client={new FixtureDaemonClient(denseInboundFixtures(10))} />);
    plant = await screen.findByRole("group", { name: PLANT_LAYOUT });
    expect(within(plant).getAllByRole("button", { name: /^Run / })).toHaveLength(4);
    expect(
      within(plant).getByRole("button", {
        name: /6 additional runs waiting at inbound/,
      }),
    ).toBeVisible();
  });

  it("marks the active list as partial in the plant when the daemon truncates it", async () => {
    window.location.hash = "#/factory?layout=plant";
    render(<App client={new TruncatedRunsClient(denseStageFixtures(50))} />);

    const plant = await screen.findByRole("group", { name: PLANT_LAYOUT });
    expect(
      within(plant).getByRole("button", {
        name: /^Workflow Implementation, gaggle Core product\. 50 or more active runs/,
      }),
    ).toBeVisible();
    expect(screen.getByText("Partial view")).toBeVisible();
  });

  it("caps idle commons goobers and still shows the configured plant", async () => {
    window.location.hash = "#/factory?layout=plant";
    render(<App client={new FixtureDaemonClient(denseCommonsFixtures(20))} />);

    const plant = await screen.findByRole("group", { name: PLANT_LAYOUT });
    expect(screen.getByText("Plant ready. No active runs are on the floor.")).toBeVisible();
    expect(within(plant).getAllByRole("button", { name: /^Goober / })).toHaveLength(12);
    expect(
      within(plant).getByRole("button", {
        name: /\d+ additional ready goobers\. Select the floor summary/,
      }),
    ).toBeVisible();
    expect(
      within(plant).getByRole("button", {
        name: /^Workflow Implementation, gaggle Core product/,
      }),
    ).toBeVisible();
  });

  it("keeps an idle plant visible instead of blanking the hall", async () => {
    window.location.hash = "#/factory?layout=plant";
    render(<App client={new IdlePlantClient(populatedDaemonFixtures())} />);

    expect(await screen.findByText("Plant ready. No active runs are on the floor.")).toBeVisible();
    const plant = await screen.findByRole("group", { name: PLANT_LAYOUT });
    expect(
      within(plant).getAllByRole("button", { name: /^Stage / }).length,
    ).toBeGreaterThan(0);
    const rail = screen.getByRole("complementary", { name: "Factory inspector" });
    expect(within(rail).getByText("Blocked stages").closest("div")).toHaveTextContent("0");
  });

  it("says a workflow topology was unread rather than drawing invented machines", async () => {
    window.location.hash = "#/factory?layout=plant";
    render(<App client={new FixtureDaemonClient(manyWorkflowFixtures(16, 15))} />);

    const plant = await screen.findByRole("group", { name: PLANT_LAYOUT });
    const lines = within(plant).getAllByRole("button", { name: /^Workflow Flow / });
    expect(lines.length).toBeGreaterThan(12);
    const unread = lines.filter((line) =>
      /topology was not read in this batch/.test(line.getAttribute("aria-label") ?? ""),
    );
    expect(unread.length).toBeGreaterThan(0);
    expect(within(plant).getAllByText("3 stages unread").length).toBeGreaterThanOrEqual(
      unread.length,
    );
    for (const line of unread) {
      expect(line).toHaveAccessibleName(/3 stages are configured and none are drawn/);
    }
    // The workflows that were read still show real machines beside them.
    expect(
      within(plant).getAllByRole("button", { name: /^Stage / }).length,
    ).toBeGreaterThan(0);
  });
});

describe("plant layout liveness", () => {
  it("moves the run that changed stage and leaves its siblings alone", async () => {
    window.location.hash = "#/factory?layout=plant";
    const user = userEvent.setup();
    const fixtures = factoryFloorFixtures();
    const sibling = {
      ...fixtures.runs.runs.find((run) => run.id === "01JZ700RETRY")!,
      id: "01JZ650SIBLING",
      startedAt: "2026-07-18T06:50:00Z",
      lastActivityAt: "2026-07-18T06:51:00Z",
    };
    fixtures.runs.runs.push(sibling);
    fixtures.stageAttempts![fixtureKey(sibling.id, "implement")] = {
      ...structuredClone(fixtures.stageAttempts![fixtureKey("01JZ700RETRY", "implement")]!),
      runId: sibling.id,
    };
    const client = new LivePlantClient(fixtures);
    render(<App client={client} />);

    const plant = await screen.findByRole("group", { name: PLANT_LAYOUT });
    const mover = await within(plant).findByRole("button", {
      name: /^Run 01JZ700RETRY.*at stage implement/,
    });
    const siblingBefore = within(plant).getByRole("button", {
      name: /^Run 01JZ650SIBLING.*at stage implement/,
    });
    const moverTransform = mover.getAttribute("style");
    const siblingTransform = siblingBefore.getAttribute("style");
    const machineCount = within(plant).getAllByRole("button", { name: /^Stage / }).length;

    client.moveRun("01JZ700RETRY", "review");
    await act(async () => {
      client.push("01JZ700RETRY", "session:stage-change");
      await new Promise((resolve) => setTimeout(resolve, 80));
    });

    const moved = await within(plant).findByRole("button", {
      name: /^Run 01JZ700RETRY.*at stage review/,
    });
    await waitFor(() => expect(moved).toHaveAttribute("data-moved", "true"));
    expect(moved).toBe(mover);
    expect(moved).toHaveClass("is-transitioning");
    expect(moved.getAttribute("style")).not.toBe(moverTransform);
    expect(moved).toHaveAccessibleName(/Moved from implement/);

    const siblingAfter = within(plant).getByRole("button", {
      name: /^Run 01JZ650SIBLING.*at stage implement/,
    });
    expect(siblingAfter).toBe(siblingBefore);
    expect(siblingAfter.getAttribute("style")).toBe(siblingTransform);
    expect(siblingAfter).toHaveAttribute("data-moved", "false");
    expect(within(plant).getAllByRole("button", { name: /^Stage / })).toHaveLength(
      machineCount,
    );

    await user.click(screen.getByRole("button", { name: "Lines" }));
    const lines = await screen.findByRole("group", { name: LINE_LAYOUT });
    expect(
      within(lines).getByRole("button", { name: /^Run 01JZ700RETRY.*at stage review/ }),
    ).toHaveAttribute("data-moved", "false");

    await user.click(screen.getByRole("button", { name: "Plant" }));
    const remounted = await screen.findByRole("group", { name: PLANT_LAYOUT });
    expect(
      within(remounted).getByRole("button", {
        name: /^Run 01JZ700RETRY.*at stage review/,
      }),
    ).toHaveAttribute("data-moved", "false");
  });
});

describe("plant layout presentation contracts", () => {
  it("keeps the approved image as a fallback when WebGL is unavailable", async () => {
    window.location.hash = "#/factory?layout=plant";
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    const plant = await screen.findByRole("group", { name: PLANT_LAYOUT });
    const renderer = plant.querySelector(".factory-plant-renderer");
    await waitFor(() => expect(renderer).toHaveAttribute("data-webgl", "fallback"));
    expect(renderer?.querySelector(".factory-plant-backdrop")).toBeInTheDocument();
    expect(renderer?.querySelector("canvas.factory-plant-webgl")).toBeInTheDocument();

    const rendererStyles = portalStyles.slice(
      portalStyles.indexOf(".factory-plant-renderer {"),
      portalStyles.indexOf(".factory-plant-live-belts"),
    );
    expect(rendererStyles).toContain('[data-webgl="ready"] .factory-plant-backdrop');
    expect(rendererStyles).toContain("opacity: 0");
    expect(rendererStyles).toContain('[data-webgl="ready"] .factory-plant-webgl');
  });

  it("never renders a free-form or sensitive daemon field", async () => {
    window.location.hash = "#/factory?layout=plant";
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    const plant = await screen.findByRole("group", { name: PLANT_LAYOUT });
    const forbidden = [
      "trigger-ref-should-not-render",
      "free-form attempt message",
      "free-form failure message",
      "Agent-Clubhouse",
      "Toolbox",
      "sha256:",
      "github:issues:write",
      "blocked_by_agent",
      "executor_error",
    ];
    const plantMarkup = plant.innerHTML;
    for (const value of forbidden) {
      expect(plantMarkup).not.toContain(value);
    }
    expect(plantMarkup).not.toMatch(/https?:\/\//);

    const lines = await (async () => {
      await user.click(screen.getByRole("button", { name: "Lines" }));
      return screen.findByRole("group", { name: LINE_LAYOUT });
    })();
    const lineMarkup = lines.innerHTML;
    for (const value of forbidden) {
      expect(lineMarkup).not.toContain(value);
    }
    expect(lineMarkup).not.toMatch(/https?:\/\//);
  });

  it("declares the plant reduced-motion contract in the stylesheet", () => {
    const factoryBlocks = portalStyles
      .slice(portalStyles.indexOf("Factory Floor (#/factory)"))
      .split("@media (prefers-reduced-motion: reduce)");

    expect(factoryBlocks).toHaveLength(2);
    const reduced = factoryBlocks[1];
    expect(reduced).toContain(".factory-plant-beacon");
    expect(reduced).toContain(".factory-plant-crate");
    expect(reduced).toContain(".factory-plant-machine[data-status=\"running\"]");
    expect(reduced).toContain(".factory-plant-staff[data-working=\"true\"]");
    expect(reduced).toContain("animation: none !important");
    expect(portalStyles).toContain("@keyframes factory-belt-operating");
    expect(portalStyles).toContain("@keyframes factory-machine-cycle");
    expect(portalStyles).toContain("@keyframes factory-plant-machine-core");

    // Only compact alarm hardware blinks. The floor wash stays static.
    const plantSection = factoryBlocks[0].slice(
      factoryBlocks[0].indexOf("The plant, isometric"),
    );
    const washBlock = plantSection.slice(
      plantSection.indexOf(".plant-alarm-wash"),
      plantSection.indexOf("/* --- Belts"),
    );
    expect(washBlock).not.toContain("animation:");
    expect(plantSection).toContain("steps(1, end) infinite");
  });

  it("fits the plant in a clipped workspace camera instead of nesting scrollbars", async () => {
    const user = userEvent.setup();
    window.location.hash = "#/factory?layout=plant";
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    const viewport = await screen.findByLabelText("Factory plant viewport");
    expect(viewport).toHaveAttribute("data-camera", "fit");
    expect(within(viewport).getByRole("button", { name: "Fit all" })).toBeVisible();
    expect(within(viewport).getByRole("button", { name: "Zoom in" })).toBeVisible();
    expect(within(viewport).getByRole("button", { name: "Zoom out" })).toBeVisible();

    await user.click(within(viewport).getByRole("button", { name: "Zoom in" }));
    expect(viewport).toHaveAttribute("data-camera", "manual");
    await user.click(within(viewport).getByRole("button", { name: "Fit all" }));
    expect(viewport).toHaveAttribute("data-camera", "fit");

    const viewportBlock = portalStyles.slice(
      portalStyles.indexOf(".factory-viewport {"),
      portalStyles.indexOf(".factory-viewport[data-dragging"),
    );
    expect(viewportBlock).toContain("overflow: hidden");
    expect(portalStyles).toContain(".page-content-workspace");
    expect(portalStyles).toContain("max-width: none");
  });

  it("draws keyboard focus on the inner machine and crate silhouettes", () => {
    expect(portalStyles).toContain(
      ".factory-plant-machine:focus-visible .plant-face",
    );
    expect(portalStyles).toContain(
      ".factory-plant-crate:focus-visible .plant-crate-face",
    );
    expect(portalStyles).toContain(
      ".factory-plant-crate:focus-visible .plant-crate-halo",
    );
    const focusBlock = portalStyles.slice(
      portalStyles.indexOf(".factory-plant-machine:focus-visible .plant-face"),
      portalStyles.indexOf(".factory-plant-lamp"),
    );
    expect(focusBlock).toContain("stroke: var(--focus)");
    expect(focusBlock).toContain("stroke-width: 2.2");
    expect(focusBlock).toContain("opacity: 1");
  });

  it("shows a plant-only legend without adding plant concepts to Lines", async () => {
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    await screen.findByRole("group", { name: LINE_LAYOUT });
    expect(screen.getByText("Machines")).toBeVisible();
    expect(screen.queryByText("Beacon alarm")).not.toBeInTheDocument();

    await switchToPlant(user);
    expect(screen.getByText("Plant", { selector: "strong" })).toBeVisible();
    expect(screen.getByText("Beacon alarm")).toBeVisible();
    expect(screen.getByText("Placard status")).toBeVisible();
    expect(screen.getByText("Ready commons")).toBeVisible();
    expect(screen.getByText("Dashed means order unknown")).toBeVisible();
    expect(screen.queryByText("Machines")).not.toBeInTheDocument();
  });

  it("marks the plant with its lens and motion mode for both themes", async () => {
    window.location.hash = "#/factory?layout=plant&lens=risk";
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    const plant = await screen.findByRole("group", { name: PLANT_LAYOUT });
    expect(plant).toHaveAttribute("data-lens", "risk");
    expect(plant).toHaveAttribute("data-motion", "full");
    expect(plant).toHaveAttribute("data-responsive-layout", "fit");
    // Theme is carried by tokens, so the plant never hard-codes a palette.
    const pageBlock = portalStyles.slice(
      portalStyles.indexOf(".factory-page {"),
      portalStyles.indexOf(".factory-heading {"),
    );
    expect(pageBlock).toContain("color: var(--ink)");
    const plantSection = portalStyles.slice(
      portalStyles.indexOf("The plant, isometric"),
      portalStyles.indexOf("/* --- Responsive"),
    );
    expect(plantSection).not.toMatch(/#[0-9a-fA-F]{6}\b/);
  });
});

class IdlePlantClient extends FixtureDaemonClient {
  override listRuns(
    request?: RunListOptions,
    options?: RequestOptions,
  ): Promise<RunList> {
    if (request?.phase === "running") {
      return Promise.resolve({ runs: [] });
    }
    return super.listRuns(request, options);
  }
}

class UnreadSignalClient extends FixtureDaemonClient {
  constructor(
    fixtures: ReturnType<typeof factoryFloorFixtures>,
    private readonly unreadRunId: string,
  ) {
    super(fixtures);
  }

  override listStageAttempts(
    runId: string,
    stage: string,
    options?: RequestOptions,
  ) {
    if (runId === this.unreadRunId) {
      return Promise.reject(new Error("synthetic unread signal"));
    }
    return super.listStageAttempts(runId, stage, options);
  }
}

class TruncatedRunsClient extends FixtureDaemonClient {
  override async listRuns(
    request?: RunListOptions,
    options?: RequestOptions,
  ): Promise<RunList> {
    const result = await super.listRuns(request, options);
    return request?.phase === "running"
      ? { ...result, nextCursor: "more-active-runs" }
      : result;
  }
}

/** A fixture client whose event stream and run list the test drives. */
class LivePlantClient extends FixtureDaemonClient {
  private readers: ((result: IteratorResult<DaemonUpdateEvent>) => void)[] = [];

  constructor(private readonly live: ReturnType<typeof factoryFloorFixtures>) {
    super(live);
  }

  connectEvents(
    _request?: EventStreamRequest,
    _options?: RequestOptions,
  ): Promise<DaemonEventStream> {
    const self = this;
    return Promise.resolve({
      close: () => {},
      [Symbol.asyncIterator]: () => ({
        next: () =>
          new Promise<IteratorResult<DaemonUpdateEvent>>((resolve) =>
            self.readers.push(resolve),
          ),
      }),
    } as DaemonEventStream);
  }

  moveRun(runId: string, stage: string): void {
    this.live.runs.runs = this.live.runs.runs.map((run: RunSummary) =>
      run.id === runId ? { ...run, currentStage: stage, lastSeq: run.lastSeq + 1 } : run,
    );
  }

  push(runId: string, id: string): void {
    this.readers.shift()?.({
      done: false,
      value: {
        id,
        type: "update",
        data: { cursor: id, models: ["run"], runIds: [runId], workflows: [] },
      } as unknown as DaemonUpdateEvent,
    });
  }
}

function denseStageFixtures(count: number): ReturnType<typeof factoryFloorFixtures> {
  const fixtures = factoryFloorFixtures();
  const template = fixtures.runs.runs.find((run) => run.id === "01JZ500BLOCKED")!;
  const attemptTemplate = fixtures.stageAttempts?.[fixtureKey(template.id, "implement")]!;
  const runs = Array.from({ length: count }, (_, index) => ({
    ...template,
    id: `01DENSE${String(index).padStart(3, "0")}`,
    startedAt: new Date(Date.parse(template.startedAt) + index * 1_000).toISOString(),
    lastActivityAt: new Date(
      Date.parse(template.lastActivityAt) + index * 1_000,
    ).toISOString(),
  }));
  fixtures.runs = { runs };
  fixtures.stageAttempts = Object.fromEntries(
    runs.map((run) => [
      fixtureKey(run.id, "implement"),
      {
        ...structuredClone(attemptTemplate),
        runId: run.id,
        attempts: attemptTemplate.attempts.map((attempt) => ({
          ...attempt,
          id: `${run.id}-attempt`,
          status: "running" as const,
          error: undefined,
        })),
      },
    ]),
  );
  return fixtures;
}

function denseInboundFixtures(count: number): ReturnType<typeof factoryFloorFixtures> {
  const fixtures = denseStageFixtures(count);
  fixtures.runs = {
    runs: fixtures.runs.runs.map((run) => ({ ...run, currentStage: undefined })),
  };
  fixtures.stageAttempts = {};
  return fixtures;
}

function denseCommonsFixtures(count: number): ReturnType<typeof factoryFloorFixtures> {
  const fixtures = factoryFloorFixtures();
  const template = fixtures.goobers!.core.items[0];
  fixtures.runs = { runs: [] };
  fixtures.goobers!.core = {
    ...fixtures.goobers!.core,
    items: Array.from({ length: count }, (_, index) => ({
      ...structuredClone(template),
      name: `ready-${String(index).padStart(2, "0")}`,
      displayName: `Ready ${String(index).padStart(2, "0")}`,
      stages: [],
    })),
  };
  return fixtures;
}

function manyWorkflowFixtures(
  count: number,
  activeIndex: number,
): ReturnType<typeof factoryFloorFixtures> {
  const fixtures = factoryFloorFixtures();
  const summaryTemplate = fixtures.workflows!.core.items[0];
  const detailTemplate = fixtures.workflowDetails![fixtureKey("core", "implementation")];
  const summaries = Array.from({ length: count }, (_, index) => {
    const name = `flow-${String(index).padStart(2, "0")}`;
    return {
      ...structuredClone(summaryTemplate),
      identity: { gaggle: "core", name },
      displayName: `Flow ${String(index).padStart(2, "0")}`,
      concurrency: { ...summaryTemplate.concurrency, activeRuns: 0 },
    };
  });
  fixtures.workflows!.core = { ...fixtures.workflows!.core, items: summaries };
  fixtures.workflowDetails = {
    ...fixtures.workflowDetails,
    ...Object.fromEntries(
      summaries.map((summary) => [
        fixtureKey("core", summary.identity.name),
        {
          ...structuredClone(detailTemplate),
          ...summary,
          graph: {
            ...structuredClone(detailTemplate.graph),
            name: summary.identity.name,
          },
        },
      ]),
    ),
  };
  const runTemplate = fixtures.runs.runs.find((run) => run.id === "01JZ500BLOCKED")!;
  const run = {
    ...runTemplate,
    id: "01ACTIVEFLOW",
    workflow: summaries[activeIndex].identity.name,
  };
  fixtures.runs = { runs: [run] };
  const attemptTemplate = fixtures.stageAttempts?.[
    fixtureKey(runTemplate.id, "implement")
  ]!;
  fixtures.stageAttempts = {
    [fixtureKey(run.id, "implement")]: {
      ...structuredClone(attemptTemplate),
      runId: run.id,
      attempts: attemptTemplate.attempts.map((attempt) => ({
        ...attempt,
        id: `${run.id}-attempt`,
        status: "running" as const,
        error: undefined,
      })),
    },
  };
  return fixtures;
}
