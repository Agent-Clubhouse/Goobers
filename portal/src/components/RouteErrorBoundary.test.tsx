import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { RouteErrorBoundary } from "./RouteErrorBoundary";

function Bomb(): never {
  throw new Error("boom");
}

// #4825: before this component existed, a single page component throwing
// during render unmounted the entire portal — no error boundary existed
// anywhere in the tree.
describe("RouteErrorBoundary", () => {
  it("renders children normally when nothing throws", () => {
    render(
      <RouteErrorBoundary>
        <p>fine</p>
      </RouteErrorBoundary>,
    );
    expect(screen.getByText("fine")).toBeInTheDocument();
  });

  it("degrades to a recoverable card instead of unmounting on a child throw", () => {
    const consoleError = vi.spyOn(console, "error").mockImplementation(() => {});
    render(
      <RouteErrorBoundary>
        <Bomb />
      </RouteErrorBoundary>,
    );

    expect(screen.getByRole("alert")).toHaveTextContent("This page hit an error");
    expect(screen.getByRole("button", { name: "Reload" })).toBeInTheDocument();
    consoleError.mockRestore();
  });

  it("reloads the page when the recovery button is clicked", async () => {
    const consoleError = vi.spyOn(console, "error").mockImplementation(() => {});
    const reload = vi.fn();
    const originalLocation = window.location;
    Object.defineProperty(window, "location", {
      configurable: true,
      value: { ...originalLocation, reload },
    });

    render(
      <RouteErrorBoundary>
        <Bomb />
      </RouteErrorBoundary>,
    );
    await userEvent.click(screen.getByRole("button", { name: "Reload" }));
    expect(reload).toHaveBeenCalledTimes(1);

    Object.defineProperty(window, "location", { configurable: true, value: originalLocation });
    consoleError.mockRestore();
  });
});
