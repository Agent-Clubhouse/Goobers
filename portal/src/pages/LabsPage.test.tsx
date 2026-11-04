import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient } from "../api/fixtureClient";
import { emptyDaemonFixtures } from "../test/daemonFixtures";
import { LabsPage } from "./LabsPage";

describe("LabsPage", () => {
  beforeEach(() => {
    window.history.replaceState(null, "", "/");
  });

  it("lists registered features with maturity and links to each feature page", () => {
    render(<LabsPage />);

    expect(screen.getByRole("heading", { name: "Labs" })).toBeInTheDocument();
    const link = screen.getByRole("link", { name: "Workflow clustering" });
    expect(link).toHaveAttribute("href", "#/labs/workflow-clustering");
    expect(screen.getByText("Concept")).toBeInTheDocument();
    expect(screen.getByText("Coming soon")).toBeInTheDocument();
  });

  it("shows the experimental banner, prerequisites, and tracking issues for a feature", () => {
    render(<LabsPage feature="workflow-clustering" />);

    expect(screen.getByRole("heading", { name: "Workflow clustering" })).toBeInTheDocument();
    expect(screen.getByRole("note")).toHaveTextContent("Labs: experimental.");
    expect(screen.getByRole("heading", { name: "Prerequisites" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "#8302" })).toHaveAttribute(
      "href",
      "https://github.com/Agent-Clubhouse/Goobers/issues/8302",
    );
    expect(screen.getByRole("link", { name: "All Labs features" })).toHaveAttribute(
      "href",
      "#/labs",
    );
  });

  it("falls back to the catalog for an unknown feature", () => {
    render(<LabsPage feature="does-not-exist" />);

    expect(screen.getByRole("heading", { name: "Labs" })).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("No Labs feature named does-not-exist");
  });

  it("is reachable from the primary navigation", async () => {
    window.location.hash = "#/overview";
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(emptyDaemonFixtures())} />);

    await user.click(await screen.findByRole("button", { name: "Labs" }));

    expect(await screen.findByRole("heading", { name: "Labs" })).toBeInTheDocument();
    expect(window.location.hash).toBe("#/labs");
  });
});
