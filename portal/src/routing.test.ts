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
        since: "2026-07-01T00:00:00Z",
        until: "2026-07-08T00:00:00Z",
      },
    };

    const hash = routeHash(route);

    expect(parseRoute(hash)).toEqual(route);
    expect(activeArea(parseRoute("#/insight"))).toBe("insight");
  });
});
