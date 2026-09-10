import { describe, expect, it } from "vitest";
import { activeArea, parseRoute, routeHash } from "./routing";

describe("definition routing", () => {
  it("round-trips gaggle and workflow identities", () => {
    const gaggle = { page: "gaggle" as const, id: "core tools" };
    const workflow = {
      page: "workflow" as const,
      gaggle: "core tools",
      id: "implementation/v2",
    };

    expect(routeHash(gaggle)).toBe("#/gaggle/core%20tools");
    expect(parseRoute(routeHash(gaggle))).toEqual(gaggle);
    expect(parseRoute(routeHash(workflow))).toEqual(workflow);
    expect(activeArea(gaggle)).toBe("workflows");
  });

  describe("Work Items routing", () => {
    it("round-trips list filters and item detail identities", () => {
      const list = {
        page: "work-items" as const,
        kind: "pr" as const,
        gaggle: "core tools",
        query: "acme/app#42",
      };
      const detail = {
        page: "work-items" as const,
        provider: "github",
        repository: "acme/app",
        kind: "issue" as const,
        id: "42",
      };

      expect(parseRoute(routeHash(list))).toEqual(list);
      expect(routeHash(list)).toBe("#/work-items?kind=pr&gaggle=core+tools&q=acme%2Fapp%2342");
      expect(routeHash(detail)).toBe("#/work-items/github/acme/app/issue/42");
      expect(parseRoute(routeHash(detail))).toEqual(detail);
      expect(activeArea(detail)).toBe("work-items");
    });
  });

  it("round-trips a gaggle-filtered Goobers inventory", () => {
    const route = { page: "goobers" as const, gaggle: "core tools" };

    expect(routeHash(route)).toBe("#/goobers?gaggle=core+tools");
    expect(parseRoute(routeHash(route))).toEqual(route);
  });
});

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

  it("round-trips a scoped Insight route with a time window preset (#2528)", () => {
    const route = {
      page: "insight" as const,
      filters: {
        gaggle: "core tools",
        workflow: "implementation/v2",
        stage: "review gate",
        window: "24h" as const,
      },
    };

    const hash = routeHash(route);

    expect(hash).toMatch(/^#\/insight\?/);
    expect(parseRoute(hash)).toEqual(route);
    expect(parseRoute("#/insight")).toEqual({ page: "insight" });
  });

  it("round-trips a shareable focused Insight section", () => {
    const route = {
      page: "insight" as const,
      filters: {
        gaggle: "core",
        window: "7d" as const,
        section: "failures" as const,
      },
    };

    expect(routeHash(route)).toBe("#/insight?gaggle=core&window=7d&section=failures");
    expect(parseRoute(routeHash(route))).toEqual(route);
  });

  it("round-trips a scoped Cost route with a time window preset", () => {
    const route = {
      page: "cost" as const,
      filters: {
        gaggle: "core tools",
        workflow: "implementation/v2",
        stage: "review gate",
        window: "30d" as const,
      },
    };

    expect(parseRoute(routeHash(route))).toEqual(route);
    expect(parseRoute("#/cost")).toEqual({ page: "cost" });
    expect(activeArea(route)).toBe("cost");
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

  it("orders error drill-through query params with the signature ahead of the time range", () => {
    const hash = routeHash({
      page: "errors",
      filters: {
        gaggle: "core",
        code: "harness.crash",
        errorClass: "unknown",
        since: "2026-07-01T00:00:00Z",
        until: "2026-07-08T00:00:00Z",
      },
    });

    expect(hash).toMatch(
      /^#\/errors\?gaggle=core&code=harness\.crash&errorClass=unknown&since=.*&until=.*/,
    );
  });
});
