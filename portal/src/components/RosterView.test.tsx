import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { sampleInstanceSnapshot } from "../api/mockClient";
import { RosterView } from "./RosterView";

describe("RosterView", () => {
  it("renders gaggles with goobers and workflows", () => {
    render(<RosterView snapshot={sampleInstanceSnapshot} />);

    expect(screen.getByRole("heading", { name: "Engineering gaggle" })).toBeInTheDocument();
    expect(screen.getByText("Coder Goober")).toBeInTheDocument();
    expect(screen.getByText("QA Goober")).toBeInTheDocument();
    expect(screen.getByText("Default implementation workflow")).toBeInTheDocument();
    expect(screen.getByText(/Plan work -> Implement change -> QA approval/)).toBeInTheDocument();
  });
});
