import { render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient } from "../api/fixtureClient";
import { populatedDaemonFixtures } from "../test/daemonFixtures";

beforeEach(() => {
  window.localStorage.clear();
});

describe("instance detail pages", () => {
  it("renders shareable recovery metadata without overflowing values into controls", async () => {
    const fixtures = populatedDaemonFixtures();
    window.location.hash = "#/instance/recovery";

    render(<App client={new FixtureDaemonClient(fixtures)} />);

    expect(await screen.findByRole("heading", { name: "Recovery metadata" })).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "Recovery inventory details" })).toHaveTextContent(
      "12 of 128 recovery slots are occupied",
    );
    expect(screen.getByText(fixtures.instance.instanceRoot + "\\recovery")).toBeInTheDocument();
    expect(screen.getByText(/portal is read-only/i)).toBeInTheDocument();
  });

  it("renders explicit empty recovery and retention states", async () => {
    const fixtures = populatedDaemonFixtures();
    delete fixtures.instance.recoveryInventory;
    window.location.hash = "#/instance/recovery";
    const { unmount } = render(<App client={new FixtureDaemonClient(fixtures)} />);
    expect(await screen.findByRole("heading", { name: "No recovery metadata reported" })).toBeInTheDocument();
    unmount();

    window.location.hash = "#/instance/retention";
    render(<App client={new FixtureDaemonClient(fixtures)} />);
    expect(await screen.findByRole("heading", { name: "No retention metadata reported" })).toBeInTheDocument();
  });

  it("keeps configuration warning details read-only", async () => {
    window.location.hash = "#/instance/warnings";
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    expect(await screen.findByRole("heading", { name: "Configuration warnings", level: 1 })).toBeInTheDocument();
    expect(screen.getAllByText(/portal is read-only/i)).not.toHaveLength(0);
    expect(screen.getByText("VER001")).toBeInTheDocument();
  });
});
