import type { OutcomeFilter, StagePopulationFilter } from "./api/types";

export type Route =
  | { page: "overview" }
  | { page: "workflows" }
  | { page: "runs"; filters?: RunRouteFilters }
  | { page: "errors"; filters: ErrorRouteFilters }
  | { page: "insight" }
  | { page: "workflow"; id: string; gaggle?: string }
  | { page: "run"; id: string };

export interface RunRouteFilters {
  gaggle?: string;
  workflow?: string;
  stage?: string;
  outcome?: OutcomeFilter;
  population?: StagePopulationFilter;
  since?: string;
  until?: string;
}

export interface ErrorRouteFilters {
  gaggle?: string;
  workflow?: string;
  stage?: string;
  code?: string;
  errorClass?: string;
  since?: string;
  until?: string;
}

export type PrimaryArea = "overview" | "workflows" | "runs" | "insight";

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
  if (area === "run" && id) {
    return { page: "run", id };
  }
  if (area === "workflows") {
    return { page: "workflows" };
  }
  if (area === "runs") {
    const filters: RunRouteFilters = {
      gaggle: optionalQuery(search, "gaggle"),
      workflow: optionalQuery(search, "workflow"),
      stage: optionalQuery(search, "stage"),
      outcome: outcomeQuery(search),
      population: populationQuery(search),
      since: optionalQuery(search, "since"),
      until: optionalQuery(search, "until"),
    };
    return Object.values(filters).some(Boolean) ? { page: "runs", filters } : { page: "runs" };
  }
  if (area === "errors") {
    return {
      page: "errors",
      filters: {
        gaggle: optionalQuery(search, "gaggle"),
        workflow: optionalQuery(search, "workflow"),
        stage: optionalQuery(search, "stage"),
        code: exactOptionalQuery(search, "code"),
        errorClass: exactOptionalQuery(search, "errorClass"),
        since: optionalQuery(search, "since"),
        until: optionalQuery(search, "until"),
      },
    };
  }
  if (area === "insight") {
    return { page: "insight" };
  }
  return { page: "overview" };
}

export function routeHash(route: Route): string {
  if (route.page === "workflow") {
    const identity = route.gaggle
      ? `${encodeURIComponent(route.gaggle)}/${encodeURIComponent(route.id)}`
      : encodeURIComponent(route.id);
    return `#/workflow/${identity}`;
  }
  if (route.page === "run") {
    return `#/run/${encodeURIComponent(route.id)}`;
  }
  if (route.page === "runs" && route.filters) {
    const search = new URLSearchParams();
    for (const [name, value] of Object.entries(route.filters)) {
      if (value) {
        search.set(name, value);
      }
    }
    const suffix = search.size > 0 ? `?${search.toString()}` : "";
    return `#/runs${suffix}`;
  }
  if (route.page === "errors") {
    const search = new URLSearchParams();
    for (const [name, value] of Object.entries(route.filters)) {
      if (value !== undefined) {
        search.set(name, value);
      }
    }
    const suffix = search.size > 0 ? `?${search.toString()}` : "";
    return `#/errors${suffix}`;
  }
  return `#/${route.page}`;
}

export function activeArea(route: Route): PrimaryArea {
  if (route.page === "workflow") {
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

function outcomeQuery(search: URLSearchParams): OutcomeFilter | undefined {
  const value = optionalQuery(search, "outcome");
  return value === "finished" ||
    value === "terminal" ||
    value === "success" ||
    value === "failure" ||
    value === "other"
    ? value
    : undefined;
}

function populationQuery(search: URLSearchParams): StagePopulationFilter | undefined {
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
