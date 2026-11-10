import { useEffect, useState, type FormEvent } from "react";
import { DaemonApiError } from "../api/errors";
import type { FluidUICustomization, FluidUISavedView, RunPhase } from "../api/types";
import { formatRelativeTimestamp } from "../dateTime";
import {
  fluidUIDensities,
  fluidUIPageLabels,
  fluidUIPanels,
  fluidUIRunPhases,
  fluidUIRunSorts,
  fluidUIVersion,
  savedViewNameProblem,
  savedViewRouteProblem,
  useFluidUI,
  type FluidUIRunSort,
} from "../fluidUI";
import { Action } from "../ui/Action";
import { FluidUISavedViews } from "./FluidUIStatus";

type Density = (typeof fluidUIDensities)[number];

interface Draft {
  density: Density | "";
  sort: FluidUIRunSort | "";
  phases: RunPhase[];
  overviewOrder: string[];
  overviewHidden: string[];
  savedViews: FluidUISavedView[];
  /** Page layouts this Portal does not edit, kept as saved. */
  otherPages: NonNullable<FluidUICustomization["pages"]>;
}

const overviewDefaultOrder: string[] = fluidUIPanels.overview.map(({ id }) => id);
const sortLabels: Record<FluidUIRunSort, string> = {
  newest: "Newest first",
  oldest: "Oldest first",
  "failing-first": "Failing first",
};

function draftFrom(customization: FluidUICustomization | null): Draft {
  const overview = customization?.pages?.overview;
  const known = new Set(overviewDefaultOrder);
  const ordered = (overview?.order ?? []).filter((id) => known.has(id));
  const otherPages = { ...(customization?.pages ?? {}) };
  delete otherPages.overview;
  return {
    density: customization?.density ?? "",
    sort: customization?.runList?.sort ?? "",
    phases: (customization?.runList?.phases ?? []).filter((phase): phase is RunPhase =>
      (fluidUIRunPhases as readonly string[]).includes(phase),
    ),
    overviewOrder: [...ordered, ...overviewDefaultOrder.filter((id) => !ordered.includes(id))],
    overviewHidden: (overview?.hidden ?? []).filter((id) => known.has(id)),
    savedViews: customization?.savedViews ?? [],
    otherPages,
  };
}

function customizationFrom(draft: Draft): FluidUICustomization {
  const customization: FluidUICustomization = { version: fluidUIVersion };
  if (draft.density) customization.density = draft.density;
  const pages = { ...draft.otherPages };
  const reordered = draft.overviewOrder.some((id, index) => id !== overviewDefaultOrder[index]);
  if (reordered || draft.overviewHidden.length > 0) {
    pages.overview = {
      ...(draft.overviewHidden.length > 0 ? { hidden: draft.overviewHidden } : {}),
      ...(reordered ? { order: draft.overviewOrder } : {}),
    };
  }
  if (Object.keys(pages).length > 0) customization.pages = pages;
  if (draft.sort || draft.phases.length > 0) {
    customization.runList = {
      ...(draft.sort ? { sort: draft.sort } : {}),
      ...(draft.phases.length > 0 ? { phases: draft.phases } : {}),
    };
  }
  if (draft.savedViews.length > 0) customization.savedViews = draft.savedViews;
  return customization;
}

function toggle<T>(values: readonly T[], value: T, on: boolean): T[] {
  return on ? (values.includes(value) ? [...values] : [...values, value]) : values.filter((item) => item !== value);
}

/**
 * Labs > Fluid UI (#8416): view the caller's customization, edit it with
 * structured controls, preview it locally (nothing saved), apply, undo, or reset.
 */
export function LabsFluidUI() {
  const fluid = useFluidUI();
  const [draft, setDraft] = useState<Draft>(() => draftFrom(fluid.saved));
  const [viewName, setViewName] = useState("");
  const [viewRoute, setViewRoute] = useState("");
  const [viewProblem, setViewProblem] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [message, setMessage] = useState<string>();
  const savedRevision = fluid.state?.revision;

  // Start from the saved document whenever it changes (load, apply, undo, reset).
  useEffect(() => {
    setDraft(draftFrom(fluid.saved));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [savedRevision, fluid.status]);

  if (fluid.status === "loading") {
    return <p className="inline-empty">Loading your Fluid UI customization…</p>;
  }
  if (fluid.status === "unavailable") {
    return (
      <p className="inline-empty">
        Fluid UI customizations are not available on this connection. The standard Portal is shown.
      </p>
    );
  }

  const update = (change: Partial<Draft>) => setDraft((current) => ({ ...current, ...change }));
  const run = async (label: string, write: () => Promise<void>) => {
    setBusy(true);
    setError(undefined);
    setMessage(undefined);
    try {
      await write();
      setMessage(label);
    } catch (cause) {
      setError(
        cause instanceof DaemonApiError && cause.code === "nothing_to_undo"
          ? "There is nothing to undo."
          : cause instanceof Error
            ? cause.message
            : String(cause),
      );
    } finally {
      setBusy(false);
    }
  };
  const move = (index: number, offset: number) => {
    const order = [...draft.overviewOrder];
    const [id] = order.splice(index, 1);
    order.splice(index + offset, 0, id);
    update({ overviewOrder: order });
  };
  const addView = (event: FormEvent) => {
    event.preventDefault();
    const name = viewName;
    const route = viewRoute.trim();
    const problem =
      savedViewNameProblem(name, new Set(draft.savedViews.map((view) => view.name))) ?? savedViewRouteProblem(route);
    if (problem) {
      setViewProblem(problem);
      return;
    }
    setViewProblem(undefined);
    update({ savedViews: [...draft.savedViews, { name, route }] });
    setViewName("");
    setViewRoute("");
  };

  const state = fluid.state;
  const undoDepth = state?.undoDepth ?? 0;
  const panelLabels = new Map<string, string>(fluidUIPanels.overview.map(({ id, label }) => [id, label]));

  return (
    <div className="fluid-ui-editor">
      <section className="content-section" aria-labelledby="fluid-ui-current">
        <h2 id="fluid-ui-current">Current customization</h2>
        {fluid.status === "error" && (
          <p className="inline-empty" role="alert">
            Could not load your customization: {fluid.error?.message}. The standard Portal is shown.
          </p>
        )}
        {state && (
          <p className="fluid-ui-meta">
            Revision {state.revision}
            {state.updatedAt && <> · updated {formatRelativeTimestamp(Date.parse(state.updatedAt))}</>} · {undoDepth} undo
            {undoDepth === 1 ? " step" : " steps"}
          </p>
        )}
        {fluid.notice && <p className="fluid-ui-meta">{fluid.notice.message}</p>}
        {state?.customization ? (
          <pre className="fluid-ui-document" aria-label="Saved customization">
            {JSON.stringify(state.customization, null, 2)}
          </pre>
        ) : (
          <p className="inline-empty">No customization saved; you see the standard Portal.</p>
        )}
        <FluidUISavedViews label="Your saved views" />
      </section>

      <section className="content-section fluid-ui-form" aria-labelledby="fluid-ui-edit">
        <h2 id="fluid-ui-edit">Edit</h2>
        <div className="fluid-ui-row">
          <label>
            Density
            <select
              aria-label="Density"
              value={draft.density}
              onChange={(event) => update({ density: event.target.value as Density | "" })}
            >
              <option value="">Standard</option>
              {fluidUIDensities.map((density) => (
                <option key={density} value={density}>
                  {density === "compact" ? "Compact" : "Comfortable"}
                </option>
              ))}
            </select>
          </label>
          <label>
            Run list sort
            <select
              aria-label="Run list sort"
              value={draft.sort}
              onChange={(event) => update({ sort: event.target.value as FluidUIRunSort | "" })}
            >
              <option value="">Standard</option>
              {fluidUIRunSorts.map((sort) => (
                <option key={sort} value={sort}>
                  {sortLabels[sort]}
                </option>
              ))}
            </select>
          </label>
        </div>

        <fieldset className="fluid-ui-fieldset">
          <legend>Run phases to show (none checked shows all)</legend>
          {fluidUIRunPhases.map((phase) => (
            <label key={phase} className="fluid-ui-check">
              <input
                type="checkbox"
                checked={draft.phases.includes(phase)}
                onChange={(event) => update({ phases: toggle(draft.phases, phase, event.target.checked) })}
              />
              {phase}
            </label>
          ))}
        </fieldset>

        <fieldset className="fluid-ui-fieldset">
          <legend>{fluidUIPageLabels.overview} panels</legend>
          <ol className="fluid-ui-panels">
            {draft.overviewOrder.map((id, index) => {
              const label = panelLabels.get(id) ?? id;
              return (
                <li key={id}>
                  <label className="fluid-ui-check">
                    <input
                      type="checkbox"
                      aria-label={`Show ${label}`}
                      checked={!draft.overviewHidden.includes(id)}
                      onChange={(event) =>
                        update({ overviewHidden: toggle(draft.overviewHidden, id, !event.target.checked) })
                      }
                    />
                    {label}
                  </label>
                  <Action
                    variant="text"
                    className="text-button"
                    aria-label={`Move ${label} up`}
                    disabled={index === 0}
                    onClick={() => move(index, -1)}
                    type="button"
                  >
                    Up
                  </Action>
                  <Action
                    variant="text"
                    className="text-button"
                    aria-label={`Move ${label} down`}
                    disabled={index === draft.overviewOrder.length - 1}
                    onClick={() => move(index, 1)}
                    type="button"
                  >
                    Down
                  </Action>
                </li>
              );
            })}
          </ol>
        </fieldset>

        <fieldset className="fluid-ui-fieldset">
          <legend>Saved views</legend>
          {draft.savedViews.length > 0 && (
            <ul className="fluid-ui-views">
              {draft.savedViews.map((view) => (
                <li key={view.name}>
                  <span>
                    {view.name} <code>{view.route}</code>
                  </span>
                  <Action
                    variant="text"
                    className="text-button"
                    aria-label={`Remove saved view ${view.name}`}
                    onClick={() => update({ savedViews: draft.savedViews.filter((item) => item.name !== view.name) })}
                    type="button"
                  >
                    Remove
                  </Action>
                </li>
              ))}
            </ul>
          )}
          <form className="fluid-ui-row" onSubmit={addView}>
            <label>
              Name
              <input aria-label="Saved view name" value={viewName} onChange={(event) => setViewName(event.target.value)} />
            </label>
            <label>
              Route
              <input
                aria-label="Saved view route"
                placeholder="#/runs?status=failed"
                value={viewRoute}
                onChange={(event) => setViewRoute(event.target.value)}
              />
            </label>
            <Action variant="secondary" type="submit">Add view</Action>
          </form>
          {viewProblem && (
            <p className="fluid-ui-error" role="alert">
              {viewProblem}
            </p>
          )}
        </fieldset>

        <div className="fluid-ui-actions">
          <Action variant="secondary" onClick={() => fluid.startPreview(customizationFrom(draft))} type="button">
            Preview
          </Action>
          {fluid.preview && (
            <Action variant="secondary" onClick={fluid.stopPreview} type="button">
              Stop preview
            </Action>
          )}
          <Action
            variant="primary"
            disabled={busy}
            onClick={() => void run("Customization applied.", () => fluid.apply(customizationFrom(draft)))}
            type="button"
          >
            Apply
          </Action>
          <Action variant="secondary" disabled={busy || undoDepth === 0} onClick={() => void run("Undone.", fluid.undo)} type="button">
            Undo
          </Action>
          <Action
            variant="secondary"
            disabled={busy || !state?.customization}
            onClick={() => void run("Reset to the standard Portal.", fluid.reset)}
            type="button"
          >
            Reset
          </Action>
          <Action
            variant="text"
            className="text-button"
            onClick={() => {
              setDraft(draftFrom(fluid.saved));
              fluid.stopPreview();
            }}
            type="button"
          >
            Discard changes
          </Action>
        </div>
        {error && (
          <p className="fluid-ui-error" role="alert">
            {error}
          </p>
        )}
        {message && (
          <p className="fluid-ui-message" role="status">
            {message}
          </p>
        )}
      </section>
    </div>
  );
}
