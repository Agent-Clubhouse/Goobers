import { describe, expect, it } from "vitest";
import { activeArea, parseRoute, routeHash } from "./routing";

describe("Insight routing", () => {
  it("round-trips scoped run drill-through filters", () => {
    const route = {
      page: "runs" as const,
      filters: {
        gaggle: "core tools",
        workflow: "implementation/v2",
        stage: "review gate",
        outcome: "terminal" as const,
        population: "measured" as const,
        since: "2026-07-01T00:00:00Z",
        until: "2026-07-08T00:00:00Z",
      },
    };

    const hash = routeHash(route);

    expect(parseRoute(hash)).toEqual(route);
    expect(activeArea(parseRoute("#/insight"))).toBe("insight");
  });

  it("retains the finished telemetry population", () => {
    expect(parseRoute("#/runs?outcome=finished")).toEqual({
      page: "runs",
      filters: {
        gaggle: undefined,
        workflow: undefined,
        stage: undefined,
        outcome: "finished",
        population: undefined,
        since: undefined,
        until: undefined,
      },
    });
  });

  it("round-trips contributor-specific usage populations", () => {
    for (const population of [
      "token-measured",
      "premium-measured",
      "cost-measured",
      "retry-waste",
    ] as const) {
      const route = {
        page: "runs" as const,
        filters: { workflow: "implementation", population },
      };
      expect(parseRoute(routeHash(route))).toEqual({
        page: "runs",
        filters: {
          gaggle: undefined,
          workflow: "implementation",
          stage: undefined,
          outcome: undefined,
          population,
          since: undefined,
          until: undefined,
        },
      });
    }
  });

  it("round-trips an exact error signature including empty values", () => {
    const route = {
      page: "errors" as const,
      filters: {
        gaggle: "core tools",
        workflow: "implementation/v2",
        stage: "review gate",
        code: "",
        errorClass: "",
        since: "2026-07-01T00:00:00Z",
        until: "2026-07-08T00:00:00Z",
      },
    };

    expect(parseRoute(routeHash(route))).toEqual(route);
    expect(activeArea(route)).toBe("insight");
  });
});
