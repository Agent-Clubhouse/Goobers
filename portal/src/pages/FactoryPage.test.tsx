import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { act } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import portalStyles from "../styles.css?raw";
import { App } from "../App";
import { DaemonUnavailableError } from "../api/errors";
import { FixtureDaemonClient, fixtureKey } from "../api/fixtureClient";
import type {
  AttemptList,
  DaemonEventStream,
  DaemonUpdateEvent,
  EventStreamRequest,
  RequestOptions,
  RunList,
  RunListOptions,
  RunSummary,
} from "../api/types";
import {
  emptyDaemonFixtures,
  factoryFloorFixtures,
  populatedDaemonFixtures,
} from "../test/daemonFixtures";

/**
 * The Factory Floor renders the daemon's own plant. These tests assert what an
 * operator standing at the window must be able to trust: the buildings are the
 * real workflows, the crates are the real active runs on their real stages, a
 * blocked stage raises an accessible alarm, and a live stage change moves that
 * one run rather than rebuilding the world.
 */

beforeEach(() => {
  window.location.hash = "#/factory";
});

describe("factory floor route", () => {
  it("renders the Factory as a first-class primary area", async () => {
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    expect(await screen.findByRole("heading", { name: "Factory" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Factory" })).toHaveAttribute(
      "aria-current",
      "page",
    );
    expect(screen.getByRole("group", { name: "Factory floor" })).toBeInTheDocument();
  });

  it("navigates to the factory from the primary nav", async () => {
    window.location.hash = "#/overview";
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    await user.click(await screen.findByRole("button", { name: "Factory" }));

    expect(await screen.findByRole("heading", { name: "Factory" })).toBeInTheDocument();
    expect(window.location.hash).toBe("#/factory");
  });

  it("shows the real gaggles, workflow lines, stages, goobers and active runs", async () => {
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    const floor = await screen.findByRole("group", { name: "Factory floor" });
    expect(
      within(floor).getByRole("button", { name: /^Workflow Implementation, gaggle Core product/ }),
    ).toBeInTheDocument();
    expect(
      within(floor).getByRole("button", {
        name: /^Workflow Implementation, gaggle Developer tools/,
      }),
    ).toBeInTheDocument();

    // Real declared stages, not an invented Intake/Build/Ship taxonomy.
    expect(
      within(floor).getByRole("button", {
        name: /^Stage query\. Deterministic stage\. workflow Implementation\. gaggle core/,
      }),
    ).toBeInTheDocument();
    expect(
      within(floor).getByRole("button", { name: /^Stage review\. Gate\. workflow Implementation\. gaggle core/ }),
    ).toBeInTheDocument();

    // Real active runs, each standing on the stage the daemon reports.
    expect(
      within(floor).getByRole("button", { name: /^Run 01JZ441DAEMONAPI.*at stage review/ }),
    ).toBeInTheDocument();
    expect(
      within(floor).getByRole("button", { name: /^Run 01JZ500BLOCKED.*at stage implement/ }),
    ).toBeInTheDocument();
    expect(
      within(floor).getByRole("button", { name: /^Goober Core implementer/ }),
    ).toBeInTheDocument();

    // The rail's default view counts the same real population.
    const rail = screen.getByRole("complementary", { name: "Factory inspector" });
    expect(within(rail).getByRole("heading", { name: "Live plant summary" })).toBeInTheDocument();
    expect(within(rail).getByText("Active runs").closest("div")).toHaveTextContent("4");
    expect(within(rail).getByText("Blocked stages").closest("div")).toHaveTextContent("1");
    expect(within(rail).getByText("Human holds").closest("div")).toHaveTextContent("1");
  });

  it("scopes the floor to one gaggle without inventing anything", async () => {
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    await screen.findByRole("group", { name: "Factory floor" });
    await user.selectOptions(screen.getByLabelText("Gaggle"), "tools");

    await waitFor(() =>
      expect(
        screen.queryByRole("button", { name: /^Workflow Implementation, gaggle Core product/ }),
      ).not.toBeInTheDocument(),
    );
    expect(
      screen.getByRole("button", { name: /^Workflow Implementation, gaggle Developer tools/ }),
    ).toBeInTheDocument();
    expect(window.location.hash).toBe("#/factory?gaggle=tools");
  });

  it("applies and clears workflow scope from the floor controls", async () => {
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    await screen.findByRole("group", { name: "Factory floor" });
    await user.selectOptions(screen.getByLabelText("Workflow"), "implementation");
    await waitFor(() =>
      expect(window.location.hash).toBe("#/factory?workflow=implementation"),
    );
    expect(
      screen.getAllByRole("button", { name: /^Workflow Implementation/ }),
    ).toHaveLength(2);

    await user.selectOptions(screen.getByLabelText("Workflow"), "");
    await waitFor(() => expect(window.location.hash).toBe("#/factory"));
  });

  it("falls back safely when the route names a gaggle that is not configured", async () => {
    window.location.hash = "#/factory?gaggle=ghost";
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    expect(
      await screen.findByText(/Gaggle "ghost" is not configured on this instance/),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /^Workflow Implementation, gaggle Core product/ }),
    ).toBeInTheDocument();
  });

  it("marks every headline and readout partial when the active list is truncated", async () => {
    render(<App client={new TruncatedFactoryClient(denseStageFixtures(50))} />);

    await screen.findByRole("group", { name: "Factory floor" });
    const status = screen.getByRole("region", { name: "Factory status" });
    expect(within(status).getByText("Plant state").closest("div")).toHaveTextContent(
      "Partial view",
    );
    expect(within(status).getByText("Work in progress").closest("div")).toHaveTextContent(
      "50+",
    );
    expect(within(status).getByText("Floor capacity").closest("div")).toHaveTextContent(
      "50+ active",
    );
    expect(screen.getByText(/50\+ active.*partial/)).toBeVisible();

    const rail = screen.getByRole("complementary", { name: "Factory inspector" });
    expect(within(rail).getByText("Active runs").closest("div")).toHaveTextContent(
      "50+partial",
    );
    expect(within(rail).getByText("Held runs").closest("div")).toHaveTextContent(/\d+\+/);
    expect(within(rail).getByText("Signals unread").closest("div")).toHaveTextContent(/\d+\+/);
    expect(within(rail).getByText("Human holds").closest("div")).toHaveTextContent(/\d+\+/);
    expect(within(rail).getByText("Blocked stages").closest("div")).toHaveTextContent(/\d+\+/);
    expect(within(rail).getByText("Floor capacity").closest("div")).toHaveTextContent(
      "partial view",
    );
  });

  it("labels workflows beyond the topology detail batch without drawing a blank line", async () => {
    render(
      <App client={new FixtureDaemonClient(manyWorkflowPortalFixtures(13, 12))} />,
    );

    const floor = await screen.findByRole("group", { name: "Factory floor" });
    const unreadLane = within(floor).getByRole("button", {
      name: /^Workflow Flow 11, gaggle Core product/,
    });
    expect(unreadLane).toHaveAccessibleName(
      /Workflow topology was not read in this batch. 3 stages are configured and none are drawn/,
    );
    expect(within(unreadLane).getByText("topology not read in this batch")).toBeVisible();
    expect(within(unreadLane).getByText("3 configured, 0 drawn")).toBeVisible();

    expect(
      within(floor).getByRole("button", {
        name: /^Stage implement\. Agentic stage\. workflow Flow 12/,
      }),
    ).toBeInTheDocument();
  });
});

describe("factory floor inspection", () => {
  it("inspects a run, its stage and its goober from the floor", async () => {
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    const floor = await screen.findByRole("group", { name: "Factory floor" });
    const rail = screen.getByRole("complementary", { name: "Factory inspector" });

    await user.click(
      within(floor).getByRole("button", { name: /^Run 01JZ500BLOCKED/ }),
    );
    expect(within(rail).getByRole("heading", { name: "01JZ500BLOCKED" })).toBeInTheDocument();
    expect(within(rail).getByText("Stage reported blocked")).toBeInTheDocument();
    expect(within(rail).getByRole("link", { name: /Open run/ })).toHaveAttribute(
      "href",
      "#/run/01JZ500BLOCKED",
    );
    // Safe identifiers only: no raw trigger ref, no journal text.
    expect(rail).not.toHaveTextContent("trigger-ref-should-not-render");
    expect(rail).not.toHaveTextContent("free-form");

    await user.click(
      within(floor).getByRole("button", {
        name: /^Stage implement\. Agentic stage\. workflow Implementation\. gaggle core/,
      }),
    );
    expect(within(rail).getByRole("heading", { name: "implement" })).toBeInTheDocument();
    expect(within(rail).getByText("WIP 1 / workflow limit 2")).toBeInTheDocument();
    expect(within(rail).getByRole("link", { name: /Runs at this stage/ })).toHaveAttribute(
      "href",
      "#/runs?gaggle=core&workflow=implementation&stage=implement",
    );

    await user.click(within(floor).getByRole("button", { name: /^Goober Core implementer/ }));
    expect(
      within(rail).getByRole("heading", { name: "Core implementer" }),
    ).toBeInTheDocument();
    expect(within(rail).getByText("copilot")).toBeInTheDocument();
    expect(within(rail).getByRole("link", { name: /Open gaggle/ })).toHaveAttribute(
      "href",
      "#/gaggle/core",
    );
    // Capabilities and skills are deliberately not dumped into the visual.
    expect(rail).not.toHaveTextContent("repo:push");
  });

  it("separates a hard block from a human hold and keeps stage load explicit", async () => {
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    const floor = await screen.findByRole("group", { name: "Factory floor" });
    const blocked = within(floor).getByRole("button", {
      name: /^Stage implement\..*gaggle core.*Blocked\. WIP 1 \/ workflow limit 2\. blocked alarm/,
    });

    expect(blocked).toHaveAttribute("data-alarm", "blocked");
    expect(blocked).toHaveAttribute("data-status", "blocked");
    expect(within(blocked).getByText("Alarm: stage blocked")).toBeInTheDocument();
    expect(blocked.querySelector(".factory-gauge-readout")?.textContent).toBe("WIP 1");
    expect(blocked.querySelector(".factory-gauge-track")).not.toBeInTheDocument();
    expect(within(blocked).getByText("BLOCKED")).toBeVisible();

    // A gate paused by a human uses a distinct amber hold alarm.
    const paused = within(floor).getByRole("button", {
      name: /^Stage review\..*gaggle tools.*Human hold/,
    });
    expect(paused).toHaveAttribute("data-alarm", "hold");
    expect(paused).toHaveAttribute("data-status", "held");
    expect(within(paused).getByText("HOLD")).toBeVisible();
    expect(within(paused).getByText("Alarm: human gate hold")).toBeInTheDocument();

    const lane = within(floor).getByRole("button", {
      name: /^Workflow Implementation, gaggle Core product/,
    });
    expect(lane).toHaveAccessibleName(/workflow limit 2/);
    expect(within(lane).getByText(/workflow limit 2/)).toBeVisible();

    // A stage whose last attempt merely failed is NOT blocked.
    const retrying = within(floor).getByRole("button", {
      name: /^Stage implement\..*gaggle tools/,
    });
    expect(retrying).toHaveAttribute("data-alarm", "off");
    expect(retrying).toHaveAttribute("data-status", "running");
  });

  it("supports keyboard selection with a visible inspector update", async () => {
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    const station = await screen.findByRole("button", {
      name: /^Stage query\. Deterministic stage\. workflow Implementation\. gaggle core/,
    });
    station.focus();
    await user.keyboard("{Enter}");

    expect(station).toHaveAttribute("aria-pressed", "true");
    expect(
      within(screen.getByRole("complementary", { name: "Factory inspector" }))
        .getByRole("heading", { name: "query" }),
    ).toBeInTheDocument();
  });

  it("caps dense stage and inbound carriers behind keyboard-accessible aggregates", async () => {
    const user = userEvent.setup();
    const { unmount } = render(
      <App client={new FixtureDaemonClient(denseStageFixtures(50))} />,
    );

    let floor = await screen.findByRole("group", { name: "Factory floor" });
    expect(within(floor).getAllByRole("button", { name: /^Run / })).toHaveLength(6);
    const stageOverflow = within(floor).getByRole("button", {
      name: /44 additional runs at stage implement/,
    });
    stageOverflow.focus();
    await user.keyboard("{Enter}");
    expect(
      within(screen.getByRole("complementary", { name: "Factory inspector" }))
        .getByRole("heading", { name: "implement" }),
    ).toBeInTheDocument();
    unmount();

    render(<App client={new FixtureDaemonClient(denseInboundFixtures(10))} />);
    floor = await screen.findByRole("group", { name: "Factory floor" });
    expect(within(floor).getAllByRole("button", { name: /^Run / })).toHaveLength(4);
    const inboundOverflow = within(floor).getByRole("button", {
      name: /6 additional runs waiting at inbound/,
    });
    inboundOverflow.focus();
    await user.keyboard("{Enter}");
    expect(
      within(screen.getByRole("complementary", { name: "Factory inspector" }))
        .getByRole("heading", { name: "Implementation" }),
    ).toBeInTheDocument();
  });

  it("caps idle commons workers and keeps the configured floor visible", async () => {
    render(<App client={new FixtureDaemonClient(denseCommonsFixtures(20))} />);

    const floor = await screen.findByRole("group", { name: "Factory floor" });
    expect(screen.getByText("Plant ready. No active runs are on the floor.")).toBeVisible();
    expect(within(floor).getAllByRole("button", { name: /^Goober / })).toHaveLength(12);
    expect(
      within(floor).getByRole("button", {
        name: /8 additional ready goobers. Select the floor summary/,
      }),
    ).toBeVisible();
    expect(
      within(floor).getByRole("button", {
        name: /^Workflow Implementation, gaggle Core product/,
      }),
    ).toBeVisible();
  });

  it("keeps lane identity sticky and keyboard focusable during horizontal scroll", async () => {
    render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);

    const floor = await screen.findByRole("group", { name: "Factory floor" });
    const lane = within(floor).getByRole("button", {
      name: /^Workflow Implementation, gaggle Core product/,
    });
    lane.focus();

    expect(lane).toHaveFocus();
    expect(lane.closest(".factory-lane-plaque-row")).toBeInTheDocument();
    const plaqueBlock = portalStyles.slice(
      portalStyles.indexOf(".factory-lane-plaque {"),
      portalStyles.indexOf(".factory-lane-plaque:hover"),
    );
    expect(plaqueBlock).toContain("position: sticky");
    expect(plaqueBlock).toContain("left: 12px");
  });

  it("keeps the alarm legible and stops the motion under reduced motion", async () => {
    // jsdom ships no matchMedia, so the hook's media query is installed here.
    Object.defineProperty(window, "matchMedia", {
      configurable: true,
      value: (query: string) =>
        ({
          matches: query.includes("prefers-reduced-motion"),
          media: query,
          onchange: null,
          addEventListener: () => {},
          removeEventListener: () => {},
          addListener: () => {},
          removeListener: () => {},
          dispatchEvent: () => false,
        }) as unknown as MediaQueryList,
    });

    try {
      render(<App client={new FixtureDaemonClient(factoryFloorFixtures())} />);
      const floor = await screen.findByRole("group", { name: "Factory floor" });

      expect(floor).toHaveAttribute("data-motion", "reduced");
      const blocked = within(floor).getByRole("button", {
        name: /^Stage implement\..*gaggle core.*Blocked/,
      });
      // Still unmistakably blocked: the state is text and data, not motion.
      expect(blocked).toHaveAttribute("data-alarm", "blocked");
      expect(within(blocked).getByText("Alarm: stage blocked")).toBeInTheDocument();
    } finally {
      Reflect.deleteProperty(window, "matchMedia");
    }
  });

  it("keeps a full-block alarm off when any run signal is unread", async () => {
    const fixtures = factoryFloorFixtures();
    const blocked = fixtures.runs.runs.find(
      (run) => run.id === "01JZ500BLOCKED",
    )!;
    const unread = {
      ...blocked,
      id: "01JZ510UNREAD",
      startedAt: "2026-07-18T06:35:00Z",
      lastActivityAt: "2026-07-18T06:36:00Z",
    };
    fixtures.runs.runs.push(unread);
    const client = new UnknownSignalClient(fixtures, unread.id);
    render(<App client={client} />);

    const floor = await screen.findByRole("group", { name: "Factory floor" });
    const station = within(floor).getByRole("button", {
      name: /^Stage implement\..*gaggle core/,
    });
    expect(station).toHaveAttribute("data-status", "unknown");
    expect(station).toHaveAttribute("data-alarm", "off");
    expect(station).toHaveAccessibleName(/1 run signals unread/);

    const rail = screen.getByRole("complementary", { name: "Factory inspector" });
    expect(within(rail).getByText("Signals unread").closest("div")).toHaveTextContent(
      "1",
    );
    const paused = within(floor).getByRole("button", {
      name: /^Stage review\..*gaggle tools.*Human hold/,
    });
    expect(paused).toHaveAttribute("data-alarm", "hold");
  });

  // jsdom does not evaluate media queries, so the reduced-motion contract is
  // asserted against the stylesheet itself: the compact beacon must stop
  // animating, and carriers must jump rather than slide.
  it("declares a reduced-motion contract for the compact beacon", () => {
    const factoryBlocks = portalStyles
      .slice(portalStyles.indexOf("Factory Floor (#/factory)"))
      .split("@media (prefers-reduced-motion: reduce)");

    expect(factoryBlocks).toHaveLength(2);
    const reduced = factoryBlocks[1];
    expect(reduced).toContain(".factory-alarm-beacon");
    expect(reduced).toContain("animation: none !important");
    expect(reduced).toContain("opacity: 1 !important");
    expect(reduced).toContain(".factory-carrier");
    expect(reduced).toContain("transition: none !important");
    // Only the compact beacon blinks. Large washes and borders stay static.
    expect(factoryBlocks[0]).toContain("steps(1, end) infinite");
    expect(factoryBlocks[0]).toContain("opacity: 0;");
    const washBlock = factoryBlocks[0].slice(
      factoryBlocks[0].indexOf(".factory-alarm-wash"),
      factoryBlocks[0].indexOf("/* --- Work carriers"),
    );
    expect(washBlock).not.toContain("animation:");
  });
});

describe("factory floor liveness", () => {
  it("updates topology incrementally without losing floor scroll or focus", async () => {
    const fixtures = factoryFloorFixtures();
    const client = new LiveFactoryClient(fixtures);
    render(<App client={client} />);

    const floor = await screen.findByRole("group", { name: "Factory floor" });
    const query = within(floor).getByRole("button", {
      name: /^Stage query\. Deterministic stage\. workflow Implementation\. gaggle core/,
    });
    floor.scrollLeft = 180;
    query.focus();

    client.addStage("core", "implementation", "verify");
    await act(async () => {
      client.pushWorkflow("core", "implementation", "workflow:topology-change");
      await new Promise((resolve) => setTimeout(resolve, 80));
    });

    expect(
      await screen.findByRole("button", {
        name: /^Stage verify\. Deterministic stage\. workflow Implementation\. gaggle core/,
      }),
    ).toBeInTheDocument();
    const updatedFloor = screen.getByRole("group", { name: "Factory floor" });
    expect(updatedFloor).toBe(floor);
    expect(updatedFloor.scrollLeft).toBe(180);
    expect(document.activeElement).toBe(query);
  });

  it("moves a run to its new stage on a live invalidation without rebuilding the floor", async () => {
    const fixtures = factoryFloorFixtures();
    const client = new LiveFactoryClient(fixtures);
    const user = userEvent.setup();
    render(<App client={client} />);

    const floor = await screen.findByRole("group", { name: "Factory floor" });
    const rail = screen.getByRole("complementary", { name: "Factory inspector" });
    const before = await within(floor).findByRole("button", {
      name: /^Run 01JZ700RETRY.*at stage implement/,
    });
    const beforeTransform = before.getAttribute("style");
    const stationsBefore = within(floor).getAllByRole("button", { name: /^Stage / }).length;
    await user.click(before);
    expect(within(rail).getByText("implement")).toBeInTheDocument();

    // The run advances a stage in the daemon; nothing else changes.
    client.moveRun("01JZ700RETRY", "review");
    await act(async () => {
      client.push("01JZ700RETRY", "session:stage-change");
      await new Promise((resolve) => setTimeout(resolve, 80));
    });

    const after = await within(floor).findByRole("button", {
      name: /^Run 01JZ700RETRY.*at stage review/,
    });
    await waitFor(() => expect(after).toHaveAttribute("data-moved", "true"));
    expect(after).toHaveClass("is-transitioning");
    expect(after.getAttribute("style")).not.toBe(beforeTransform);
    expect(after.getAttribute("aria-label")).toContain("Moved from implement");
    // Same element identity: the floor was not torn down and rebuilt, which is
    // what lets the crate slide to its new machine instead of teleporting.
    expect(after).toBe(before);
    expect(within(floor).getAllByRole("button", { name: /^Stage / })).toHaveLength(
      stationsBefore,
    );
    // The stage it left is now empty, so its goober walks back to the commons.
    expect(
      within(floor).getByRole("button", { name: /^Goober Tools implementer/ }),
    ).toHaveAccessibleName(/Idle in the ready commons/);
    expect(within(rail).getByText("review")).toBeInTheDocument();
    expect(within(rail).getByText("Running")).toBeInTheDocument();
  });

  it("does not move or animate a survivor when a sibling run leaves", async () => {
    const fixtures = factoryFloorFixtures();
    const survivor = fixtures.runs.runs.find((run) => run.id === "01JZ700RETRY")!;
    const sibling = {
      ...survivor,
      id: "01JZ650SIBLING",
      startedAt: "2026-07-18T06:50:00Z",
      lastActivityAt: "2026-07-18T06:51:00Z",
    };
    fixtures.runs.runs.push(sibling);
    const attempts =
      fixtures.stageAttempts?.[fixtureKey(survivor.id, "implement")];
    fixtures.stageAttempts![fixtureKey(sibling.id, "implement")] = {
      ...structuredClone(attempts!),
      runId: sibling.id,
    };
    const client = new LiveFactoryClient(fixtures);
    render(<App client={client} />);

    const floor = await screen.findByRole("group", { name: "Factory floor" });
    const before = await within(floor).findByRole("button", {
      name: /^Run 01JZ700RETRY.*at stage implement/,
    });
    const transform = before.getAttribute("style");

    client.removeRun(sibling.id);
    await act(async () => {
      client.push(sibling.id, "session:sibling-finished");
      await new Promise((resolve) => setTimeout(resolve, 80));
    });

    const after = await within(floor).findByRole("button", {
      name: /^Run 01JZ700RETRY.*at stage implement/,
    });
    expect(after).toBe(before);
    expect(after.getAttribute("style")).toBe(transform);
    expect(after).toHaveAttribute("data-moved", "false");
    expect(after).not.toHaveClass("is-transitioning");
  });

  it("bounds its reads: one active-run list, one read per workflow, one per visible run", async () => {
    const client = new FixtureDaemonClient(factoryFloorFixtures());
    const listRuns = vi.spyOn(client, "listRuns");
    const getWorkflow = vi.spyOn(client, "getWorkflow");
    const listRunEvents = vi.spyOn(client, "listRunEvents");
    const listStageAttempts = vi.spyOn(client, "listStageAttempts");
    render(<App client={client} />);
    await screen.findByRole("group", { name: "Factory floor" });
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 80));
    });

    // Active work comes from one bounded, server-side phase filter.
    expect(listRuns).toHaveBeenCalledWith(
      expect.objectContaining({ phase: "running", limit: 50 }),
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
    // Topology is read once per workflow, never once per run or per stage.
    expect(new Set(getWorkflow.mock.calls.map((call) => `${call[0]}/${call[1]}`))).toEqual(
      new Set(["core/implementation", "tools/implementation"]),
    );
    // A gate needs its journal (only it records gate.paused); an ordinary stage
    // needs only that one stage's attempts. No run is read both ways.
    expect(new Set(listRunEvents.mock.calls.map((call) => call[0]))).toEqual(
      new Set(["01JZ441DAEMONAPI", "01JZ600PAUSED"]),
    );
    expect(
      new Set(listStageAttempts.mock.calls.map((call) => `${call[0]}/${call[1]}`)),
    ).toEqual(new Set(["01JZ500BLOCKED/implement", "01JZ700RETRY/implement"]));
  });

  it("reports an idle instance honestly instead of drawing imaginary work", async () => {    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    // The populated fixture has exactly one running run and nothing held.
    const rail = await screen.findByRole("complementary", { name: "Factory inspector" });
    expect(within(rail).getByText("Active runs").closest("div")).toHaveTextContent("1");
    expect(within(rail).getByText("Blocked stages").closest("div")).toHaveTextContent("0");
    // Attention lists recent terminal outcomes, and none of them pretend to be WIP.
    const attention = within(rail).getByRole("region", { name: "Attention" });
    expect(
      within(attention).queryByText(/Paused at a human gate|Stage reported blocked/),
    ).not.toBeInTheDocument();
    expect(
      within(attention).getByRole("link", { name: "Open run" }),
    ).toHaveAttribute("href", "#/run/01JZ402DASHBOARD");
  });

  it("does not claim an idle floor before the active-run detail read completes", async () => {
    const fixtures = factoryFloorFixtures();
    fixtures.runs.runs = fixtures.runs.runs.filter((run) => run.phase !== "running");
    const client = new DeferredActiveRunsClient(fixtures);
    render(<App client={client} />);

    expect(
      await screen.findByRole("heading", { name: "Loading factory floor" }),
    ).toBeInTheDocument();
    expect(screen.queryByText("The floor is idle")).not.toBeInTheDocument();
    expect(screen.queryByRole("group", { name: "Factory floor" })).not.toBeInTheDocument();

    client.release();

    const floor = await screen.findByRole("group", { name: "Factory floor" });
    expect(screen.getByText("Plant ready. No active runs are on the floor.")).toBeVisible();
    expect(
      within(floor).getByRole("button", {
        name: /^Workflow Implementation, gaggle Core product/,
      }),
    ).toBeInTheDocument();
    expect(within(floor).getAllByRole("button", { name: /^Stage / }).length).toBeGreaterThan(0);
    expect(
      within(floor).getByRole("button", { name: /^Goober Core implementer/ }),
    ).toHaveAccessibleName(/Idle in the ready commons/);
  });

  it("uses fixed error copy and never renders a daemon error message", async () => {
    const client = new FailingActiveRunsClient(factoryFloorFixtures());
    render(<App client={client} />);

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Factory data unavailable");
    expect(alert).not.toHaveTextContent(FailingActiveRunsClient.sensitiveMessage);
  });

  it("says the plant is empty when the instance has no gaggles", async () => {
    render(<App client={new FixtureDaemonClient(emptyDaemonFixtures())} />);

    expect(
      await screen.findByRole("heading", { name: "No gaggles configured" }),
    ).toBeInTheDocument();
    expect(screen.queryByRole("group", { name: "Factory floor" })).not.toBeInTheDocument();
    expect(screen.getByText(/0 gaggles · 0 workflows/)).toBeInTheDocument();
  });

  it("keeps the last good floor and says so when the daemon degrades", async () => {
    const fixtures = factoryFloorFixtures();
    const client = new LiveFactoryClient(fixtures);
    render(<App client={client} />);
    await screen.findByRole("group", { name: "Factory floor" });

    vi.spyOn(client, "listRuns").mockRejectedValue(new DaemonUnavailableError());
    await act(async () => {
      client.push("01JZ700RETRY", "session:degraded");
      await new Promise((resolve) => setTimeout(resolve, 80));
    });

    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent("Floor refresh failed"),
    );
    // The plant that was last read stays on screen rather than blanking.
    expect(screen.getByRole("group", { name: "Factory floor" })).toBeInTheDocument();
    expect(
      screen.getByRole("complementary", { name: "Factory inspector" }),
    ).toHaveTextContent("Degraded");
  });
});

class UnknownSignalClient extends FixtureDaemonClient {
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
  ): Promise<AttemptList> {
    if (runId === this.unreadRunId) {
      return Promise.reject(new Error("synthetic unread signal"));
    }
    return super.listStageAttempts(runId, stage, options);
  }
}

class TruncatedFactoryClient extends FixtureDaemonClient {
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

function denseStageFixtures(
  count: number,
): ReturnType<typeof factoryFloorFixtures> {
  const fixtures = factoryFloorFixtures();
  const template = fixtures.runs.runs.find(
    (run) => run.id === "01JZ500BLOCKED",
  )!;
  const attemptTemplate =
    fixtures.stageAttempts?.[fixtureKey(template.id, "implement")]!;
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

function denseInboundFixtures(
  count: number,
): ReturnType<typeof factoryFloorFixtures> {
  const fixtures = denseStageFixtures(count);
  fixtures.runs = {
    runs: fixtures.runs.runs.map((run) => ({
      ...run,
      currentStage: undefined,
    })),
  };
  fixtures.stageAttempts = {};
  return fixtures;
}

function denseCommonsFixtures(
  count: number,
): ReturnType<typeof factoryFloorFixtures> {
  const fixtures = factoryFloorFixtures();
  const template = fixtures.goobers!.core.items[0];
  fixtures.runs = { runs: [] };
  fixtures.goobers!.core = {
    ...fixtures.goobers!.core,
    items: Array.from({ length: count }, (_, index) => ({
      ...structuredClone(template),
      name: `ready-${String(index).padStart(2, "0")}`,
      displayName: `Ready ${String(index).padStart(2, "0")}`,
    })),
  };
  fixtures.goobers!.tools = {
    ...fixtures.goobers!.tools,
    items: [],
  };
  return fixtures;
}

function manyWorkflowPortalFixtures(
  count: number,
  activeIndex: number,
): ReturnType<typeof factoryFloorFixtures> {
  const fixtures = factoryFloorFixtures();
  const summaryTemplate = fixtures.workflows!.core.items[0];
  const detailTemplate =
    fixtures.workflowDetails![fixtureKey("core", "implementation")];
  const summaries = Array.from({ length: count }, (_, index) => {
    const name = `flow-${String(index).padStart(2, "0")}`;
    return {
      ...structuredClone(summaryTemplate),
      identity: { gaggle: "core", name },
      displayName: `Flow ${String(index).padStart(2, "0")}`,
      concurrency: { ...summaryTemplate.concurrency, activeRuns: 0 },
    };
  });
  fixtures.workflows!.core = {
    ...fixtures.workflows!.core,
    items: summaries,
  };
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
  const runTemplate = fixtures.runs.runs.find(
    (run) => run.id === "01JZ500BLOCKED",
  )!;
  const run = {
    ...runTemplate,
    id: "01ACTIVEFLOW",
    workflow: summaries[activeIndex].identity.name,
  };
  fixtures.runs = { runs: [run] };
  const attemptTemplate =
    fixtures.stageAttempts?.[fixtureKey(runTemplate.id, "implement")]!;
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

class DeferredActiveRunsClient extends FixtureDaemonClient {
  private releaseGate!: () => void;
  private readonly gate = new Promise<void>((resolve) => {
    this.releaseGate = resolve;
  });

  release(): void {
    this.releaseGate();
  }

  override async listRuns(
    request?: RunListOptions,
    options?: RequestOptions,
  ): Promise<RunList> {
    if (request?.phase === "running") {
      await this.gate;
    }
    return super.listRuns(request, options);
  }
}

class FailingActiveRunsClient extends FixtureDaemonClient {
  static readonly sensitiveMessage =
    "raw daemon failure detail 7f3e should never render";

  override listRuns(
    request?: RunListOptions,
    options?: RequestOptions,
  ): Promise<RunList> {
    if (request?.phase === "running") {
      return Promise.reject(new Error(FailingActiveRunsClient.sensitiveMessage));
    }
    return super.listRuns(request, options);
  }
}

/** A fixture client whose event stream and run list the test drives. */
class LiveFactoryClient extends FixtureDaemonClient {
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

  removeRun(runId: string): void {
    this.live.runs.runs = this.live.runs.runs.filter((run) => run.id !== runId);
  }

  addStage(gaggle: string, workflow: string, stage: string): void {
    const detail = this.live.workflowDetails![fixtureKey(gaggle, workflow)];
    detail.stageCount += 1;
    detail.graph.nodes.push({ id: stage, kind: "deterministic" });
    detail.graph.edges = detail.graph.edges.map((edge) =>
      edge.source === "implement" && edge.target === "review"
        ? { ...edge, target: stage }
        : edge,
    );
    detail.graph.edges.push({ source: stage, target: "review" });
    detail.stages.push({
      name: stage,
      kind: "deterministic",
      goal: "Synthetic verification stage.",
      owner: null,
      evaluator: "",
      capabilities: [],
    });
    const summary = this.live.workflows![gaggle].items.find(
      (candidate) => candidate.identity.name === workflow,
    );
    if (summary) {
      summary.stageCount += 1;
    }
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

  pushWorkflow(gaggle: string, workflow: string, id: string): void {
    this.readers.shift()?.({
      done: false,
      value: {
        id,
        type: "update",
        data: {
          cursor: id,
          models: ["workflow"],
          runIds: [],
          workflows: [{ gaggle, name: workflow }],
        },
      } as unknown as DaemonUpdateEvent,
    });
  }
}
