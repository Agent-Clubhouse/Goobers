import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { CopyCommand } from "./CopyCommand";

describe("CopyCommand", () => {
  const writeText = vi.fn();

  beforeEach(() => {
    writeText.mockReset();
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText },
    });
  });

  it("copies the exact command and announces success", async () => {
    writeText.mockResolvedValue(undefined);
    render(
      <CopyCommand
        command="goobers run core implementation"
        failureLabel="Command was not copied."
        idleLabel="Copy manual run command"
        successLabel="Manual run command copied."
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Copy manual run command" }));
    await waitFor(() =>
      expect(writeText).toHaveBeenCalledWith("goobers run core implementation"),
    );
    expect(screen.getByRole("status")).toHaveTextContent("Manual run command copied.");
    expect(screen.getByRole("button", { name: "Manual run command copied." })).toBeInTheDocument();
  });

  it("announces failure and keeps a retryable accessible name", async () => {
    writeText.mockRejectedValue(new Error("clipboard denied"));
    render(
      <CopyCommand
        command="goobers run core implementation"
        failureLabel="Command was not copied."
        idleLabel="Copy manual run command"
        successLabel="Manual run command copied."
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Copy manual run command" }));
    await waitFor(() =>
      expect(screen.getByRole("status")).toHaveTextContent("Command was not copied."),
    );
    expect(
      screen.getByRole("button", { name: "Retry copy manual run command" }),
    ).toBeInTheDocument();
  });
});
