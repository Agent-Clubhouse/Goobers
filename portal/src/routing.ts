import type { InsightWindow } from "./insightData";
import { hasScopeFilters, type ScopeFilters } from "./scope";

export type Route =
  | { page: "overview" }
  | { page: "workflows" }
  | { page: "goobers"; gaggle?: string }
  | { page: "gaggle"; id: string }
  | { page: "runs"; filters?: RunRouteFilters }
  | { page: "errors"; filters: ErrorRouteFilters }
  | { page: "insight"; filters?: InsightRouteFilters }
  | { page: "cost"; filters?: ScopeFilters }
  | { page: "work-items"; kind?: "pr" | "issue"; provider?: string; repository?: string; id?: string }
  | { page: "workflow"; id: string; gaggle?: string }
  | { page: "run"; id: string };

// The Runs and Insight route filters are exactly the shared scope model
// (#2528) — kept as named aliases so call sites read in terms of the view
// they're for, without three parallel field-by-field type declarations.
export type RunStatusFilter = "active" | "attention" | "complete" | "all";

export interface RunRouteFilters extends ScopeFilters {
  status?: RunStatusFilter;
}

export type InsightSection = "contributors" | "usage" | "failures" | "latency";

export interface InsightRouteFilters extends ScopeFilters {
  section?: InsightSection;
}

export interface ErrorRouteFilters extends ScopeFilters {
  code?: string;
  errorClass?: string;
}

export type PrimaryArea =
  | "overview"
  | "workflows"
  | "goobers"
  | "runs"
  | "work-items"
  | "insight"
  | "cost";

export function parseRoute(hash = window.location.hash): Route {
  const fragment = hash.replace(/^#\/?/, "");
  const queryStart = fragment.indexOf("?");
  const path = queryStart >= 0 ? fragment.slice(0, queryStart) : fragment;
  const search = new URLSearchParams(queryStart >= 0 ? fragment.slice(queryStart + 1) : "");
  const [area, first, second] = path.split("/");
  const id = first ? decodeURIComponent(first) : "";
  if (area === "workflow" && id) {
    return second
      ? { page: "workflow", gaggle: id, id: decodeURIComponent(second) }
      : { page: "workflow", id };
  }
  if (area === "gaggle" && id) {
    return { page: "gaggle", id };
  }
  if (area === "run" && id) {
    return { page: "run", id };
  }
  if (area === "work-items") {
    const segments = path.split("/");
    const detailKind = segments[4] === "pr" || segments[4] === "issue" ? segments[4] : undefined;
    if (first && second && segments[3] && detailKind && segments[5]) {
      return {
        page: "work-items",
        provider: decodeURIComponent(first),
        repository: `${decodeURIComponent(second)}/${decodeURIComponent(segments[3])}`,
        kind: detailKind,
        id: decodeURIComponent(segments[5]),
      };
    }
    const filterKind = optionalQuery(search, "kind");
    return {
      page: "work-items",
      kind: filterKind === "pr" || filterKind === "issue" ? filterKind : undefined,
    };
  }
  if (area === "workflows") {
    return { page: "workflows" };
  }
  if (area === "goobers") {
    const gaggle = optionalQuery(search, "gaggle");
    return gaggle ? { page: "goobers", gaggle } : { page: "goobers" };
  }
  if (area === "runs") {
    const filters: RunRouteFilters = {
      ...parseScopeFilters(search),
      status: runStatusQuery(search),
    };
    return hasScopeFilters(filters) || filters.status ? { page: "runs", filters } : { page: "runs" };
  }
  if (area === "errors") {
    return {
      page: "errors",
      filters: {
        ...parseScopeFilters(search),
        code: exactOptionalQuery(search, "code"),
        errorClass: exactOptionalQuery(search, "errorClass"),
      },
    };
  }
  if (area === "insight") {
    const filters = {
      ...parseScopeFilters(search),
      section: insightSectionQuery(search),
    };
    return hasScopeFilters(filters) || filters.section
      ? { page: "insight", filters }
      : { page: "insight" };
  }
  if (area === "cost") {
    const filters = parseScopeFilters(search);
    return hasScopeFilters(filters) ? { page: "cost", filters } : { page: "cost" };
  }
  return { page: "overview" };
}

export function routeHash(route: Route): string {
  if (route.page === "gaggle") {
    return `#/gaggle/${encodeURIComponent(route.id)}`;
  }
  if (route.page === "workflow") {
    const identity = route.gaggle
      ? `${encodeURIComponent(route.gaggle)}/${encodeURIComponent(route.id)}`
      : encodeURIComponent(route.id);
    return `#/workflow/${identity}`;
  }
  if (route.page === "run") {
    return `#/run/${encodeURIComponent(route.id)}`;
  }
  if (route.page === "work-items") {
    if (route.provider && route.repository && route.kind && route.id) {
      const [owner, name] = route.repository.split("/", 2);
      if (owner && name) {
        return `#/work-items/${encodeURIComponent(route.provider)}/${encodeURIComponent(owner)}/${encodeURIComponent(name)}/${route.kind}/${encodeURIComponent(route.id)}`;
      }
    }
    const search = new URLSearchParams();
    writeQuery(search, "kind", route.kind);
    return `#/work-items${search.size > 0 ? `?${search.toString()}` : ""}`;
  }
  if (route.page === "goobers" && route.gaggle) {
    const search = new URLSearchParams({ gaggle: route.gaggle });
    return `#/goobers?${search.toString()}`;
  }
  if (route.page === "runs" && route.filters) {
    const search = new URLSearchParams();
    encodeScopeFilters(search, route.filters);
    writeQuery(search, "status", route.filters.status);
    const suffix = search.size > 0 ? `?${search.toString()}` : "";
    return `#/runs${suffix}`;
  }
  if (route.page === "errors") {
    const search = new URLSearchParams();
    writeScopeIdentity(search, route.filters);
    writeExactQuery(search, "code", route.filters.code);
    writeExactQuery(search, "errorClass", route.filters.errorClass);
    writeScopeWindow(search, route.filters);
    const suffix = search.size > 0 ? `?${search.toString()}` : "";
    return `#/errors${suffix}`;
  }
  if ((route.page === "insight" || route.page === "cost") && route.filters) {
    const search = new URLSearchParams();
    encodeScopeFilters(search, route.filters);
    if (route.page === "insight") {
      writeQuery(search, "section", route.filters.section);
    }
    const suffix = search.size > 0 ? `?${search.toString()}` : "";
    return `#/${route.page}${suffix}`;
  }
  return `#/${route.page}`;
}

export function activeArea(route: Route): PrimaryArea {
  if (route.page === "gaggle" || route.page === "workflow") {
    return "workflows";
  }
  if (route.page === "run") {
    return "runs";
  }
  if (route.page === "errors") {
    return "insight";
  }
  return route.page;
}

export type Navigate = (route: Route) => void;

function optionalQuery(search: URLSearchParams, name: string): string | undefined {
  return search.get(name) || undefined;
}

function exactOptionalQuery(search: URLSearchParams, name: string): string | undefined {
  return search.has(name) ? (search.get(name) ?? "") : undefined;
}

function outcomeQuery(search: URLSearchParams): ScopeFilters["outcome"] {
  const value = optionalQuery(search, "outcome");
  return value === "finished" ||
    value === "terminal" ||
    value === "success" ||
    value === "failure" ||
    value === "other"
    ? value
    : undefined;
}

function populationQuery(search: URLSearchParams): ScopeFilters["population"] {
  const value = optionalQuery(search, "population");
  return value === "attempts" ||
    value === "measured" ||
    value === "token-measured" ||
  value === "premium-measured" ||
  value === "cost-measured" ||
    value === "retry-waste"
    ? value
    : undefined;
}

function insightSectionQuery(search: URLSearchParams): InsightSection | undefined {
  const value = optionalQuery(search, "section");
  return value === "contributors" ||
    value === "usage" ||
    value === "failures" ||
    value === "latency"
    ? value
    : undefined;
}

function runStatusQuery(search: URLSearchParams): RunStatusFilter | undefined {
  const value = optionalQuery(search, "status");
  return value === "active" ||
    value === "attention" ||
    value === "complete" ||
    value === "all"
    ? value
    : undefined;
}

function windowQuery(search: URLSearchParams): InsightWindow | undefined {
  const value = optionalQuery(search, "window");
  return value === "24h" || value === "7d" || value === "30d" || value === "all"
    ? value
    : undefined;
}

function writeQuery(search: URLSearchParams, name: string, value: string | undefined): void {
  if (value) {
    search.set(name, value);
  }
}

function writeExactQuery(search: URLSearchParams, name: string, value: string | undefined): void {
  if (value !== undefined) {
    search.set(name, value);
  }
}

function parseScopeFilters(search: URLSearchParams): ScopeFilters {
  return {
    gaggle: optionalQuery(search, "gaggle"),
    workflow: optionalQuery(search, "workflow"),
    stage: optionalQuery(search, "stage"),
    outcome: outcomeQuery(search),
    population: populationQuery(search),
    since: optionalQuery(search, "since"),
    until: optionalQuery(search, "until"),
    window: windowQuery(search),
  };
}

function writeScopeIdentity(search: URLSearchParams, filters: ScopeFilters): void {
  writeQuery(search, "gaggle", filters.gaggle);
  writeQuery(search, "workflow", filters.workflow);
  writeQuery(search, "stage", filters.stage);
}

function writeScopeRefinement(search: URLSearchParams, filters: ScopeFilters): void {
  writeQuery(search, "outcome", filters.outcome);
  writeQuery(search, "population", filters.population);
}

function writeScopeWindow(search: URLSearchParams, filters: ScopeFilters): void {
  writeQuery(search, "since", filters.since);
  writeQuery(search, "until", filters.until);
  writeQuery(search, "window", filters.window);
}

// Runs/Insight order: identity, then the outcome/population refinement, then
// the time range — matches the field order Runs' URLs already used before
// #2528, so existing bookmarks/links keep resolving to the same hash.
function encodeScopeFilters(search: URLSearchParams, filters: ScopeFilters): void {
  writeScopeIdentity(search, filters);
  writeScopeRefinement(search, filters);
  writeScopeWindow(search, filters);
}
