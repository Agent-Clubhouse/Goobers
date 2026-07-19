import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { QueryStateBoundary, type QueryState } from "./queryState";

describe("QueryStateBoundary", () => {
  it.each<{
    state: QueryState<string>;
    expected: string;
  }>([
    { state: { status: "loading" }, expected: "Loading" },
    { state: { status: "empty" }, expected: "No results" },
    { state: { status: "error", error: new Error("offline") }, expected: "Error: offline" },
    { state: { status: "stale", data: "cached" }, expected: "Stale: cached" },
    { state: { status: "ready", data: "live" }, expected: "Ready: live" },
  ])("renders the $state.status state", ({ state, expected }) => {
    render(
      <QueryStateBoundary
        state={state}
        loading={<p>Loading</p>}
        empty={<p>No results</p>}
        error={(error) => <p>Error: {error.message}</p>}
        stale={(data) => <p>Stale: {data}</p>}
      >
        {(data) => <p>Ready: {data}</p>}
      </QueryStateBoundary>,
    );

    expect(screen.getByText(expected)).toBeInTheDocument();
  });

  it("exposes a refresh error alongside stale data", () => {
    render(
      <QueryStateBoundary
        state={{ status: "stale", data: "cached", error: new Error("refresh failed") }}
        loading={null}
        empty={null}
        error={() => null}
        stale={(data, error) => (
          <p>
            {data}: {error?.message}
          </p>
        )}
      >
        {() => null}
      </QueryStateBoundary>,
    );

    expect(screen.getByText("cached: refresh failed")).toBeInTheDocument();
  });
});
