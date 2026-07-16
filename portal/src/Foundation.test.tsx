import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "./App";
import { parseRoute, routeHash } from "./foundation/navigation";
import { themeStorageKey } from "./foundation/theme";

describe("portal foundations", () => {
  const storedValues = new Map<string, string>();

  beforeEach(() => {
    storedValues.clear();
    Object.defineProperty(window, "localStorage", {
      configurable: true,
      value: {
        clear: () => storedValues.clear(),
        getItem: (key: string) => storedValues.get(key) ?? null,
        removeItem: (key: string) => storedValues.delete(key),
        setItem: (key: string, value: string) => storedValues.set(key, value),
      },
    });
    delete document.documentElement.dataset.theme;
    window.history.replaceState(null, "", "#/overview");
  });

  it.each([
    ["#/overview", "One run needs attention."],
    ["#/workflows", "Workflows"],
    ["#/runs", "Runs"],
  ])("renders the %s shell route from static fixtures", (hash, heading) => {
    window.history.replaceState(null, "", hash);
    render(<App />);

    expect(screen.getByRole("heading", { name: heading, level: 1 })).toBeInTheDocument();
    expect(screen.getByText("Static fixtures")).toBeInTheDocument();
    expect(screen.getByRole("navigation", { name: "Primary" })).toBeInTheDocument();
  });

  it("parses and formats shell and detail routes", () => {
    expect(parseRoute("#/workflows")).toEqual({ page: "workflows" });
    expect(parseRoute("#/workflow/implementation")).toEqual({
      page: "workflow",
      id: "implementation",
    });
    expect(parseRoute("#/run/01JZ455ESCALATE")).toEqual({
      page: "run",
      id: "01JZ455ESCALATE",
    });
    expect(routeHash({ page: "runs" })).toBe("#/runs");
    expect(routeHash({ page: "run", id: "01JZ455ESCALATE" })).toBe("#/run/01JZ455ESCALATE");
  });

  it("restores and persists independently selected themes", async () => {
    window.localStorage.setItem(themeStorageKey, "dark");
    const user = userEvent.setup();
    render(<App />);

    expect(document.documentElement).toHaveAttribute("data-theme", "dark");
    await user.click(screen.getByRole("button", { name: "Use light theme" }));

    await waitFor(() => {
      expect(document.documentElement).toHaveAttribute("data-theme", "light");
      expect(window.localStorage.getItem(themeStorageKey)).toBe("light");
    });
  });

  it("supports keyboard navigation, route focus, and content skipping", async () => {
    const user = userEvent.setup();
    render(<App />);

    const workflowsButton = screen.getByRole("button", { name: /Workflows/ });
    workflowsButton.focus();
    await user.keyboard("{Enter}");

    expect(await screen.findByRole("heading", { name: "Workflows", level: 1 })).toBeInTheDocument();
    expect(workflowsButton).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("main")).toHaveFocus();

    const skipButton = screen.getByRole("button", { name: "Skip to content" });
    skipButton.focus();
    await user.keyboard("{Enter}");
    expect(screen.getByRole("main")).toHaveFocus();
  });

  it("keeps responsive shell classes and accessible action names", () => {
    const { container } = render(<App />);

    expect(container.querySelector(".portal-frame")).toBeInTheDocument();
    expect(container.querySelector(".sidebar .primary-nav")).toBeInTheDocument();
    for (const button of screen.getAllByRole("button")) {
      expect(button).toHaveAccessibleName();
    }
  });

  it("makes filters and dismiss controls perform their visible actions", async () => {
    const user = userEvent.setup();
    const { rerender } = render(<App />);

    await user.click(screen.getByRole("button", { name: "Dismiss warning" }));
    expect(screen.queryByText("Config changed since daemon start")).not.toBeInTheDocument();

    window.location.hash = "#/runs";
    fireEvent(window, new HashChangeEvent("hashchange"));
    rerender(<App />);
    const activeFilter = await screen.findByRole("button", { name: "active" });
    await user.click(activeFilter);

    expect(activeFilter).toHaveAttribute("aria-pressed", "true");
    expect(screen.getAllByRole("button", { name: /Open run/ })).toHaveLength(1);
  });

  it("exposes graph kind, state, and keyboard selection without relying on color", async () => {
    const user = userEvent.setup();
    window.history.replaceState(null, "", "#/run/01JZ455ESCALATE");
    render(<App />);

    expect(
      screen.getByRole("group", { name: "Implementation execution graph" }),
    ).toHaveAccessibleDescription(/Outgoing:/);
    const reviewGate = screen.getByRole("button", { name: /Review gate, gate, complete/ });
    reviewGate.focus();
    await user.keyboard("{Enter}");

    expect(screen.getByRole("complementary", { name: "Review gate attempt inspector" })).toBeInTheDocument();
    expect(screen.getAllByText("complete", { selector: ".graph-node-state" })).not.toHaveLength(0);
  });
});
