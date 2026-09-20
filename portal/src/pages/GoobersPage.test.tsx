import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient } from "../api/fixtureClient";
import { emptyDaemonFixtures, populatedDaemonFixtures } from "../test/daemonFixtures";

const portalStyles = readFileSync("src/styles.css", "utf8");

describe("goobers roster page", () => {
  it("is reachable from primary nav and lists every goober across gaggles", async () => {
    window.location.hash = "#/overview";
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    await userEvent.click(await screen.findByRole("button", { name: "Goobers" }));

    expect(await screen.findByRole("heading", { name: "Goobers" })).toBeInTheDocument();
    expect(window.location.hash).toBe("#/goobers");
    expect(screen.queryByText("Core implementer")).not.toBeInTheDocument();
    expect(screen.queryByText("Tools implementer")).not.toBeInTheDocument();
    expect(screen.queryByText("2 goobers")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Core product/ }))
      .toHaveAttribute("aria-expanded", "false");
    expect(screen.getByRole("button", { name: /Developer tools/ }))
      .toHaveAttribute("aria-expanded", "false");
    expect(portalStyles).toMatch(
      /\.goober-roster-page\s*\{[^}]*grid-template-columns:\s*minmax\(0,\s*1fr\)/s,
    );
    expect(portalStyles).toMatch(
      /\.goober-card-toggle-label\s*\{[^}]*display:\s*flex/s,
    );
    expect(portalStyles).toMatch(
      /\.goober-card \.goober-summary\s*\{[^}]*grid-template-columns:\s*180px\s+minmax\(0,\s*1fr\)/s,
    );
    expect(portalStyles).toMatch(
      /\.goober-card \.goober-summary\s*\{[^}]*gap:\s*12px\s+48px/s,
    );
    expect(portalStyles).toMatch(
      /\.goober-summary-basics\s*\{[^}]*gap:\s*10px/s,
    );
    expect(portalStyles).toMatch(
      /\.goober-detail \.property-list div\s*\{[^}]*grid-template-columns:\s*120px\s+minmax\(0,\s*1fr\)/s,
    );
  });

  it("filters by owning gaggle and keeps the group disclosure keyboard accessible", async () => {
    window.location.hash = "#/goobers?gaggle=core";
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    const summary = await screen.findByRole("button", { name: /Core product/ });
    expect(summary).toHaveClass("definition-disabled");
    expect(within(summary).getByText("Disabled")).toBeInTheDocument();
    expect(summary).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByText("Core implementer")).not.toBeInTheDocument();
    expect(screen.queryByText("Tools implementer")).not.toBeInTheDocument();

    summary.focus();
    await userEvent.keyboard("{Enter}");
    expect(summary).toHaveAttribute("aria-expanded", "true");
    expect(
      screen.getByRole("region", { name: "Core product goober personas" }),
    ).toBeInTheDocument();
    expect(screen.getByText("Core implementer")).toBeInTheDocument();
    expect(screen.getByText("core/implementer")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "View all gaggles" })).toHaveAttribute(
      "href",
      "#/goobers",
    );
  });

  it("expands a card to reveal detail and toggle to raw YAML", async () => {
    window.location.hash = "#/goobers";
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    await userEvent.click(await screen.findByRole("button", { name: /Core product/ }));
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
    const card = toggle.closest("article");
    if (!(card instanceof HTMLElement)) {
      throw new Error("Expanded Goober card was not rendered.");
    }
    expect(
      within(card).getByText("Implements claimed backlog items end to end."),
    ).toBeInTheDocument();
    expect(
      within(panel).queryByText("Implements claimed backlog items end to end."),
    ).not.toBeInTheDocument();
    expect(
      within(card).getByText(/core\/implementation\/implement \(agentic\)/),
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
