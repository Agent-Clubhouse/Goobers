import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { DataList, DataRow } from "./DataList";

function rows(count: number) {
  return Array.from({ length: count }, (_, index) => (
    <DataRow href={`#/run/${index}`} key={index} label={`Open run ${index}`}>
      <span>{index}</span>
    </DataRow>
  ));
}

describe("DataList", () => {
  it("caps rendered rows and names the overflow for an unbounded input (DASH-15)", () => {
    render(
      <DataList ariaLabel="Run history" maxRows={200}>
        {rows(5000)}
      </DataList>,
    );

    const region = screen.getByRole("region", { name: "Run history" });
    // A 5000-row input mounts a bounded number of interactive rows, not 5000.
    expect(within(region).getAllByRole("link")).toHaveLength(200);
    expect(within(region).getByText(/Showing 200 of 5000/)).toBeInTheDocument();
  });

  it("renders every row and no overflow affordance when under the cap", () => {
    render(<DataList ariaLabel="Run history">{rows(3)}</DataList>);

    expect(screen.getAllByRole("link")).toHaveLength(3);
    expect(screen.queryByText(/Showing/)).not.toBeInTheDocument();
  });

  it("links the overflow affordance to the full list when a target is given", () => {
    render(
      <DataList
        ariaLabel="Run history"
        maxRows={5}
        overflow={{ href: "#/runs", label: "View all in Runs" }}
      >
        {rows(40)}
      </DataList>,
    );

    const link = screen.getByRole("link", { name: "View all in Runs" });
    expect(link).toHaveAttribute("href", "#/runs");
    expect(screen.getByText(/Showing 5 of 40/)).toBeInTheDocument();
  });
});
