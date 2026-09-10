import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient } from "../api/fixtureClient";
import { emptyDaemonFixtures, populatedDaemonFixtures } from "../test/daemonFixtures";

describe("goobers roster page", () => {
  it("is reachable from primary nav and lists every goober across gaggles", async () => {
    window.location.hash = "#/overview";
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    await userEvent.click(await screen.findByRole("button", { name: "Goobers" }));

    expect(await screen.findByRole("heading", { name: "Goobers" })).toBeInTheDocument();
    expect(window.location.hash).toBe("#/goobers");
    expect(screen.getByText("Core implementer")).toBeInTheDocument();
    expect(screen.getByText("Tools implementer")).toBeInTheDocument();
    expect(screen.getByText("2 goobers")).toBeInTheDocument();
    expect(
      screen.getByRole("region", { name: "Core product goober personas" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("region", { name: "Developer tools goober personas" }),
    ).toBeInTheDocument();
  });

  it("filters by owning gaggle and keeps the group disclosure keyboard accessible", async () => {
    window.location.hash = "#/goobers?gaggle=core";
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    const roster = await screen.findByRole("region", { name: "Core product goober personas" });
    const disclosure = roster.closest("section");
    const summary = within(disclosure!).getByRole("button", { name: /Core product/ });
    expect(summary).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByText("Core implementer")).toBeInTheDocument();
    expect(screen.queryByText("Tools implementer")).not.toBeInTheDocument();
    expect(screen.getByText("core/implementer")).toBeInTheDocument();

    summary.focus();
    await userEvent.keyboard("{Enter}");
    expect(summary).toHaveAttribute("aria-expanded", "false");
    expect(screen.getByRole("link", { name: "View all gaggles" })).toHaveAttribute(
      "href",
      "#/goobers",
    );
  });

  it("expands a card to reveal detail and toggle to raw YAML", async () => {
    window.location.hash = "#/goobers";
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    const toggle = await screen.findByRole("button", { name: /Core implementer/ });
    expect(toggle).toHaveAttribute("aria-expanded", "false");

    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");

    const detail = screen.getByRole("tablist", { name: "Core implementer config view" });
    expect(within(detail).getByRole("tab", { name: "Fields" })).toHaveAttribute(
      "aria-selected",
      "true",
    );
    const panel = detail.closest(".goober-detail");
    if (!(panel instanceof HTMLElement)) {
      throw new Error("Expanded Goober detail panel was not rendered.");
    }
    expect(
      within(panel).getByText("Implements claimed backlog items end to end."),
    ).toBeInTheDocument();
    expect(
      within(panel).getByText("core/implementation/implement (agentic)"),
    ).toBeInTheDocument();

    await userEvent.click(within(detail).getByRole("tab", { name: "Raw YAML" }));
    expect(within(detail).getByRole("tab", { name: "Raw YAML" })).toHaveAttribute(
      "aria-selected",
      "true",
    );
    const yamlBlock = screen.getByText(/name: implementer/);
    expect(yamlBlock).toBeInTheDocument();
    expect(yamlBlock.textContent).toContain("harness: copilot");
    expect(yamlBlock.textContent).toContain("core/implementation");
  });

  it("shows a ready-empty roster without inventing goobers", async () => {
    window.location.hash = "#/goobers";
    render(<App client={new FixtureDaemonClient(emptyDaemonFixtures())} />);

    expect(await screen.findByRole("heading", { name: "No goobers configured" })).toBeInTheDocument();
    expect(screen.getByText("goobers init --guided")).toBeInTheDocument();
  });
});
