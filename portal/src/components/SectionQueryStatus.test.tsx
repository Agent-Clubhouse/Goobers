import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { SectionQueryStatus } from "./SectionQueryStatus";

describe("SectionQueryStatus", () => {
  it("exposes a stable loading state", () => {
    render(<SectionQueryStatus loading message="Refreshing results…" />);

    expect(screen.getByRole("status")).toHaveAttribute("aria-busy", "true");
    expect(screen.getByRole("status")).toHaveAttribute("data-state", "loading");
    expect(screen.getByRole("status")).toHaveTextContent("Refreshing results");
  });

  it("keeps errors visible with the standard retry action", async () => {
    const retry = vi.fn();
    const user = userEvent.setup();
    render(<SectionQueryStatus error message="Refresh failed." retry={retry} />);

    const alert = screen.getByRole("alert");
    expect(alert).toHaveAttribute("data-state", "error");
    await user.click(screen.getByRole("button", { name: "Retry" }));
    expect(retry).toHaveBeenCalledOnce();
  });
});
