export type Route =
  | { page: "overview" }
  | { page: "workflows" }
  | { page: "runs" }
  | { page: "workflow"; id: string }
  | { page: "run"; id: string };

export type Navigate = (route: Route) => void;
export type PrimaryArea = "overview" | "workflows" | "runs";

export function parseRoute(hash = window.location.hash): Route {
  const path = hash.replace(/^#\/?/, "");
  const [area, id] = path.split("/");
  if (area === "workflow" && id) {
    return { page: "workflow", id };
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
  if (route.page === "workflow" || route.page === "run") {
    return `#/${route.page}/${route.id}`;
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
