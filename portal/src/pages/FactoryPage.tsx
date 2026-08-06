import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type RefObject } from "react";
import type { DaemonClient } from "../api/types";
import { FactoryFloor } from "../components/FactoryFloor";
import { FactoryInspector } from "../components/FactoryInspector";
import { FactoryPlant } from "../components/FactoryPlant";
import { FactoryViewport } from "../components/FactoryViewport";
import { useFactoryFloor, type FactoryFloorData } from "../factoryData";
import { CLASSIC_PLANT_HEIGHT, CLASSIC_PLANT_WIDTH } from "../factoryClassicPlant";
import {
  DEFAULT_FACTORY_LAYOUT,
  FACTORY_LAYOUTS,
  factoryLayoutDescription,
  factoryLayoutLabel,
  type FactoryLayout,
} from "../factoryLayout";
import { FACTORY_LENSES, type FactoryFloorModel, type FactoryLens } from "../factoryModel";
import {
  observePlantForcedColors,
  plantForcedColorsActive,
} from "../plantForcedColors";
import { overviewSelection, type FactorySelection } from "../factorySelection";
import { usePrefersReducedMotion } from "../hooks/usePrefersReducedMotion";
import { useLiveData } from "../liveData";
import {
  summarizePlantRisk,
  type PlantReadState,
  type PlantRiskSummary,
} from "../plantRisk";
import type { FactoryRouteScope, Navigate } from "../routing";

/**
 * Factory Floor: the whole instance as a working plant.
 *
 * Every building, crate and worker on this page is a daemon fact: configured
 * workflows are production lines, their declared stages are the machines, the
 * runs the daemon reports as running are the work carriers standing on those
 * machines, and goobers appear at a stage only when they own it and it is
 * holding work. Nothing is simulated and nothing loops for decoration.
 *
 * Two layouts draw that one model. `Lines` is the precise topology and `Plant`
 * is the isometric hall. The layout lives in the route so a view can be shared,
 * but it is presentation only: it changes no read, no model, no entity ID and
 * no selection. The lens is a separate control because it asks a different
 * question, what to emphasise, and both layouts answer it.
 */
export function FactoryPage({
  client,
  navigate,
  scope,
  standalone,
}: {
  client: DaemonClient;
  navigate: Navigate;
  scope?: FactoryRouteScope;
  standalone: boolean;
}) {
  const lens: FactoryLens = scope?.lens ?? "world";
  const layout: FactoryLayout = scope?.layout ?? DEFAULT_FACTORY_LAYOUT;
  const requested = useMemo(
    () => ({ gaggle: scope?.gaggle, workflow: scope?.workflow }),
    [scope?.gaggle, scope?.workflow],
  );
  const query = useFactoryFloor(client, requested);
  const { dataFreshness, freshness: transport } = useLiveData();
  const reducedMotion = usePrefersReducedMotion();
  const inspectorToggleRef = useRef<HTMLButtonElement>(null);
  const plantShellRef = useRef<HTMLDivElement>(null);
  const pendingPlantFocusRef = useRef<string | undefined>(undefined);
  const capturePlantFocus = useCallback(() => {
    const active = document.activeElement;
    const shell = plantShellRef.current;
    if (!(active instanceof HTMLElement) || !shell?.contains(active)) {
      return;
    }
    const control = active.closest<HTMLElement>("[data-plant-focus-id]");
    if (control?.dataset.plantFocusId) {
      pendingPlantFocusRef.current = control.dataset.plantFocusId;
    }
  }, []);
  const forcedColors = usePlantForcedColors(capturePlantFocus);
  const shortViewport = useShortFactoryViewport();
  const [selection, setSelection] = useState<FactorySelection>(overviewSelection);
  const [inspectorOpen, setInspectorOpen] = useState(() => !shortViewport);
  const transitionModel =
    query.state.status === "ready" || query.state.status === "stale"
      ? query.state.data.model
      : undefined;
  const animateTransitions = useTransitionAnimation(transitionModel, layout);

  useEffect(() => {
    if (shortViewport) {
      setInspectorOpen(false);
    }
  }, [shortViewport]);

  useEffect(() => {
    const trackPlantFocus = (event: FocusEvent) => {
      const target = event.target;
      const shell = plantShellRef.current;
      if (!(target instanceof HTMLElement) || !shell) {
        return;
      }
      if (shell.contains(target)) {
        const control = target.closest<HTMLElement>("[data-plant-focus-id]");
        if (control?.dataset.plantFocusId) {
          pendingPlantFocusRef.current = control.dataset.plantFocusId;
        }
        return;
      }
      if (target !== document.body && target !== document.documentElement) {
        pendingPlantFocusRef.current = undefined;
      }
    };
    document.addEventListener("focusin", trackPlantFocus);
    return () => document.removeEventListener("focusin", trackPlantFocus);
  }, []);

  useLayoutEffect(() => {
    const shell = plantShellRef.current;
    const pendingFocus = pendingPlantFocusRef.current;
    if (!pendingFocus || !shell) {
      return;
    }
    const restore = () => {
      const target = Array.from(
        shell.querySelectorAll<HTMLElement>("[data-plant-focus-id]"),
      ).find((element) => element.dataset.plantFocusId === pendingFocus);
      if (!target) {
        return false;
      }
      target.focus({ preventScroll: true });
      pendingPlantFocusRef.current = undefined;
      return true;
    };
    if (restore()) {
      return;
    }
    const observer = new MutationObserver(() => {
      if (restore()) {
        observer.disconnect();
      }
    });
    observer.observe(shell, { childList: true, subtree: true });
    return () => observer.disconnect();
  }, [forcedColors, layout]);

  if (query.state.status === "loading") {
    return (
      <section aria-live="polite" className="daemon-state factory-loading" role="status">
        <span aria-hidden="true" className="loading-mark" />
        <div>
          <h1>Loading factory floor</h1>
          <p>Reading configured lines, stage topology, and active work.</p>
        </div>
      </section>
    );
  }
  if (query.state.status === "error") {
    return (
      <section className="daemon-state daemon-state-error" role="alert">
        <div>
          <h1>Factory data unavailable</h1>
          <p>The current plant state could not be read. No substitute data is shown.</p>
        </div>
        <button className="reconnect-button" onClick={query.retry} type="button">
          {standalone ? "Reload" : "Reconnect"}
        </button>
      </section>
    );
  }
  if (query.state.status !== "ready" && query.state.status !== "stale") {
    return null;
  }

  const data = query.state.data;
  const stale = query.state.status === "stale";
  const error = query.state.status === "stale" ? query.state.error : undefined;
  const freshness = freshnessFor(stale, Boolean(error));
  const readState: PlantReadState = {
    data: dataFreshness,
    query: freshness.state,
    transport,
  };
  const risk = summarizePlantRisk({ model: data.model, readState });

  return (
    <div className="factory-page">
      <FactoryHeader
        data={data}
        inspectorOpen={inspectorOpen}
        layout={layout}
        lens={lens}
        navigate={navigate}
        onToggleInspector={() => setInspectorOpen((open) => !open)}
        standalone={standalone}
        toggleRef={inspectorToggleRef}
      />

      <div className="factory-notices">
        {error && (
          <div className="workflow-stale-error" role="alert">
            <span>
              <strong>Floor refresh failed</strong>
              <small>Showing the last confirmed plant state.</small>
            </span>
            <button className="text-button" onClick={query.retry} type="button">
              Try again
            </button>
          </div>
        )}

        {(data.droppedScope.gaggle || data.droppedScope.workflow) && (
          <p className="factory-scope-notice" role="status">
            {data.droppedScope.gaggle
              ? `Gaggle "${data.droppedScope.gaggle}" is not configured on this instance. `
              : ""}
            {data.droppedScope.workflow
              ? `Workflow "${data.droppedScope.workflow}" is not configured in the selected scope. `
              : ""}
            Showing the floor without it.
          </p>
        )}
      </div>

      <FactoryStatusStrip model={data.model} risk={risk} />

      <div className="factory-layout">
        <div className="factory-stage-area">
          <div className="factory-stage-notices">
            {data.model.emptyReason === "no-active-runs" && (
              <p className="factory-idle-note" role="status">
                Plant ready. No active runs are on the floor.
              </p>
            )}
          </div>
          <div
            className="factory-viewport-shell"
            ref={plantShellRef}
          >
          {data.model.emptyReason === "no-gaggles" ||
          data.model.emptyReason === "no-workflows" ? (
            <FactoryEmptyState data={data} standalone={standalone} />
          ) : (
            layout === "plant" ? (
                <FactoryViewport
                  inspectorOpen={inspectorOpen}
                  key="plant"
                  label={
                    forcedColors
                      ? "Factory plant exact high-contrast topology"
                      : "Factory plant"
                  }
                  worldHeight={
                    forcedColors ? data.model.height : CLASSIC_PLANT_HEIGHT
                  }
                  worldWidth={
                    forcedColors ? data.model.width : CLASSIC_PLANT_WIDTH
                  }
                >
                  {forcedColors ? (
                    <div
                      className="factory-forced-colors-exact"
                      data-exact-topology="true"
                    >
                      <p className="factory-forced-colors-notice" role="status">
                        High-contrast mode uses the exact Lines topology.
                      </p>
                      <FactoryFloor
                        animateTransitions={animateTransitions}
                        lens={lens}
                        model={data.model}
                        onSelect={(next) => {
                          setSelection(next);
                          setInspectorOpen(true);
                        }}
                        reducedMotion={reducedMotion}
                        selection={selection}
                      />
                    </div>
                  ) : (
                    <FactoryPlant
                      animateTransitions={animateTransitions}
                      lens={lens}
                      model={data.model}
                      onSelect={(next) => {
                        setSelection(next);
                        setInspectorOpen(true);
                      }}
                      reducedMotion={reducedMotion}
                      readState={readState}
                      selection={selection}
                    />
                  )}
                </FactoryViewport>
              ) : (
                <FactoryViewport
                  inspectorOpen={inspectorOpen}
                  key="lines"
                  label="Factory lines"
                  worldHeight={data.model.height}
                  worldWidth={data.model.width}
                >
                  <FactoryFloor
                    animateTransitions={animateTransitions}
                    lens={lens}
                    model={data.model}
                    onSelect={(next) => {
                      setSelection(next);
                      setInspectorOpen(true);
                    }}
                    reducedMotion={reducedMotion}
                    selection={selection}
                  />
                </FactoryViewport>
              )
          )}
            <div
              aria-hidden={!inspectorOpen}
              className={inspectorOpen ? "factory-inspector-drawer is-open" : "factory-inspector-drawer"}
              inert={!inspectorOpen}
            >
              <button
                aria-label="Close factory inspector"
                className="factory-inspector-close"
                onClick={() => {
                  setInspectorOpen(false);
                  window.requestAnimationFrame(() => inspectorToggleRef.current?.focus());
                }}
                type="button"
              >
                ×
              </button>
              <FactoryInspector
                data={data}
                freshness={freshness}
                onSelect={setSelection}
                selection={selection}
              />
            </div>
          </div>
          <FloorLegend
            layout={layout}
            lens={lens}
            model={data.model}
            risk={risk}
            stale={stale}
          />
        </div>
      </div>
    </div>
  );
}

function FactoryStatusStrip({
  model,
  risk,
}: {
  model: FactoryFloorModel;
  risk: PlantRiskSummary;
}) {
  const workingGoobers = model.counts.goobers - model.counts.idleGoobers;
  const status = model.counts.blockedStages > 0
      ? "Intervention required"
      : model.counts.heldStages > 0
        ? "Human hold"
      : model.runsTruncated
        ? "Partial view"
      : !risk.complete
        ? "Read incomplete"
      : model.counts.unreadRuns > 0
        ? "Signals incomplete"
        : model.counts.activeRuns > 0
          ? "Work in motion"
          : "Ready";
  return (
    <section aria-label="Factory status" className="factory-status-strip">
      <div
        className="factory-status-card factory-status-card-primary"
        data-alert={model.counts.blockedStages > 0 ? "on" : "off"}
        data-state={
          model.counts.blockedStages > 0
              ? "blocked"
              : model.counts.heldStages > 0
                ? "held"
            : model.runsTruncated
              ? "partial"
            : !risk.complete
              ? "incomplete"
                : "normal"
        }
      >
        <span>Plant state</span>
        <strong>{status}</strong>
        <small>
          {`${model.counts.blockedStages} blocked · ${model.counts.heldStages} held · ${model.counts.unreadRuns} unread${
                model.runsTruncated
                  ? " · more active runs exist beyond this batch"
                  : ""
              }${
                risk.complete
                  ? ""
                  : ` · ${risk.detail || "current plant risk cannot be confirmed"}`
              }`}
        </small>
      </div>
      <div className="factory-status-card">
        <span>Work in progress</span>
        <strong>
          {model.counts.activeRuns}
          {model.runsTruncated ? "+" : ""}
        </strong>
        <small>
          {model.runsTruncated ? "partial count; " : ""}
          {model.counts.queuedRuns} waiting at inbound
        </small>
      </div>
      <div className="factory-status-card">
        <span>Floor capacity</span>
        <strong>{model.runsTruncated ? `${model.capacity.wip}+ active` : capacityReadout(model)}</strong>
        <small>
          {model.runsTruncated
            ? model.capacity.limit === undefined
              ? "partial view; workflow limits unread"
              : `partial view; known workflow limits total ${model.capacity.limit}`
            : model.capacity.unknownLimits > 0
            ? `${model.capacity.unknownLimits} limits unread`
            : "configured concurrent limit"}
        </small>
      </div>
      <div className="factory-status-card">
        <span>Goobers posted</span>
        <strong>
          {workingGoobers} / {model.counts.goobers}
        </strong>
        <small>{model.counts.idleGoobers} ready in commons</small>
      </div>
    </section>
  );
}

function capacityReadout(model: FactoryFloorModel): string {
  return model.capacity.limit === undefined
    ? `${model.capacity.wip} / ?`
    : `${model.capacity.wip} / ${model.capacity.limit}`;
}

function freshnessFor(
  stale: boolean,
  errored: boolean,
): { label: string; state: "live" | "refreshing" | "degraded" } {
  if (errored) {
    return { label: "Degraded: showing last confirmed read", state: "degraded" };
  }
  return stale
    ? { label: "Refreshing live reads", state: "refreshing" }
    : { label: "Live", state: "live" };
}

function FactoryHeader({
  data,
  inspectorOpen,
  layout,
  lens,
  navigate,
  onToggleInspector,
  standalone,
  toggleRef,
}: {
  data: FactoryFloorData;
  inspectorOpen: boolean;
  layout: FactoryLayout;
  lens: FactoryLens;
  navigate: Navigate;
  onToggleInspector: () => void;
  standalone: boolean;
  toggleRef: RefObject<HTMLButtonElement | null>;
}) {
  const gaggles = data.inventories.map((inventory) => inventory.gaggle);
  const workflows = data.inventories
    .filter(
      (inventory) => !data.scope.gaggle || inventory.gaggle.name === data.scope.gaggle,
    )
    .flatMap((inventory) => inventory.workflows);
  const go = (next: {
    gaggle?: string;
    workflow?: string;
    lens?: FactoryLens;
    layout?: FactoryLayout;
  }) =>
    navigate({
      page: "factory",
      scope: {
        gaggle: next.gaggle,
        workflow: next.workflow,
        lens: next.lens === "world" ? undefined : next.lens,
        layout: next.layout === DEFAULT_FACTORY_LAYOUT ? undefined : next.layout,
      },
    });

  return (
    <header className="factory-heading">
      <div className="factory-heading-title">
        <p className="page-kicker">Operations floor</p>
        <h1>Factory</h1>
        <p className="sr-only">
          {standalone
            ? "Every configured line, its machines, and the work standing on them, read from this instance."
            : "Every configured line, its machines, and the work standing on them, read live from the daemon."}
        </p>
      </div>

      <div className="factory-controls">
        <label className="factory-control">
          <span>Gaggle</span>
          <select
            onChange={(event) =>
              go({ gaggle: event.target.value || undefined, lens, layout })
            }
            value={data.scope.gaggle ?? ""}
          >
            <option value="">All gaggles</option>
            {gaggles.map((gaggle) => (
              <option key={gaggle.name} value={gaggle.name}>
                {gaggle.displayName}
              </option>
            ))}
          </select>
        </label>
        <label className="factory-control">
          <span>Workflow</span>
          <select
            onChange={(event) =>
              go({
                gaggle: data.scope.gaggle,
                workflow: event.target.value || undefined,
                lens,
                layout,
              })
            }
            value={data.scope.workflow ?? ""}
          >
            <option value="">All workflows</option>
            {[...new Set(workflows.map((workflow) => workflow.identity.name))]
              .sort()
              .map((name) => (
                <option key={name} value={name}>
                  {workflows.find((workflow) => workflow.identity.name === name)
                    ?.displayName ?? name}
                </option>
              ))}
          </select>
        </label>
        <div className="factory-segmented-control">
          <span className="factory-control-caption">Layout</span>
          <div aria-label="Floor layout" className="factory-layout-switch" role="group">
            {FACTORY_LAYOUTS.map((candidate) => (
              <button
                aria-pressed={candidate === layout}
                className={
                  candidate === layout
                    ? "factory-layout-button is-active"
                    : "factory-layout-button"
                }
                key={candidate}
                onClick={() =>
                  go({
                    gaggle: data.scope.gaggle,
                    workflow: data.scope.workflow,
                    lens,
                    layout: candidate,
                  })
                }
                title={factoryLayoutDescription(candidate)}
                type="button"
              >
                {factoryLayoutLabel(candidate)}
              </button>
            ))}
          </div>
          <button
            aria-pressed={inspectorOpen}
            className="factory-inspector-toggle"
            onClick={onToggleInspector}
            ref={toggleRef}
            type="button"
          >
            Inspector
          </button>
        </div>
        <div className="factory-segmented-control">
          <span className="factory-control-caption">Lens</span>
          <div
            aria-label="Floor lens"
            className="factory-lens"
            role="group"
          >
            {FACTORY_LENSES.map((candidate) => (
              <button
                aria-pressed={candidate === lens}
                className={
                  candidate === lens ? "factory-lens-button is-active" : "factory-lens-button"
                }
                key={candidate}
                onClick={() =>
                  go({
                    gaggle: data.scope.gaggle,
                    workflow: data.scope.workflow,
                    lens: candidate,
                    layout,
                  })
                }
                type="button"
              >
                {lensLabel(candidate)}
              </button>
            ))}
          </div>
        </div>
      </div>
    </header>
  );
}

function lensLabel(lens: FactoryLens): string {
  switch (lens) {
    case "world":
      return "World";
    case "flow":
      return "Flow";
    case "risk":
      return "Risk";
  }
}

function FloorLegend({
  layout,
  lens,
  model,
  risk,
  stale,
}: {
  layout: FactoryLayout;
  lens: FactoryLens;
  model: FactoryFloorModel;
  risk: PlantRiskSummary;
  stale: boolean;
}) {
  return (
    <div className="factory-legend">
      <div className="factory-legend-group">
        <strong>Work</strong>
        <span className="factory-legend-item" data-legend="running">Running</span>
        <span className="factory-legend-item" data-legend="paused">Gate pause</span>
        <span className="factory-legend-item" data-legend="blocked">Blocked</span>
        <span className="factory-legend-item" data-legend="unknown">Signal unread</span>
        <span className="factory-legend-item" data-legend="inbound">Inbound</span>
      </div>
      {layout === "plant" ? (
        <div className="factory-legend-group" data-layout-legend="plant">
          <strong>Stage shape</strong>
          <span className="factory-legend-item" data-legend="shape-agentic">Silo: agentic</span>
          <span className="factory-legend-item" data-legend="shape-gate">Arch: gate</span>
          <span className="factory-legend-item" data-legend="shape-evaluator">Beam: evaluator</span>
          <span className="factory-legend-item" data-legend="shape-deterministic">Press: deterministic</span>
          <span className="factory-legend-item" data-legend="shape-parallel">Twin press: parallel</span>
          <span className="factory-legend-item" data-legend="shape-terminal">Cone: terminal</span>
          <span className="factory-legend-item" data-legend="beacon">Beacon: confirmed hazard</span>
          <span className="factory-legend-item" data-legend="unknown">Open marker: unread</span>
          <span className="factory-legend-item" data-legend="observed">Dashed means order unknown</span>
        </div>
      ) : (
        <div className="factory-legend-group" data-layout-legend="lines">
          <strong>Machines</strong>
          <span className="factory-legend-item" data-legend="machine-running">Running</span>
          <span className="factory-legend-item" data-legend="machine-held">Human hold</span>
          <span className="factory-legend-item" data-legend="machine-blocked">Blocked</span>
          <span className="factory-legend-item" data-legend="idle">Idle</span>
          <span className="factory-legend-item" data-legend="gate">Gate</span>
          <span className="factory-legend-item" data-legend="observed">Observed, order unknown</span>
        </div>
      )}
      {lens === "risk" ? (
        <p
          className="factory-legend-risk"
          data-complete={risk.complete ? "true" : "false"}
          data-risk={risk.level}
          role="status"
        >
          <strong>{risk.headline}</strong>
          {risk.detail ? <span> {risk.detail}</span> : null}
        </p>
      ) : null}
      <span className="factory-legend-readout">
        {model.counts.activeRuns}{model.runsTruncated ? "+" : ""} active ·{" "}
        {model.counts.blockedRuns} held · {model.counts.blockedStages} blocked ·{" "}
        {model.counts.heldStages} human hold · {model.counts.unreadRuns} unread
        {model.runsTruncated ? " · partial" : ""}
        {stale ? " · refreshing" : ""}
      </span>
    </div>
  );
}

function useTransitionAnimation(
  model: FactoryFloorModel | undefined,
  layout: FactoryLayout,
): boolean {
  const [state, setState] = useState(() => ({
    animate: false,
    layout,
    model,
  }));

  if (state.model !== model) {
    setState({
      animate:
        state.model !== undefined &&
        model !== undefined &&
        modelHasTransition(model),
      layout,
      model,
    });
  } else if (state.layout !== layout) {
    setState({
      animate: false,
      layout,
      model,
    });
  }

  useEffect(() => {
    if (!state.animate) {
      return;
    }
    const timer = window.setTimeout(() => {
      setState((current) =>
        current === state ? { ...current, animate: false } : current,
      );
    }, 1000);
    return () => window.clearTimeout(timer);
  }, [state]);

  return (
    model !== undefined &&
    state.model === model &&
    state.layout === layout &&
    state.animate
  );
}

function usePlantForcedColors(onBeforeChange: () => void): boolean {
  const [active, setActive] = useState(() => plantForcedColorsActive());
  useEffect(
    () =>
      observePlantForcedColors((next) => {
        onBeforeChange();
        setActive(next);
      }),
    [onBeforeChange],
  );
  return active;
}

function useShortFactoryViewport(): boolean {
  const query = "(max-height: 420px)";
  const [short, setShort] = useState(() =>
    typeof window !== "undefined" && typeof window.matchMedia === "function"
      ? window.matchMedia(query).matches
      : false,
  );
  useEffect(() => {
    if (typeof window.matchMedia !== "function") {
      return;
    }
    const media = window.matchMedia(query);
    const update = () => setShort(media.matches);
    media.addEventListener?.("change", update);
    update();
    return () => media.removeEventListener?.("change", update);
  }, []);
  return short;
}

function modelHasTransition(model: FactoryFloorModel): boolean {
  return model.carriers.some(
    (carrier) => carrier.transition?.kind === "stage-change",
  );
}

function FactoryEmptyState({
  data,
  standalone,
}: {
  data: FactoryFloorData;
  standalone: boolean;
}) {
  const reason = data.model.emptyReason;
  const heading =
    reason === "no-gaggles"
      ? "No gaggles configured"
      : reason === "no-workflows"
        ? "No workflows in this scope"
        : "The floor is idle";
  const body =
    reason === "no-gaggles"
      ? standalone
        ? "This instance has no gaggle definitions loaded, so there is no plant to show."
        : "The daemon reports no gaggle definitions, so there is no plant to show."
      : reason === "no-workflows"
        ? "The selected scope has no workflow definitions. Widen the scope to see other lines."
        : "Every line is configured and staffed, but the daemon reports no running runs right now.";

  return (
    <section className="factory-empty empty-state" role="status">
      <img alt="" src="/goober-mascot.png" />
      <div>
        <h2>{heading}</h2>
        <p>{body}</p>
        <p className="factory-empty-counts">
          {data.model.counts.gaggles} gaggles · {data.model.counts.workflows} workflows ·{" "}
          {data.model.counts.goobers} goobers · {data.model.counts.activeRuns} active runs
        </p>
      </div>
    </section>
  );
}
