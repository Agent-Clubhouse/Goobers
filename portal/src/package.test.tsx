import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { PortalWorkbench } from "./package";
import { FixtureDaemonClient } from "./api/fixtureClient";
import { emptyDaemonFixtures } from "./test/daemonFixtures";
import { defaultPortalConfig } from "./cobrand";

describe("PortalWorkbench", () => {
  afterEach(() => {
    cleanup();
    document.querySelectorAll("[data-test-header-host]").forEach((element) => element.remove());
    document.getElementById("cobrand-theme")?.remove();
    delete document.documentElement.dataset.theme;
    window.localStorage.clear();
  });

  const createHost = () => {
    const target = document.createElement("div");
    target.dataset.testHeaderHost = "";
    document.body.appendChild(target);
    return target;
  };

  it("renders the existing workbench with an explicit injected client", async () => {
    window.location.hash = "#/overview";
    render(<PortalWorkbench client={new FixtureDaemonClient(emptyDaemonFixtures())} scope="user:instance" />);
    expect(await screen.findByText("Healthy")).toBeInTheDocument();
    expect(screen.getByRole("navigation", { name: "Primary" })).toBeInTheDocument();
    expect(document.querySelectorAll(".topbar")).toHaveLength(1);
    expect(document.querySelector(".portal-frame > .topbar")).toBeInTheDocument();
    expect(document.querySelector(".portal-frame-hosted-header")).not.toBeInTheDocument();
  });

  it("rejects an empty scope", () => {
    expect(() => PortalWorkbench({
      client: new FixtureDaemonClient(emptyDaemonFixtures()), scope: " ",
    })).toThrow("nonempty cursor scope");
  });

  it("passes explicit Fleet host context to the workbench", async () => {
    const { container } = render(
      <PortalWorkbench
        client={new FixtureDaemonClient(emptyDaemonFixtures())}
        mode="fleet"
        scope="user:instance"
      />,
    );

    expect(await screen.findByText("Goobers Fleet")).toBeInTheDocument();
    expect(container.querySelector(".portal-frame")).toHaveAttribute("data-host", "fleet");
  });

  it("hosts exactly one complete header without replacing native controls or instance support", async () => {
    const target = createHost();
    const client = new FixtureDaemonClient(emptyDaemonFixtures());
    vi.spyOn(client, "getPortalConfig").mockResolvedValue({
      ...defaultPortalConfig,
      brand: { ...defaultPortalConfig.brand, name: "Contoso", tagline: "Productivity", logoUrl: "/contoso.png" },
      theme: { ...defaultPortalConfig.theme, accentLight: "#00796b", accentDark: "#80cbc4" },
      support: { ...defaultPortalConfig.support, docsUrl: "https://example.test/docs" },
    });
    const onHelp = vi.fn();
    const view = render(
      <PortalWorkbench client={client} scope="operator:contoso" headerHost={{
        target,
        actions: <button type="button" onClick={onHelp}>Host help</button>,
      }} />,
    );

    expect(await within(target).findByText("Contoso")).toBeInTheDocument();
    expect(target.querySelector(".topbar-hosted-brand-copy small")).toHaveTextContent("Productivity");
    expect(target.querySelector("img")).toHaveAttribute("src", "/contoso.png");
    expect(document.querySelectorAll(".topbar")).toHaveLength(1);
    expect(view.container.querySelector(".portal-frame > .topbar")).not.toBeInTheDocument();
    expect(view.container.querySelector(".portal-frame")).toHaveClass("portal-frame-hosted-header");
    expect(screen.getByRole("link", { name: "Docs" })).toHaveAttribute("href", "https://example.test/docs");
    expect(target.querySelector(".freshness-status")).toHaveAttribute("role", "status");
    fireEvent.click(within(target).getByRole("button", { name: "Host help" }));
    expect(onHelp).toHaveBeenCalledOnce();
    fireEvent.click(within(target).getByRole("button", { name: "Use dark theme" }));
    await waitFor(() => expect(document.documentElement).toHaveAttribute("data-theme", "dark"));
    expect(document.getElementById("cobrand-theme")).toHaveTextContent("#80cbc4");
    fireEvent.click(screen.getByRole("button", { name: "Runs" }));
    await screen.findByRole("heading", { name: "Runs" });
    fireEvent.click(within(target).getByRole("button", { name: "Go to overview" }));
    await screen.findByText("Healthy");
    expect(window.location.hash).toBe("#/overview");
    view.unmount();
    expect(target).toBeEmptyDOMElement();
  });

  it("moves the header between host targets and restores the default layout when hosting is removed", async () => {
    const first = createHost();
    const second = createHost();
    const client = new FixtureDaemonClient(emptyDaemonFixtures());
    const view = render(<PortalWorkbench client={client} scope="operator:instance" headerHost={{ target: first }} />);
    await screen.findByText("Healthy");
    expect(first.querySelector(".topbar")).toBeInTheDocument();
    view.rerender(<PortalWorkbench client={client} scope="operator:instance" headerHost={{ target: second }} />);
    expect(first).toBeEmptyDOMElement();
    expect(second.querySelector(".topbar")).toBeInTheDocument();
    expect(document.querySelectorAll(".topbar")).toHaveLength(1);
    view.rerender(<PortalWorkbench client={client} scope="operator:instance" />);
    expect(second).toBeEmptyDOMElement();
    expect(view.container.querySelector(".portal-frame > .topbar")).toBeInTheDocument();
    expect(view.container.querySelector(".portal-frame-hosted-header")).not.toBeInTheDocument();
  });

  it("rejects a header container from another document", () => {
    const otherDocument = document.implementation.createHTMLDocument();
    expect(() => PortalWorkbench({
      client: new FixtureDaemonClient(emptyDaemonFixtures()),
      scope: "operator:instance",
      headerHost: { target: otherDocument.body },
    })).toThrow("same-document container");
  });
});
