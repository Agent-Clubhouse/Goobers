import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { ChildPublicationItem } from "../api/types";
import { ChildPublicationSummary } from "./ChildPublicationSummary";

const confirmed: ChildPublicationItem = { action: "pr", state: "confirmed", head: "factory/children/child", base: "main", commit: "a".repeat(40), pullRequestUrl: "https://github.com/owner/repo/pull/7", pullRequestNumber: 7, needsHuman: false };

describe("child publication status", () => {
  it("links only a confirmed PR", () => {
    render(<ChildPublicationSummary publication={{ status: "recorded", items: [confirmed] }} />);
    expect(screen.getByRole("link", { name: "Open child PR #7" })).toHaveAttribute("href", confirmed.pullRequestUrl);
  });

  it("shows an uncertain effect without a link or a duplicate-create action", () => {
    render(<ChildPublicationSummary publication={{ status: "recorded", items: [{ ...confirmed, state: "effect_pending", needsHuman: true, pullRequestUrl: "", pullRequestNumber: 0 }] }} />);
    expect(screen.getByText(/PR publication needs a human/)).toBeInTheDocument();
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });

  it("keeps prepared publication separate from a confirmed provider effect", () => {
    render(<ChildPublicationSummary publication={{ status: "recorded", items: [{ ...confirmed, state: "prepared", pullRequestUrl: "", pullRequestNumber: 0 }] }} />);
    expect(screen.getByText(/no provider effect has been recorded/)).toBeInTheDocument();
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });

  it.each(["javascript:alert(1)", "https://secret@example.com/pull/7"])("refuses an unsafe returned URL: %s", (pullRequestUrl) => {
    render(<ChildPublicationSummary publication={{ status: "recorded", items: [{ ...confirmed, pullRequestUrl }] }} />);
    expect(screen.getByText("Publication details unavailable.")).toBeInTheDocument();
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });

  it("does not present unavailable records as absence or expired records as live", () => {
    const { rerender } = render(<ChildPublicationSummary publication={{ status: "unavailable", items: [] }} />);
    expect(screen.getByText("Publication details unavailable.")).toBeInTheDocument();
    rerender(<ChildPublicationSummary publication={{ status: "expired", items: [confirmed] }} />);
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });
});
