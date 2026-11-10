// Labs Fluid UI (#8338, #8416): loads the caller's per-user presentation
// customization and exposes the effective document to the Portal. A
// customization is data, never code: it can only pick density, hide or order
// documented panels, set run-list defaults, and list saved views. Anything the
// Portal cannot validate falls back to the standard UI with a visible notice.

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { DaemonApiError } from "./api/errors";
import type {
  DaemonClient,
  FluidUICustomization,
  FluidUICustomizationState,
  FluidUIPageLayout,
  RunPhase,
  RunSummary,
} from "./api/types";

export const fluidUIVersion = "portal.customization/v1";

export const fluidUIDensities = ["comfortable", "compact"] as const;
export const fluidUIRunSorts = ["newest", "oldest", "failing-first"] as const;
export type FluidUIRunSort = (typeof fluidUIRunSorts)[number];

/**
 * Panels a customization may hide or reorder, per page. These IDs are a stable
 * contract with stored documents: never rename one; add new IDs instead. IDs a
 * page does not render are ignored.
 */
export const fluidUIPanels = {
  overview: [
    { id: "summary", label: "Instance summary" },
    { id: "attention", label: "Needs attention" },
    { id: "activity", label: "Active runs and recent outcomes" },
    { id: "warnings", label: "Configuration warnings" },
  ],
} as const satisfies Record<string, readonly { id: string; label: string }[]>;

export type FluidUIPage = keyof typeof fluidUIPanels;

export const fluidUIPageLabels: Record<FluidUIPage, string> = { overview: "Overview" };

/** Run phases the run-list phase filter understands. */
export const fluidUIRunPhases: readonly RunPhase[] = [
  "running",
  "completed",
  "failed",
  "aborted",
  "escalated",
];

const identifierPattern = /^[a-z][a-z0-9-]{0,63}$/;
const maxPages = 32;
const maxPanelsPerPage = 64;
const maxRunPhases = 16;
const maxSavedViews = 20;
const maxViewNameLength = 64;
const maxViewRouteLength = 512;

export type FluidUIValidation =
  | { ok: true; customization: FluidUICustomization }
  | { ok: false; problem: string };

/**
 * Mirrors the daemon's schema checks (internal/fluidui) so a document the
 * Portal does not understand is never partially applied. Problems lead with
 * the field path, like the API's 400 messages.
 */
export function validateFluidUICustomization(value: unknown): FluidUIValidation {
  const fail = (problem: string): FluidUIValidation => ({ ok: false, problem });
  if (!isObject(value)) return fail("$: customization must be an object");
  const known = new Set(["version", "density", "pages", "runList", "savedViews"]);
  for (const key of Object.keys(value)) {
    if (!known.has(key)) return fail(`${key}: unknown field`);
  }
  if (value.version !== fluidUIVersion) {
    return fail(`version: unsupported version ${JSON.stringify(value.version)}, expected "${fluidUIVersion}"`);
  }
  if (value.density !== undefined && !includes(fluidUIDensities, value.density)) {
    return fail(`density: must be "comfortable" or "compact"`);
  }
  if (value.pages !== undefined) {
    if (!isObject(value.pages)) return fail("pages: must be an object");
    const pages = Object.entries(value.pages);
    if (pages.length > maxPages) return fail(`pages: at most ${maxPages} pages`);
    for (const [page, layout] of pages) {
      if (!identifierPattern.test(page)) return fail(`pages: page ID ${JSON.stringify(page)} is not a lowercase slug`);
      if (!isObject(layout)) return fail(`pages.${page}: must be an object`);
      for (const key of Object.keys(layout)) {
        if (key !== "hidden" && key !== "order") return fail(`pages.${page}.${key}: unknown field`);
      }
      const problem =
        identifierListProblem(`pages.${page}.hidden`, layout.hidden, maxPanelsPerPage) ??
        identifierListProblem(`pages.${page}.order`, layout.order, maxPanelsPerPage);
      if (problem) return fail(problem);
    }
  }
  if (value.runList !== undefined) {
    if (!isObject(value.runList)) return fail("runList: must be an object");
    for (const key of Object.keys(value.runList)) {
      if (key !== "sort" && key !== "phases") return fail(`runList.${key}: unknown field`);
    }
    if (value.runList.sort !== undefined && !includes(fluidUIRunSorts, value.runList.sort)) {
      return fail(`runList.sort: must be "newest", "oldest" or "failing-first"`);
    }
    const problem = identifierListProblem("runList.phases", value.runList.phases, maxRunPhases);
    if (problem) return fail(problem);
  }
  if (value.savedViews !== undefined) {
    if (!Array.isArray(value.savedViews)) return fail("savedViews: must be a list");
    if (value.savedViews.length > maxSavedViews) return fail(`savedViews: at most ${maxSavedViews} saved views`);
    const names = new Set<string>();
    for (const [index, view] of value.savedViews.entries()) {
      const field = `savedViews[${index}]`;
      if (!isObject(view)) return fail(`${field}: must be an object`);
      for (const key of Object.keys(view)) {
        if (key !== "name" && key !== "route") return fail(`${field}.${key}: unknown field`);
      }
      const nameProblem = savedViewNameProblem(view.name, names);
      if (nameProblem) return fail(`${field}.name: ${nameProblem}`);
      names.add(view.name as string);
      const routeProblem = savedViewRouteProblem(view.route);
      if (routeProblem) return fail(`${field}.route: ${routeProblem}`);
    }
  }
  return { ok: true, customization: value as unknown as FluidUICustomization };
}

/** Explains why a saved-view name is invalid, or returns undefined. */
export function savedViewNameProblem(name: unknown, existing: ReadonlySet<string> = new Set()): string | undefined {
  if (typeof name !== "string" || name.trim() === "" || name.length > maxViewNameLength || name.trim() !== name) {
    return `must be 1-${maxViewNameLength} characters without surrounding spaces`;
  }
  if (existing.has(name)) return `${JSON.stringify(name)} is repeated`;
  return undefined;
}

/** Saved views may only target in-app hash routes, never another origin or a script URL. */
export function savedViewRouteProblem(route: unknown): string | undefined {
  if (typeof route !== "string" || route.length > maxViewRouteLength) {
    return `must be a Portal route of at most ${maxViewRouteLength} characters`;
  }
  if (!route.startsWith("#/")) return 'must be a Portal route starting with "#/"';
  for (const character of route) {
    const code = character.codePointAt(0) ?? 0;
    if (code < 0x21 || code === 0x7f || '<>"\\'.includes(character)) {
      return "contains a disallowed character";
    }
  }
  return undefined;
}

function identifierListProblem(field: string, ids: unknown, limit: number): string | undefined {
  if (ids === undefined) return undefined;
  if (!Array.isArray(ids)) return `${field}: must be a list`;
  if (ids.length > limit) return `${field}: at most ${limit} entries`;
  const seen = new Set<string>();
  for (const [index, id] of ids.entries()) {
    if (typeof id !== "string" || !identifierPattern.test(id)) {
      return `${field}[${index}]: ${JSON.stringify(id)} is not a lowercase slug`;
    }
    if (seen.has(id)) return `${field}[${index}]: ${JSON.stringify(id)} is repeated`;
    seen.add(id);
  }
  return undefined;
}

/** Lists page, panel, and phase IDs this Portal does not render; they are ignored. */
export function unknownFluidUIIdentifiers(customization: FluidUICustomization): string[] {
  const unknown: string[] = [];
  for (const [page, layout] of Object.entries(customization.pages ?? {})) {
    const panels = isFluidUIPage(page) ? new Set<string>(fluidUIPanels[page].map(({ id }) => id)) : undefined;
    if (!panels) {
      unknown.push(`page ${page}`);
      continue;
    }
    for (const id of new Set([...(layout.hidden ?? []), ...(layout.order ?? [])])) {
      if (!panels.has(id)) unknown.push(`${page} panel ${id}`);
    }
  }
  for (const phase of customization.runList?.phases ?? []) {
    if (!includes(fluidUIRunPhases, phase)) unknown.push(`run phase ${phase}`);
  }
  return unknown;
}

export function isFluidUIPage(page: string): page is FluidUIPage {
  return Object.hasOwn(fluidUIPanels, page);
}

export interface FluidUIPanel {
  id: string;
  node: ReactNode;
}

/**
 * Orders a page's panels by the layout's `order` (unlisted panels keep their
 * standard order after the listed ones) and drops hidden panels. Unknown IDs
 * are ignored, so a layout can never remove a panel the page does not know.
 */
export function arrangeFluidUIPanels<Panel extends { id: string }>(
  panels: readonly Panel[],
  layout: FluidUIPageLayout | undefined,
): Panel[] {
  if (!layout) return [...panels];
  const hidden = new Set(layout.hidden ?? []);
  const rank = new Map((layout.order ?? []).map((id, index) => [id, index]));
  return panels
    .map((panel, index) => ({ panel, index }))
    .filter(({ panel }) => !hidden.has(panel.id))
    .sort(
      (a, b) =>
        (rank.get(a.panel.id) ?? Number.MAX_SAFE_INTEGER) - (rank.get(b.panel.id) ?? Number.MAX_SAFE_INTEGER) ||
        a.index - b.index,
    )
    .map(({ panel }) => panel);
}

/** True when a layout changes the standard order of panels the page renders. */
export function fluidUIReordersPanels(page: FluidUIPage, layout: FluidUIPageLayout | undefined): boolean {
  const ids = fluidUIPanels[page].map(({ id }) => id);
  return arrangeFluidUIPanels(ids.map((id) => ({ id })), { order: layout?.order }).some(
    ({ id }, index) => id !== ids[index],
  );
}

/** The run-list phases a customization filters to, ignoring phases the Portal does not know. */
export function fluidUIRunPhaseFilter(customization: FluidUICustomization | null): RunPhase[] {
  return fluidUIRunPhases.filter((phase) => customization?.runList?.phases?.includes(phase));
}

const failingPhases = new Set<RunPhase>(["failed", "aborted", "escalated"]);

/** Applies a run-list sort to already-loaded runs (the daemon returns newest first). */
export function sortRunsForFluidUI(runs: readonly RunSummary[], sort: FluidUIRunSort | undefined): RunSummary[] {
  const startedAt = (run: RunSummary) => Date.parse(run.startedAt) || 0;
  switch (sort) {
    case "oldest":
      return [...runs].sort((a, b) => startedAt(a) - startedAt(b));
    case "failing-first":
      return runs
        .map((run, index) => ({ run, index }))
        .sort(
          (a, b) =>
            Number(failingPhases.has(b.run.phase)) - Number(failingPhases.has(a.run.phase)) || a.index - b.index,
        )
        .map(({ run }) => run);
    default:
      return [...runs];
  }
}

/** Drops empty fields so previews and saved documents stay minimal. */
export function normalizeFluidUICustomization(customization: FluidUICustomization): FluidUICustomization {
  const pages: Record<string, FluidUIPageLayout> = {};
  for (const [page, layout] of Object.entries(customization.pages ?? {})) {
    const next: FluidUIPageLayout = {};
    if (layout.hidden?.length) next.hidden = [...layout.hidden];
    if (layout.order?.length) next.order = [...layout.order];
    if (next.hidden || next.order) pages[page] = next;
  }
  const runList: NonNullable<FluidUICustomization["runList"]> = {};
  if (customization.runList?.sort) runList.sort = customization.runList.sort;
  if (customization.runList?.phases?.length) runList.phases = [...customization.runList.phases];
  return {
    version: customization.version,
    ...(customization.density ? { density: customization.density } : {}),
    ...(Object.keys(pages).length > 0 ? { pages } : {}),
    ...(runList.sort || runList.phases ? { runList } : {}),
    ...(customization.savedViews?.length
      ? { savedViews: customization.savedViews.map(({ name, route }) => ({ name, route })) }
      : {}),
  };
}

export interface FluidUINotice {
  kind: "fallback" | "ignored";
  message: string;
}

export interface FluidUIActions {
  applyFluidUI: (customization: FluidUICustomization) => Promise<FluidUICustomizationState>;
  undoFluidUI: () => Promise<FluidUICustomizationState>;
  resetFluidUI: () => Promise<FluidUICustomizationState>;
}

/** Binds the write actions to a client; hosts without the store reject them. */
export function fluidUIClientActions(client: DaemonClient): FluidUIActions {
  const unsupported = () => Promise.reject(new Error("Fluid UI customizations are not available on this connection."));
  return {
    applyFluidUI: (customization) =>
      client.putLabsFluidUICustomization ? client.putLabsFluidUICustomization(customization) : unsupported(),
    undoFluidUI: () => (client.undoLabsFluidUICustomization ? client.undoLabsFluidUICustomization() : unsupported()),
    resetFluidUI: () => (client.resetLabsFluidUICustomization ? client.resetLabsFluidUICustomization() : unsupported()),
  };
}

export interface FluidUIContextValue {
  /** `unavailable` means this connection has no customization store; the standard UI applies. */
  status: "loading" | "ready" | "unavailable" | "error";
  error?: Error;
  state?: FluidUICustomizationState;
  /** The saved document when it validates, otherwise null (standard UI). */
  saved: FluidUICustomization | null;
  /** A local, unsaved preview; discarded on stop, apply, or reload. */
  preview: FluidUICustomization | null;
  /** What the Portal applies right now: the preview, else the saved document. */
  effective: FluidUICustomization | null;
  notice?: FluidUINotice;
  startPreview: (customization: FluidUICustomization) => void;
  stopPreview: () => void;
  apply: (customization: FluidUICustomization) => Promise<void>;
  undo: () => Promise<void>;
  reset: () => Promise<void>;
  reload: () => void;
}

const unavailable = () => Promise.reject(new Error("Fluid UI customizations are not available here."));

const standardFluidUI: FluidUIContextValue = {
  status: "unavailable",
  saved: null,
  preview: null,
  effective: null,
  startPreview: () => undefined,
  stopPreview: () => undefined,
  apply: unavailable,
  undo: unavailable,
  reset: unavailable,
  reload: () => undefined,
};

const FluidUIContext = createContext<FluidUIContextValue>(standardFluidUI);

export function useFluidUI(): FluidUIContextValue {
  return useContext(FluidUIContext);
}

/** The effective customization; null means the standard Portal. */
export function useFluidUICustomization(): FluidUICustomization | null {
  return useContext(FluidUIContext).effective;
}

export function FluidUIProvider({
  actions,
  children,
  client,
}: {
  actions: FluidUIActions;
  children: ReactNode;
  client: DaemonClient;
}) {
  const supported = typeof client.getLabsFluidUICustomization === "function";
  const [status, setStatus] = useState<FluidUIContextValue["status"]>(supported ? "loading" : "unavailable");
  const [error, setError] = useState<Error>();
  const [state, setState] = useState<FluidUICustomizationState>();
  const [preview, setPreview] = useState<FluidUICustomization | null>(null);
  const [loadCount, setLoadCount] = useState(0);
  // Hosts rebind actions on every render; read the latest without churning the context.
  const actionsRef = useRef(actions);
  actionsRef.current = actions;

  useEffect(() => {
    if (!client.getLabsFluidUICustomization) {
      setStatus("unavailable");
      return;
    }
    const controller = new AbortController();
    void client
      .getLabsFluidUICustomization({ signal: controller.signal })
      .then((next) => {
        setState(next);
        setError(undefined);
        setStatus("ready");
      })
      .catch((cause: unknown) => {
        if (controller.signal.aborted) return;
        // A daemon without the store (503 labs_unavailable, or an older daemon's
        // 404) keeps the standard Portal silently; anything else is reported in
        // Labs but still never breaks a page.
        if (cause instanceof DaemonApiError && (cause.status === 503 || cause.status === 404)) {
          setStatus("unavailable");
          return;
        }
        setError(cause instanceof Error ? cause : new Error(String(cause)));
        setStatus("error");
      });
    return () => controller.abort();
  }, [client, loadCount]);

  const savedValidation = useMemo(
    () =>
      state?.customization && !state.incompatible ? validateFluidUICustomization(state.customization) : undefined,
    [state],
  );
  const saved = savedValidation?.ok ? savedValidation.customization : null;
  const effective = preview ?? saved;

  const notice = useMemo<FluidUINotice | undefined>(() => {
    if (state?.incompatible) {
      return {
        kind: "fallback",
        message:
          "Your saved Fluid UI customization is not compatible with this Portal version, so the standard Portal is shown.",
      };
    }
    if (savedValidation && !savedValidation.ok && !preview) {
      return {
        kind: "fallback",
        message: `Your Fluid UI customization could not be applied (${savedValidation.problem}), so the standard Portal is shown.`,
      };
    }
    const ignored = effective ? unknownFluidUIIdentifiers(effective) : [];
    if (ignored.length > 0) {
      return {
        kind: "ignored",
        message: `Your Fluid UI customization mentions items this Portal does not show, so they are ignored: ${ignored.join(", ")}.`,
      };
    }
    return undefined;
  }, [effective, preview, savedValidation, state?.incompatible]);

  const commit = useCallback(async (write: () => Promise<FluidUICustomizationState>) => {
    const next = await write();
    setState(next);
    setPreview(null);
    setStatus("ready");
  }, []);

  const value = useMemo<FluidUIContextValue>(
    () => ({
      status,
      error,
      state,
      saved,
      preview,
      effective,
      notice,
      startPreview: (customization) => setPreview(normalizeFluidUICustomization(customization)),
      stopPreview: () => setPreview(null),
      apply: (customization) => commit(() => actionsRef.current.applyFluidUI(normalizeFluidUICustomization(customization))),
      undo: () => commit(() => actionsRef.current.undoFluidUI()),
      reset: () => commit(() => actionsRef.current.resetFluidUI()),
      reload: () => setLoadCount((count) => count + 1),
    }),
    [commit, effective, error, notice, preview, saved, state, status],
  );

  return <FluidUIContext.Provider value={value}>{children}</FluidUIContext.Provider>;
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function includes<T extends string>(values: readonly T[], value: unknown): value is T {
  return typeof value === "string" && (values as readonly string[]).includes(value);
}
