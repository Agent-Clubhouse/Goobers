import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { PortalWorkbench } from "./package";
import { FixtureDaemonClient } from "./api/fixtureClient";
import { emptyDaemonFixtures } from "./test/daemonFixtures";

describe("PortalWorkbench", () => {
  it("renders the existing workbench with an explicit injected client", async () => {
    window.location.hash = "#/overview";
    render(<PortalWorkbench client={new FixtureDaemonClient(emptyDaemonFixtures())} scope="user:instance" />);
    expect(await screen.findByText("Daemon ready")).toBeInTheDocument();
    expect(screen.getByRole("navigation", { name: "Primary" })).toBeInTheDocument();
  });

  it("rejects an empty scope", () => {
    expect(() => PortalWorkbench({
      client: new FixtureDaemonClient(emptyDaemonFixtures()), scope: " ",
    })).toThrow("nonempty cursor scope");
  });
});
