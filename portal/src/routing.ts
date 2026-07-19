export type Route =
  | { page: "overview" }
  | { page: "workflows" }
  | { page: "runs" }
  | { page: "workflow"; id: string; gaggle?: string }
  | { page: "run"; id: string };

export type PrimaryArea = "overview" | "workflows" | "runs";

export function parseRoute(hash = window.location.hash): Route {
  const path = hash.replace(/^#\/?/, "");
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
    return { page: "runs" };
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
  return `#/${route.page}`;
}

export function activeArea(route: Route): PrimaryArea {
  if (route.page === "workflow") {
    return "workflows";
  }
  if (route.page === "run") {
    return "runs";
  }
  return route.page;
}

export type Navigate = (route: Route) => void;
