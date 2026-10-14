import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { RunSummary } from "../api/types";
import { populatedDaemonFixtures } from "../test/daemonFixtures";
import { RunWorkTitle } from "./RunWorkTitle";

function fixture(url?: string, displayTitle?: string): RunSummary {
  const run = populatedDaemonFixtures().runs.runs.find((candidate) => candidate.operator?.issue);
  if (!run?.operator) throw new Error("Missing fixture operator");
  return { ...run, operator: { ...run.operator, displayTitle, issue: { number: "123", title: "Example item", url } } };
}

describe("RunWorkTitle", () => {
  it.each([
    undefined,
    "Investigate #123: Example item",
    "Investigate #1234 then #123: Example item",
  ])("links the exact claimed ID without changing the title: %s", (displayTitle) => {
    const run = fixture("https://example.test/incidents/123", displayTitle);
    const { container } = render(<RunWorkTitle run={run} separator=" · " />);
    expect(container.textContent).toBe(displayTitle ?? "#123 · Example item");
    const link = screen.getByRole("link", { name: "#123" });
    expect(link).toHaveAttribute("href", "https://example.test/incidents/123");
    expect(link).toHaveAttribute("target", "_blank");
    expect(link).toHaveAttribute("rel", "noopener noreferrer");
  });

  it.each([
    undefined, "", "/items/123", "//example.test/123", "javascript:alert(1)",
    "https://user:secret@example.test/123", "https:///123", "https://example.test/123\n",
    `https://example.test/${"a".repeat(2048)}`,
  ])("keeps an absent or invalid URL as plain text: %s", (url) => {
    const { container } = render(<RunWorkTitle run={fixture(url)} />);
    expect(container.textContent).toBe("#123 Example item");
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });

  it.each(["Investigate #1234", "Investigate #123-other", "Investigate another item"])(
    "does not guess a link for an unmatched display title: %s", (displayTitle) => {
      const { container } = render(<RunWorkTitle run={fixture("https://example.test/123", displayTitle)} />);
      expect(container.textContent).toBe(displayTitle);
      expect(screen.queryByRole("link")).not.toBeInTheDocument();
    },
  );

  it("renders a number-only link for claimed-work metadata", () => {
    const { container } = render(<RunWorkTitle run={fixture("https://example.test/123", "Custom title")} numberOnly />);
    expect(container.textContent).toBe("#123");
    expect(screen.getByRole("link")).toHaveAttribute("href", "https://example.test/123");
  });

  it("retains the run ID when no work identity is available", () => {
    const run = { ...fixture(), operator: undefined };
    const { container } = render(<RunWorkTitle run={run} />);
    expect(container.textContent).toBe(run.id);
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });
});
