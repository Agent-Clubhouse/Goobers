import { describe, expect, it } from "vitest";
import {
  hasScopeFilters,
  hasScopeIdentity,
  scopeIdentity,
  scopeWindowLabel,
} from "./scope";

describe("scopeWindowLabel", () => {
  it("renders nothing when no time range is set", () => {
    expect(scopeWindowLabel({})).toBe("");
  });

  it("renders a bounded range, and each open-ended half", () => {
    const since = "2026-07-01T00:00:00Z";
    const until = "2026-07-08T00:00:00Z";
    expect(scopeWindowLabel({ since, until })).toContain("from");
    expect(scopeWindowLabel({ since, until })).toContain("to");
    expect(scopeWindowLabel({ since })).toMatch(/^ since /);
    expect(scopeWindowLabel({ until })).toMatch(/^ through /);
  });
});

describe("hasScopeFilters", () => {
  it("is false for an all-undefined filter set", () => {
    expect(hasScopeFilters({})).toBe(false);
    expect(
      hasScopeFilters({ gaggle: undefined, outcome: undefined, since: undefined }),
    ).toBe(false);
  });

  it("is true when any field is set", () => {
    expect(hasScopeFilters({ gaggle: "core" })).toBe(true);
    expect(hasScopeFilters({ window: "24h" })).toBe(true);
    expect(hasScopeFilters({ since: "2026-07-01T00:00:00Z" })).toBe(true);
  });
});

describe("scopeIdentity / hasScopeIdentity", () => {
  it("extracts only gaggle/workflow/stage, dropping refinements", () => {
    expect(
      scopeIdentity({
        gaggle: "core",
        workflow: "implementation",
        outcome: "terminal",
        population: "measured",
        since: "2026-07-01T00:00:00Z",
      }),
    ).toEqual({ gaggle: "core", workflow: "implementation", stage: undefined });
  });

  it("treats an undefined filter set as no identity", () => {
    expect(scopeIdentity(undefined)).toEqual({
      gaggle: undefined,
      workflow: undefined,
      stage: undefined,
    });
  });

  it("is false for identity-less refinements and true once gaggle/workflow/stage appear", () => {
    expect(hasScopeIdentity({ outcome: "terminal", since: "2026-07-01T00:00:00Z" })).toBe(false);
    expect(hasScopeIdentity({ gaggle: "core" })).toBe(true);
    expect(hasScopeIdentity({ stage: "implement" })).toBe(true);
  });
});
