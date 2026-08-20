import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { ScopePivot } from "./ScopePivot";

describe("ScopePivot", () => {
  it("links into Runs and Insight pre-scoped to the given gaggle/workflow", () => {
    render(<ScopePivot label="core / implementation" scope={{ gaggle: "core", workflow: "implementation" }} />);

    const runsLink = screen.getByRole("link", { name: "View core / implementation in Runs" });
    const insightLink = screen.getByRole("link", { name: "View core / implementation in Insight" });

    expect(runsLink).toHaveAttribute("href", "#/runs?gaggle=core&workflow=implementation");
    expect(insightLink).toHaveAttribute("href", "#/insight?gaggle=core&workflow=implementation");
  });

  it("carries a gaggle-only scope", () => {
    render(<ScopePivot label="core" scope={{ gaggle: "core" }} />);

    expect(screen.getByRole("link", { name: "View core in Runs" })).toHaveAttribute(
      "href",
      "#/runs?gaggle=core",
    );
    expect(screen.getByRole("link", { name: "View core in Insight" })).toHaveAttribute(
      "href",
      "#/insight?gaggle=core",
    );
  });

  it("preserves an active time window instead of resetting to all time (#2529)", () => {
    render(
      <ScopePivot
        label="core"
        scope={{ gaggle: "core", since: "2026-07-01T00:00:00Z", until: "2026-07-08T00:00:00Z" }}
      />,
    );

    const runsLink = screen.getByRole("link", { name: "View core in Runs" });
    expect(runsLink).toHaveAttribute(
      "href",
      expect.stringContaining("since=2026-07-01T00%3A00%3A00Z"),
    );
    expect(runsLink).toHaveAttribute(
      "href",
      expect.stringContaining("until=2026-07-08T00%3A00%3A00Z"),
    );
  });
});
