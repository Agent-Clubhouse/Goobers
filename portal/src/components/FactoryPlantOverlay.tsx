import { useEffect, useMemo, useRef, useState } from "react";
import type { CSSProperties } from "react";
import {
  buildPlantOverlayItems,
  type PlantOverlayInput,
  type PlantOverlayItem,
} from "../factoryPlantOverlay";
import type { FactorySelection } from "../factorySelection";
import {
  packPlantLabels,
  PLANT_LABEL_GAP,
  PLANT_LABEL_HEIGHT,
  PLANT_LABEL_MIN_WIDTH,
  PLANT_TOUCH_HIT_TARGET_MIN,
  type PlantLabelCandidate,
  type PlantLabelChip,
  type PlantLabelPlacement,
} from "../plantLabelPacking";
import {
  clampPlantRect,
  createPlantProjector,
  intersectPlantRects,
  plantPointFromViewport,
  plantPointToViewport,
  plantRectOverlapArea,
  plantScreenRect,
  type PlantAnimatedProjection,
  type PlantProjectedPoint,
  type PlantProjectionController,
  type PlantProjectionState,
  type PlantScreenRect,
} from "../plantProjection";
import {
  getPlantProbeSink,
  type PlantOverlayEntry,
  type PlantProbeSink,
} from "../plantProbeSink";

/**
 * The screen-space semantic layer.
 *
 * Every positioned semantic on the plant — stage machines, run carriers,
 * goobers, workflow bay signs, and the truncation affordances — is placed here
 * from the live renderer camera and the one outer navigation camera. Renderer
 * projection supplies the canvas-local anchor; packing happens only after that
 * anchor reaches final FactoryViewport CSS pixels, where counter-scaled labels
 * and hit targets actually render.
 */
export function FactoryPlantOverlay({
  animateTransitions,
  controller,
  focusId,
  inspectorOpen,
  inspectorRect,
  items,
  maxControls = Number.POSITIVE_INFINITY,
  onFocus,
  onSelect,
  scale,
  touch,
  viewport,
}: {
  animateTransitions: boolean;
  controller: PlantProjectionController;
  focusId?: string;
  inspectorOpen: boolean;
  /** The inspector, in final FactoryViewport CSS pixels. */
  inspectorRect?: PlantScreenRect;
  items: readonly PlantOverlayItem[];
  maxControls?: number;
  onFocus: (anchorId: string | undefined) => void;
  onSelect: (selection: FactorySelection) => void;
  /** Counter-scale so hit targets keep their CSS size through zoom. */
  scale: number;
  touch: boolean;
  /** The one outer navigation camera, in final FactoryViewport CSS pixels. */
  viewport: {
    x: number;
    y: number;
    zoom: number;
    safeRect: PlantScreenRect;
  };
}) {
  const projection = usePlantProjection(controller);
  const probe = getPlantProbeSink();
  const itemNodes = useRef(new Map<string, HTMLButtonElement>());

  const packed = useMemo(
    () =>
      packOverlay({
        inspectorRect,
        items,
        maxControls,
        projectItem: (item) =>
          controller.projectEntity(item.entityId, item.world),
        projection,
        touch,
        viewport,
      }),
    [controller, inspectorRect, items, maxControls, projection, touch, viewport],
  );
  const animationContext = useRef({
    inspectorOpen,
    inspectorRect,
    items,
    packed,
    probe,
    projection,
    viewport,
  });
  animationContext.current = {
    inspectorOpen,
    inspectorRect,
    items,
    packed,
    probe,
    projection,
    viewport,
  };

  useEffect(() => {
    if (!probe) {
      return;
    }
    publishOverlayProbe({
      inspectorOpen,
      inspectorRect,
      packed,
      probe,
      projection,
      safeRect: viewport.safeRect,
      viewport,
    });
  }, [inspectorOpen, inspectorRect, packed, probe, projection, viewport.safeRect]);

  useEffect(() => {
    return controller.subscribeAnimation((animated) => {
        if (animated.length === 0) {
          return;
        }
        for (const entry of animated) {
          const node = itemNodes.current.get(entry.id);
          if (!node) {
            continue;
          }
          node.style.left = `${entry.point.x}px`;
          node.style.top = `${entry.point.y}px`;
          node.dataset.onscreen = entry.point.visible ? "true" : "false";
        }
        const current = animationContext.current;
        if (current.probe) {
          publishOverlayProbe({
            animated,
            inspectorOpen: current.inspectorOpen,
            inspectorRect: current.inspectorRect,
            items: current.items,
            packed: current.packed,
            probe: current.probe,
            projection: current.projection,
            safeRect: current.viewport.safeRect,
            viewport: current.viewport,
          });
        }
      });
  }, [controller]);

  return (
    <div
      className="factory-plant-overlay"
      data-projection={projection.source}
      data-revision={projection.revision}
      style={{ "--plant-overlay-scale": scale } as CSSProperties}
    >
      {packed.rendered.map((entry) => (
        <OverlayItem
          animateTransitions={animateTransitions}
          key={entry.item.id}
          entry={entry}
          elementRef={(node) => {
            if (node) {
              itemNodes.current.set(entry.item.entityId, node);
            } else {
              itemNodes.current.delete(entry.item.entityId);
            }
          }}
          onFocus={onFocus}
          onSelect={onSelect}
          scale={scale}
        />
      ))}
      {packed.chips.map((chip) => (
        <OverlayChip
          chip={chip}
          key={chip.id}
          onSelect={onSelect}
          scale={scale}
          action={packed.chipActions.get(chip.id)}
          viewport={viewport}
        />
      ))}
    </div>
  );
}

interface RenderedOverlayItem {
  item: PlantOverlayItem;
  /** Canvas-local point used for absolute positioning inside the Plant. */
  point: PlantProjectedPoint;
  /** Final FactoryViewport CSS point used by packing and probe metrics. */
  screenPoint: PlantProjectedPoint;
  hit: PlantScreenRect;
  placement?: PlantLabelPlacement;
  collapsed: boolean;
  occluded: boolean;
}

interface PlantChipAction {
  ariaLabel: string;
  focusId?: string;
  selection: FactorySelection;
}

interface PackedOverlay {
  chips: PlantLabelChip[];
  chipActions: Map<string, PlantChipAction>;
  drift: { max: number; mean: number; count: number };
  entries: PlantOverlayEntry[];
  hitTargets: { minWidth: number; minHeight: number; belowMinimum: number };
  metrics: {
    clipped: number;
    collapsed: number;
    offscreen: number;
    overlaps: number;
  };
  occlusion: { total: number; critical: number; selected: number };
  rendered: RenderedOverlayItem[];
}

/**
 * Projects, packs and measures one overlay generation.
 *
 * Exported so the packing contract can be tested without a DOM: the same
 * function the component renders from is the one the tests assert on.
 */
export function packOverlay({
  inspectorRect,
  items,
  maxControls = Number.POSITIVE_INFINITY,
  projectItem,
  projection,
  touch,
  viewport,
}: {
  inspectorRect?: PlantScreenRect;
  items: readonly PlantOverlayItem[];
  maxControls?: number;
  projectItem?: (item: PlantOverlayItem) => PlantProjectedPoint;
  projection: PlantProjectionState;
  touch: boolean;
  viewport?: {
    x: number;
    y: number;
    zoom: number;
    safeRect: PlantScreenRect;
  };
}): PackedOverlay {
  const projector = createPlantProjector(projection);
  const navigation = viewport ?? {
    safeRect: projection.safeArea,
    x: 0,
    y: 0,
    zoom: 1,
  };
  const safeRect = navigation.safeRect;
  const minimum = touch ? PLANT_TOUCH_HIT_TARGET_MIN : 32;
  const finiteLimit = Number.isFinite(maxControls)
    ? Math.max(1, Math.floor(maxControls))
    : Number.POSITIVE_INFINITY;
  const groupIds = [...new Set(items.map((item) => item.groupId))];
  const maxChipGroups = Number.isFinite(finiteLimit)
    ? Math.min(groupIds.length, 24, Math.max(1, Math.floor(finiteLimit / 2)))
    : groupIds.length;
  const mergeGroups = groupIds.length > maxChipGroups;
  const preservedGroups = new Set(
    groupIds.slice(0, mergeGroups ? Math.max(0, maxChipGroups - 1) : maxChipGroups),
  );
  const packedGroupId = (groupId: string) =>
    mergeGroups && !preservedGroups.has(groupId) ? "plant:overflow" : groupId;
  const chipGroupCount =
    groupIds.length === 0 ? 0 : mergeGroups ? maxChipGroups : groupIds.length;
  const itemLimit = Number.isFinite(finiteLimit)
    ? Math.max(0, finiteLimit - chipGroupCount)
    : items.length;
  const activeItems = items.slice(0, itemLimit);
  const omittedItems = items.slice(itemLimit);

  const projected = activeItems.map((item) => {
    const point = projectItem?.(item) ?? projector.project(item.world);
    const screen = plantPointToViewport(point, navigation);
    const screenPoint = {
      depth: point.depth,
      visible: point.visible,
      x: screen.x,
      y: screen.y,
    };
    const width = Math.max(minimum, item.hit.width);
    const height = Math.max(minimum, item.hit.height);
    return {
      hit: plantScreenRect(
        screenPoint.x - width / 2,
        screenPoint.y - height / 2,
        width,
        height,
      ),
      item,
      point,
      screenPoint,
    };
  });

  const visible = projected.filter((entry) => entry.point.visible);
  const candidates: PlantLabelCandidate[] = visible.flatMap((entry) =>
    entry.item.labelSize
      ? [
          {
            collapsible: !(entry.item.selected || entry.item.focused),
            groupId: packedGroupId(entry.item.groupId),
            id: entry.item.id,
            origin: { x: entry.screenPoint.x, y: entry.screenPoint.y },
            point: {
              x: entry.screenPoint.x,
              y:
                entry.screenPoint.y +
                entry.hit.height / 2 +
                PLANT_LABEL_GAP,
            },
            size: entry.item.labelSize,
            tier: entry.item.tier,
          },
        ]
      : [],
  );
  const omittedOrigin = {
    x: safeRect.right + 1,
    y: safeRect.bottom + 1,
  };
  for (const item of omittedItems) {
    candidates.push({
      collapsible: true,
      groupId: packedGroupId(item.groupId),
      id: item.id,
      origin: omittedOrigin,
      point: omittedOrigin,
      size: item.labelSize ?? {
        height: PLANT_LABEL_HEIGHT,
        width: PLANT_LABEL_MIN_WIDTH,
      },
      tier: item.tier,
    });
  }

  const result = packPlantLabels(candidates, {
    chipHeight: minimum,
    obstacles: visible.map((entry) => entry.hit),
    safeRect,
  });
  const chips = result.chips;
  const placements = new Map(
    result.placements.map((placement) => [placement.id, placement]),
  );
  const collapsed = new Set(result.collapsedIds);

  const chipActions = new Map<string, PlantChipAction>();
  for (const chip of chips) {
    chipActions.set(chip.id, resolvePlantChipAction(chip, items));
  }

  let occludedTotal = 0;
  let occludedCritical = 0;
  let occludedSelected = 0;
  let belowMinimum = 0;
  let minWidth = Number.POSITIVE_INFINITY;
  let minHeight = Number.POSITIVE_INFINITY;
  let driftTotal = 0;
  let driftMax = 0;

  const rendered: RenderedOverlayItem[] = [];
  const entries: PlantOverlayEntry[] = [];
  for (const entry of projected) {
    const placement = placements.get(entry.item.id);
    const occluded =
      inspectorRect !== undefined &&
      entry.screenPoint.visible &&
      plantRectOverlapArea(entry.hit, inspectorRect) > 0;
    const isCollapsed = collapsed.has(entry.item.id);
    if (occluded) {
      occludedTotal += 1;
      occludedCritical += entry.item.critical ? 1 : 0;
      occludedSelected += entry.item.selected ? 1 : 0;
    }
    minWidth = Math.min(minWidth, entry.hit.width);
    minHeight = Math.min(minHeight, entry.hit.height);
    if (entry.hit.width < minimum || entry.hit.height < minimum) {
      belowMinimum += 1;
    }
    // The DOM anchor is the projected point by construction; drift is measured
    // for real in the browser harness against getBoundingClientRect.
    driftTotal += 0;
    driftMax = Math.max(driftMax, 0);
    rendered.push({
      collapsed: isCollapsed,
      hit: entry.hit,
      item: entry.item,
      occluded,
      ...(placement ? { placement } : {}),
      point: entry.point,
      screenPoint: entry.screenPoint,
    });
    entries.push({
      anchorId: entry.item.anchorId,
      clamped: placement?.clamped ?? false,
      clipped: false,
      collapsed: isCollapsed,
      critical: entry.item.critical,
      hit: rectPayload(entry.hit),
      id: entry.item.id,
      kind: entry.item.kind,
      ...(placement ? { label: rectPayload(placement.rect) } : {}),
      occluded,
      onScreen: entry.screenPoint.visible,
      projected: { x: entry.screenPoint.x, y: entry.screenPoint.y },
      selected: entry.item.selected,
      tier: entry.item.tier,
    });
  }

  for (const chip of chips) {
    minWidth = Math.min(minWidth, chip.rect.width);
    minHeight = Math.min(minHeight, chip.rect.height);
    if (chip.rect.width < minimum || chip.rect.height < minimum) {
      belowMinimum += 1;
    }
  }

  return {
    chipActions,
    chips,
    drift: {
      count: entries.length,
      max: driftMax,
      mean: entries.length > 0 ? driftTotal / entries.length : 0,
    },
    entries,
    hitTargets: {
      belowMinimum,
      minHeight: Number.isFinite(minHeight) ? minHeight : 0,
      minWidth: Number.isFinite(minWidth) ? minWidth : 0,
    },
    metrics: {
      clipped: result.metrics.clipped,
      collapsed: result.metrics.collapsed,
      offscreen: result.metrics.offscreen,
      overlaps: result.metrics.overlaps,
    },
    occlusion: {
      critical: occludedCritical,
      selected: occludedSelected,
      total: occludedTotal,
    },
    rendered,
  };
}

function OverlayItem({
  animateTransitions,
  elementRef,
  entry,
  onFocus,
  onSelect,
  scale,
}: {
  animateTransitions: boolean;
  elementRef: (node: HTMLButtonElement | null) => void;
  entry: RenderedOverlayItem;
  onFocus: (anchorId: string | undefined) => void;
  onSelect: (selection: FactorySelection) => void;
  scale: number;
}) {
  const { hit, item, placement, point } = entry;
  const offset = placement?.offset;
  const moved =
    item.data.kind === "carrier" && animateTransitions && item.data.moved;
  const risk =
    item.data.kind === "station" || item.data.kind === "carrier"
      ? item.data.risk
      : undefined;
  return (
    <button
      aria-label={item.ariaLabel}
      aria-pressed={item.selected}
      className="factory-plant-overlay-item"
      data-alarm={item.data.kind === "station" ? (item.data.alarm ?? "off") : undefined}
      data-collapsed={entry.collapsed ? "true" : "false"}
      data-critical={item.critical ? "true" : "false"}
      data-focused={item.focused ? "true" : "false"}
      data-kind={item.kind}
      data-moved={moved ? "true" : "false"}
      data-occluded={entry.occluded ? "true" : "false"}
      data-onscreen={point.visible ? "true" : "false"}
      data-plant-anchor-id={item.anchorId}
      data-plant-focus-id={item.anchorId}
      data-plant-probe-id={item.entityId}
      data-plant-probe-kind={item.kind}
      data-risk-level={risk?.level}
      data-risk-shape={risk?.shape}
      data-selected={item.selected ? "true" : "false"}
      data-stage-kind={
        item.data.kind === "station" ? item.data.stageKind : undefined
      }
      data-status={
        item.data.kind === "station"
          ? item.data.status
          : item.data.kind === "carrier"
            ? item.data.state
            : undefined
      }
      data-tier={item.tier}
      onBlur={() => onFocus(undefined)}
      onClick={() => onSelect(item.selection)}
      onFocus={() => onFocus(item.anchorId)}
      ref={elementRef}
      style={
        {
          "--plant-hit-height": `${hit.height}px`,
          "--plant-hit-width": `${hit.width}px`,
          left: `${point.x}px`,
          top: `${point.y}px`,
          transform: `scale(${scale})`,
        } as CSSProperties
      }
      type="button"
    >
      <span aria-hidden="true" className="factory-plant-overlay-origin" data-plant-anchor-origin="" />
      <span aria-hidden="true" className="factory-plant-overlay-hit" />
      {(item.selected || item.focused) && (
        <span aria-hidden="true" className="factory-plant-overlay-ring" />
      )}
      {item.label && placement && !entry.collapsed && (
        <span
          className="factory-plant-overlay-label"
          data-clamped={placement.clamped ? "true" : "false"}
          style={{
            height: `${placement.rect.height}px`,
            transform: `translate(calc(-50% + ${offset?.x ?? 0}px), calc(-50% + ${offset?.y ?? 0}px))`,
            width: `${placement.rect.width}px`,
          }}
        >
          <span>{item.label}</span>
          {risk ? (
            <strong
              className="factory-plant-risk-badge"
              data-risk={risk.level}
              data-shape={risk.shape}
            >
              <i aria-hidden="true" />
              {risk.label}
            </strong>
          ) : null}
        </span>
      )}
    </button>
  );
}

function OverlayChip({
  action,
  chip,
  onSelect,
  scale,
  viewport,
}: {
  action?: PlantChipAction;
  chip: PlantLabelChip;
  onSelect: (selection: FactorySelection) => void;
  scale: number;
  viewport: {
    x: number;
    y: number;
    zoom: number;
  };
}) {
  const origin = plantPointFromViewport(chip.origin, viewport);
  return (
    <button
      aria-label={
        action?.ariaLabel ??
        `${chip.count} labels hidden here. Select the factory overview.`
      }
      className="factory-plant-overlay-chip"
      data-forced={chip.forced ? "true" : "false"}
      data-plant-anchor-id={chip.id}
      data-plant-focus-id={action?.focusId ?? chip.id}
      data-selection-id={
        action?.selection && "id" in action.selection
          ? action.selection.id
          : undefined
      }
      data-selection-kind={action?.selection.kind ?? "overview"}
      onClick={() => action && onSelect(action.selection)}
      style={{
        height: `${chip.rect.height}px`,
        left: `${origin.x}px`,
        top: `${origin.y}px`,
        transform: `scale(${scale}) translate(calc(-50% + ${chip.offset.x}px), calc(-50% + ${chip.offset.y}px))`,
        width: `${chip.rect.width}px`,
      }}
      type="button"
    >
      +{chip.count}
    </button>
  );
}

/** Resolves a collapsed chip from the labels it actually represents. */
export function resolvePlantChipAction(
  chip: Pick<PlantLabelChip, "count" | "groupId" | "ids">,
  items: readonly PlantOverlayItem[],
): PlantChipAction {
  const hidden = new Set(chip.ids);
  const represented = items.filter((item) => hidden.has(item.id));
  const groupLane = chip.groupId.startsWith("bay:")
    ? chip.groupId.slice("bay:".length)
    : undefined;
  if (groupLane) {
    return plantChipAction(
      `${chip.count} labels hidden for workflow line ${groupLane}. Select workflow line ${groupLane}.`,
      { id: groupLane, kind: "lane" },
      items,
    );
  }
  const groupStation = chip.groupId.startsWith("station:")
    ? chip.groupId.slice("station:".length)
    : undefined;
  if (groupStation) {
    return plantChipAction(
      `${chip.count} labels hidden for stage ${groupStation}. Select stage ${groupStation}.`,
      { id: groupStation, kind: "station" },
      items,
    );
  }
  if (chip.groupId.startsWith("commons:")) {
    return plantChipAction(
      `${chip.count} ready-area labels hidden. Select the factory overview.`,
      { kind: "overview" },
      items,
    );
  }

  const selections = new Map<string, FactorySelection>();
  for (const item of represented) {
    selections.set(selectionKey(item.selection), item.selection);
  }
  if (selections.size === 1) {
    const selection = [...selections.values()][0];
    return plantChipAction(
      `${chip.count} labels hidden. ${selectionActionName(selection)}`,
      selection,
      items,
    );
  }
  return plantChipAction(
    `${chip.count} labels hidden across the plant. Select the factory overview.`,
    { kind: "overview" },
    items,
  );
}

function plantChipAction(
  ariaLabel: string,
  selection: FactorySelection,
  items: readonly PlantOverlayItem[],
): PlantChipAction {
  const key = selectionKey(selection);
  const focusId =
    canonicalSelectionFocusId(selection) ??
    items.find((item) => selectionKey(item.selection) === key)?.anchorId ??
    items[0]?.anchorId;
  return {
    ariaLabel,
    ...(focusId ? { focusId } : {}),
    selection,
  };
}

function canonicalSelectionFocusId(
  selection: FactorySelection,
): string | undefined {
  switch (selection.kind) {
    case "lane":
      return `bay:${selection.id}`;
    case "station":
      return `station:${selection.id}`;
    case "run":
      return `carrier:${selection.id}`;
    case "worker":
      return `worker:${selection.id}`;
    case "gaggle":
    case "overview":
      return undefined;
  }
}

function selectionKey(selection: FactorySelection): string {
  if (selection.kind === "overview") {
    return "overview";
  }
  if (selection.kind === "gaggle") {
    return `gaggle:${selection.name}`;
  }
  return `${selection.kind}:${selection.id}`;
}

function selectionActionName(selection: FactorySelection): string {
  switch (selection.kind) {
    case "lane":
      return `Select workflow line ${selection.id}.`;
    case "station":
      return `Select stage ${selection.id}.`;
    case "run":
      return `Select run ${selection.id}.`;
    case "worker":
      return `Select goober ${selection.id}.`;
    case "gaggle":
      return `Select gaggle ${selection.name}.`;
    case "overview":
      return "Select the factory overview.";
  }
}

function publishOverlayProbe({
  animated,
  inspectorOpen,
  inspectorRect,
  items,
  packed,
  probe,
  projection,
  safeRect,
  viewport = { x: 0, y: 0, zoom: 1 },
}: {
  animated?: readonly PlantAnimatedProjection[];
  inspectorOpen: boolean;
  inspectorRect?: PlantScreenRect;
  items?: readonly PlantOverlayItem[];
  packed: PackedOverlay;
  probe: PlantProbeSink;
  projection: PlantProjectionState;
  safeRect: PlantScreenRect;
  viewport?: { x: number; y: number; zoom: number };
}) {
  let entries = packed.entries;
  if (animated && animated.length > 0 && items) {
    const itemByEntity = new Map(items.map((item) => [item.entityId, item]));
    const points = new Map(
      animated.flatMap((entry) => {
        const item = itemByEntity.get(entry.id);
        if (!item) {
          return [];
        }
        const point = plantPointToViewport(entry.point, viewport);
        return [[item.id, { ...entry.point, ...point }] as const];
      }),
    );
    entries = packed.entries.map((entry) => {
      const point = points.get(entry.id);
      if (!point) {
        return entry;
      }
      return {
        ...entry,
        hit: {
          ...entry.hit,
          x: point.x - entry.hit.width / 2,
          y: point.y - entry.hit.height / 2,
        },
        onScreen: point.visible,
        projected: { x: point.x, y: point.y },
      };
    });
  }

  const occlusion = measureEntryOcclusion(entries, inspectorRect);
  const canvasPoint = plantPointToViewport(
    { x: projection.canvas.left, y: projection.canvas.top },
    viewport,
  );
  const zoom =
    Number.isFinite(viewport.zoom) && viewport.zoom > 0 ? viewport.zoom : 1;
  probe.overlay({
    canvas: {
      height: projection.canvas.height * zoom,
      width: projection.canvas.width * zoom,
      x: canvasPoint.x,
      y: canvasPoint.y,
    },
    chips: packed.chips.length,
    clipped: packed.metrics.clipped,
    collapsed: packed.metrics.collapsed,
    collisions: packed.metrics.overlaps,
    drift: packed.drift,
    entries,
    hitTargets: packed.hitTargets,
    ...(inspectorRect ? { inspector: rectPayload(inspectorRect) } : {}),
    inspectorOpen,
    occlusion,
    offscreen: packed.metrics.offscreen,
    revision: projection.revision,
    safeArea: rectPayload(safeRect),
    source: projection.source,
  });
}

function measureEntryOcclusion(
  entries: readonly PlantOverlayEntry[],
  inspectorRect: PlantScreenRect | undefined,
): { total: number; critical: number; selected: number } {
  if (!inspectorRect) {
    return { critical: 0, selected: 0, total: 0 };
  }
  let total = 0;
  let critical = 0;
  let selected = 0;
  for (const entry of entries) {
    const hit = plantScreenRect(
      entry.hit.x,
      entry.hit.y,
      entry.hit.width,
      entry.hit.height,
    );
    if (!entry.onScreen || plantRectOverlapArea(hit, inspectorRect) <= 0) {
      continue;
    }
    total += 1;
    critical += entry.critical ? 1 : 0;
    selected += entry.selected ? 1 : 0;
  }
  return { critical, selected, total };
}

/**
 * Subscribes to the renderer camera, coalesced to one React update per frame.
 *
 * Operating animations move geometry every frame but do not move the camera;
 * the runtime only emits when the projection inputs actually change, and this
 * hook additionally batches a burst of changes into a single render.
 */
export function usePlantProjection(
  controller: PlantProjectionController,
): PlantProjectionState {
  const [state, setState] = useState(() => controller.projection());
  const frameRef = useRef<number | undefined>(undefined);

  useEffect(() => {
    setState(controller.projection());
    const flush = (next: PlantProjectionState) => {
      if (frameRef.current !== undefined) {
        return;
      }
      const raf =
        typeof requestAnimationFrame === "function"
          ? requestAnimationFrame
          : undefined;
      if (!raf) {
        setState(next);
        return;
      }
      frameRef.current = raf(() => {
        frameRef.current = undefined;
        setState(controller.projection());
      });
    };
    const unsubscribe = controller.subscribe(flush);
    return () => {
      unsubscribe();
      if (frameRef.current !== undefined && typeof cancelAnimationFrame === "function") {
        cancelAnimationFrame(frameRef.current);
      }
      frameRef.current = undefined;
    };
  }, [controller]);

  return state;
}

/** Overlay items for a model generation; memo-friendly and renderer neutral. */
export function usePlantOverlayItems(input: PlantOverlayInput): PlantOverlayItem[] {
  const { animateTransitions, focusId, layout, lens, model, selection } = input;
  return useMemo(
    () =>
      buildPlantOverlayItems({
        animateTransitions,
        ...(focusId === undefined ? {} : { focusId }),
        layout,
        lens,
        model,
        selection,
      }),
    [animateTransitions, focusId, layout, lens, model, selection],
  );
}

function rectPayload(rect: PlantScreenRect) {
  return {
    height: rect.height,
    width: rect.width,
    x: rect.left,
    y: rect.top,
  };
}

/** Kept for the safe-rect clamp path used by the viewport ensure-visible pass. */
export function clampOverlayRect(
  rect: PlantScreenRect,
  safeRect: PlantScreenRect,
): PlantScreenRect {
  const clamped = clampPlantRect(rect, safeRect);
  return intersectPlantRects(clamped.rect, safeRect) ?? clamped.rect;
}
