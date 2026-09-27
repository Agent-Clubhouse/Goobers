import { describe, expect, it } from "vitest";
import { manualRunCommand, statusCommand } from "./manualRunCommand";

describe("manual run commands", () => {
  it("uses the fully qualified workflow and current instance root", () => {
    expect(manualRunCommand("core", "implementation")).toBe(
      "goobers run core/implementation '.'",
    );
  });

  it("quotes Windows paths for PowerShell", () => {
    expect(manualRunCommand("gaggle", "workflow", "C:\\Goobers\\O'Brien instance")).toBe(
      "goobers run gaggle/workflow 'C:\\Goobers\\O''Brien instance'",
    );
  });

  it("builds a status command with the full instance path", () => {
    expect(statusCommand("C:\\Goobers\\instances\\local-dev")).toBe(
      "goobers status 'C:\\Goobers\\instances\\local-dev'",
    );
  });
});
