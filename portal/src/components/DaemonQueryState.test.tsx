import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { DaemonAuthError, DaemonUnavailableError } from "../api/errors";
import { DaemonErrorState } from "./DaemonQueryState";

// #2916: a 401/403 must render as a distinct auth failure — with the status
// preserved and clearly identified as an auth failure — rather than being
// folded into the generic "daemon unavailable" state.
describe("DaemonErrorState", () => {
  it.each([401, 403] as const)(
    "identifies a %d as an auth failure and preserves the status",
    (status) => {
      render(<DaemonErrorState error={new DaemonAuthError(status)} retry={vi.fn()} />);

      expect(screen.getByRole("alert")).toBeInTheDocument();
      expect(
        screen.getByRole("heading", {
          name: status === 401 ? "Authentication required" : "Access denied",
        }),
      ).toBeInTheDocument();
      expect(screen.getByText(new RegExp(`HTTP ${status}`))).toBeInTheDocument();
      expect(screen.queryByText(/Daemon unavailable/i)).not.toBeInTheDocument();
    },
  );

  it("still renders the generic unavailable state for non-auth failures", () => {
    render(<DaemonErrorState error={new DaemonUnavailableError()} retry={vi.fn()} />);

    expect(screen.getByRole("heading", { name: "Daemon unavailable" })).toBeInTheDocument();
    expect(screen.queryByText(/Authentication required/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/Access denied/i)).not.toBeInTheDocument();
  });
});
