import { describe, expect, it } from "vitest";
import { manualRunCommand, quotePowerShell } from "./manualRunCommand";

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

  it("escapes embedded PowerShell quotes", () => {
    expect(quotePowerShell("C:\\work\\it's")).toBe("'C:\\work\\it''s'");
  });
});
