/**
 * Deterministic label packing for the Factory Plant overlay.
 *
 * Every positioned semantic on the plant is projected from one camera, so the
 * only remaining question is what to do when two truths land on the same
 * pixels. This solver answers it the same way every time, for the same inputs,
 * in the same order:
 *
 *   selected / focused / alarm  >  blocked / held / unknown  >  active
 *   >  workflow bay sign  >  idle
 *
 * A label that cannot be placed is never drawn on top of a more important one
 * and never quietly dropped: it collapses into a truthful `+N` chip for its
 * group, and the chip is packed under the same rules. Hit targets stay on the
 * geometry they belong to — only labels move — so a packed plant still selects
 * the machine the operator pointed at.
 */

import {
  centeredPlantScreenRect,
  clampPlantRect,
  plantRectsOverlap,
  plantScreenRect,
  type PlantScreenPoint,
  type PlantScreenRect,
} from "./plantProjection";

export type PlantLabelTier =
  | "selected"
  | "focused"
  | "alarm"
  | "attention"
  | "active"
  | "sign"
  | "idle";

/** Descending importance. Ties break on the stable semantic id, never order. */
export const PLANT_LABEL_TIER_PRIORITY: Readonly<Record<PlantLabelTier, number>> =
  Object.freeze({
    active: 400,
    alarm: 800,
    attention: 600,
    focused: 900,
    idle: 100,
    selected: 1000,
    sign: 200,
  });

export const PLANT_LABEL_TIER_ORDER: readonly PlantLabelTier[] = Object.freeze([
  "selected",
  "focused",
  "alarm",
  "attention",
  "active",
  "sign",
  "idle",
]);

/** Minimum pointer target on a precise pointer, in CSS pixels. */
export const PLANT_HIT_TARGET_MIN = 32;
/** Minimum pointer target on a coarse pointer, in CSS pixels. */
export const PLANT_TOUCH_HIT_TARGET_MIN = 44;

export const PLANT_LABEL_HEIGHT = 20;
export const PLANT_LABEL_GAP = 6;
export const PLANT_LABEL_CHARACTER_WIDTH = 6.2;
export const PLANT_LABEL_PADDING = 14;
export const PLANT_LABEL_MIN_WIDTH = 34;
export const PLANT_LABEL_MAX_WIDTH = 168;

export interface PlantLabelSize {
  width: number;
  height: number;
}

export interface PlantLabelCandidate {
  /** Stable semantic id; the same id across frames, models and renderers. */
  id: string;
  /** The aggregate a collapsed label reports into. */
  groupId: string;
  tier: PlantLabelTier;
  /** Explicit override for the tier priority. */
  priority?: number;
  /** The projected anchor, in canvas pixels. */
  origin: PlantScreenPoint;
  /** Preferred label centre, usually just below the anchor. */
  point: PlantScreenPoint;
  size: PlantLabelSize;
  /** False keeps a label out of the collapse path; it is clamped instead. */
  collapsible?: boolean;
}

export interface PlantLabelPlacement {
  id: string;
  groupId: string;
  tier: PlantLabelTier;
  priority: number;
  origin: PlantScreenPoint;
  point: PlantScreenPoint;
  rect: PlantScreenRect;
  /** Offset from the anchor, in canvas pixels; what the DOM applies. */
  offset: PlantScreenPoint;
  offsetIndex: number;
  clamped: boolean;
}

export interface PlantLabelChip {
  id: string;
  groupId: string;
  tier: PlantLabelTier;
  priority: number;
  origin: PlantScreenPoint;
  point: PlantScreenPoint;
  rect: PlantScreenRect;
  offset: PlantScreenPoint;
  count: number;
  ids: string[];
  /** The chip could not find clear space and was pinned inside the safe rect. */
  forced: boolean;
}

export interface PlantLabelPackingMetrics {
  candidates: number;
  placed: number;
  collapsed: number;
  chips: number;
  clamped: number;
  clipped: number;
  /** Residual overlaps after packing. This is the number that must stay zero. */
  overlaps: number;
  /** Labels whose anchor was outside the safe rectangle entirely. */
  offscreen: number;
}

export interface PlantLabelPackingResult {
  placements: PlantLabelPlacement[];
  chips: PlantLabelChip[];
  collapsedIds: string[];
  metrics: PlantLabelPackingMetrics;
}

export interface PlantLabelPackingOptions {
  safeRect: PlantScreenRect;
  /** Fixed rectangles labels must avoid, normally the hit targets. */
  obstacles?: readonly PlantScreenRect[];
  gap?: number;
  /** Spatial hash cell size; only affects speed, never the result. */
  cellSize?: number;
  chipHeight?: number;
  /** Overrides the chip anchor for a group. */
  groupOrigins?: Readonly<Record<string, PlantScreenPoint>>;
}

/** Deterministic width estimate; the DOM is then sized to exactly this. */
export function estimatePlantLabelWidth(
  text: string,
  options: { min?: number; max?: number; padding?: number } = {},
): number {
  const min = options.min ?? PLANT_LABEL_MIN_WIDTH;
  const max = options.max ?? PLANT_LABEL_MAX_WIDTH;
  const padding = options.padding ?? PLANT_LABEL_PADDING;
  const measured = text.length * PLANT_LABEL_CHARACTER_WIDTH + padding;
  return Math.round(Math.min(max, Math.max(min, measured)));
}

export function plantLabelPriority(
  tier: PlantLabelTier,
  override?: number,
): number {
  return override ?? PLANT_LABEL_TIER_PRIORITY[tier];
}

/**
 * Candidate offsets, in preference order.
 *
 * The first entry is "exactly where the label wants to be", so an uncrowded
 * plant places every label on its anchor and measures zero packing drift.
 */
export function plantLabelOffsets(
  size: PlantLabelSize,
  gap = PLANT_LABEL_GAP,
): PlantScreenPoint[] {
  const dx = size.width + gap;
  const dy = size.height + gap;
  return [
    { x: 0, y: 0 },
    { x: 0, y: dy },
    { x: 0, y: -dy },
    { x: dx, y: 0 },
    { x: -dx, y: 0 },
    { x: dx, y: dy },
    { x: -dx, y: dy },
    { x: dx, y: -dy },
    { x: -dx, y: -dy },
    { x: 0, y: dy * 2 },
    { x: 0, y: -dy * 2 },
    { x: dx * 1.5, y: dy },
    { x: -dx * 1.5, y: dy },
    { x: dx * 1.5, y: -dy },
    { x: -dx * 1.5, y: -dy },
    { x: 0, y: dy * 3 },
    { x: 0, y: -dy * 3 },
  ];
}

/**
 * A bounded spatial hash.
 *
 * Label packing is quadratic if every candidate is tested against every placed
 * rectangle, and a 50-workflow plant has enough labels for that to show. The
 * hash bounds the comparison set without changing which placement wins.
 */
export class PlantScreenHash {
  private readonly cells = new Map<string, PlantScreenRect[]>();

  constructor(private readonly cellSize = 64) {}

  insert(rect: PlantScreenRect): void {
    for (const key of this.keys(rect)) {
      const bucket = this.cells.get(key);
      if (bucket) {
        bucket.push(rect);
      } else {
        this.cells.set(key, [rect]);
      }
    }
  }

  overlaps(rect: PlantScreenRect): boolean {
    for (const key of this.keys(rect)) {
      for (const candidate of this.cells.get(key) ?? []) {
        if (plantRectsOverlap(rect, candidate)) {
          return true;
        }
      }
    }
    return false;
  }

  private keys(rect: PlantScreenRect): string[] {
    const size = this.cellSize;
    const minX = Math.floor(rect.left / size);
    const maxX = Math.floor(rect.right / size);
    const minY = Math.floor(rect.top / size);
    const maxY = Math.floor(rect.bottom / size);
    const keys: string[] = [];
    for (let x = minX; x <= maxX; x += 1) {
      for (let y = minY; y <= maxY; y += 1) {
        keys.push(`${x}:${y}`);
      }
    }
    return keys;
  }
}

export function packPlantLabels(
  candidates: readonly PlantLabelCandidate[],
  options: PlantLabelPackingOptions,
): PlantLabelPackingResult {
  const safeRect = options.safeRect;
  const gap = options.gap ?? PLANT_LABEL_GAP;
  const chipHeight = options.chipHeight ?? PLANT_LABEL_HEIGHT;
  const hash = new PlantScreenHash(options.cellSize ?? 64);
  for (const obstacle of options.obstacles ?? []) {
    hash.insert(obstacle);
  }

  const ordered = [...candidates].sort(
    (left, right) =>
      plantLabelPriority(right.tier, right.priority) -
        plantLabelPriority(left.tier, left.priority) ||
      left.id.localeCompare(right.id),
  );

  const placements: PlantLabelPlacement[] = [];
  const collapsedByGroup = new Map<
    string,
    { ids: string[]; tier: PlantLabelTier; priority: number; origin: PlantScreenPoint }
  >();
  const metrics: PlantLabelPackingMetrics = {
    candidates: ordered.length,
    chips: 0,
    clamped: 0,
    clipped: 0,
    collapsed: 0,
    offscreen: 0,
    overlaps: 0,
    placed: 0,
  };

  for (const candidate of ordered) {
    const priority = plantLabelPriority(candidate.tier, candidate.priority);
    const anchorOutside =
      candidate.origin.x < safeRect.left ||
      candidate.origin.x > safeRect.right ||
      candidate.origin.y < safeRect.top ||
      candidate.origin.y > safeRect.bottom;
    if (anchorOutside) {
      metrics.offscreen += 1;
      // A critical semantic is never dissolved into a `+N`. Its anchor may be
      // off the safe rectangle — mid-pan, say — but the operator still has to
      // be able to read and reach it, so it is placed rather than counted.
      if (candidate.collapsible !== false) {
        collapse(collapsedByGroup, candidate, priority);
        metrics.collapsed += 1;
        continue;
      }
    }
    const resolved = place(candidate, candidate.size, hash, safeRect, gap);
    if (!resolved) {
      if (candidate.collapsible === false) {
        // A control that must stay reachable is relocated rather than hidden,
        // and relocated somewhere free rather than stacked on a neighbour.
        const rescued = placeAnywhere(
          candidate.size,
          hash,
          safeRect,
          candidate.point,
          gap,
        );
        const clamp = clampPlantRect(
          centeredPlantScreenRect(candidate.point, candidate.size.width, candidate.size.height),
          safeRect,
        );
        const rect = rescued?.rect ?? clamp.rect;
        hash.insert(rect);
        placements.push({
          clamped: true,
          groupId: candidate.groupId,
          id: candidate.id,
          offset: {
            x: rect.left + rect.width / 2 - candidate.origin.x,
            y: rect.top + rect.height / 2 - candidate.origin.y,
          },
          offsetIndex: -1,
          origin: candidate.origin,
          point: { x: rect.left + rect.width / 2, y: rect.top + rect.height / 2 },
          priority,
          rect,
          tier: candidate.tier,
        });
        metrics.placed += 1;
        metrics.clamped += 1;
        if (!rescued && clamp.clipped) {
          metrics.clipped += 1;
        }
        continue;
      }
      collapse(collapsedByGroup, candidate, priority);
      metrics.collapsed += 1;
      continue;
    }
    if (resolved.clipped) {
      metrics.clipped += 1;
    }
    if (resolved.clamped) {
      metrics.clamped += 1;
    }
    hash.insert(resolved.rect);
    placements.push({
      clamped: resolved.clamped,
      groupId: candidate.groupId,
      id: candidate.id,
      offset: {
        x: resolved.center.x - candidate.origin.x,
        y: resolved.center.y - candidate.origin.y,
      },
      offsetIndex: resolved.offsetIndex,
      origin: candidate.origin,
      point: resolved.center,
      priority,
      rect: resolved.rect,
      tier: candidate.tier,
    });
    metrics.placed += 1;
  }

  const chips: PlantLabelChip[] = [];
  const collapsedIds: string[] = [];
  for (const [groupId, group] of [...collapsedByGroup].sort(([left], [right]) =>
    left.localeCompare(right),
  )) {
    collapsedIds.push(...group.ids);
    const origin = options.groupOrigins?.[groupId] ?? group.origin;
    const text = `+${group.ids.length}`;
    const size = {
      height: chipHeight,
      width: estimatePlantLabelWidth(text, {
        min: Math.max(30, chipHeight),
      }),
    };
    const chipCandidate: PlantLabelCandidate = {
      groupId,
      id: `chip:${groupId}`,
      origin,
      point: { x: origin.x, y: origin.y + chipHeight },
      size,
      tier: group.tier,
    };
    const resolved = place(chipCandidate, size, hash, safeRect, gap);
    const rescued =
      resolved ?? placeAnywhere(size, hash, safeRect, chipCandidate.point, gap);
    const rect =
      rescued?.rect ??
      clampPlantRect(centeredPlantScreenRect(chipCandidate.point, size.width, size.height), safeRect)
        .rect;
    hash.insert(rect);
    const center = { x: rect.left + rect.width / 2, y: rect.top + rect.height / 2 };
    chips.push({
      count: group.ids.length,
      forced: rescued === undefined,
      groupId,
      id: chipCandidate.id,
      ids: [...group.ids].sort(),
      offset: { x: center.x - origin.x, y: center.y - origin.y },
      origin,
      point: center,
      priority: group.priority,
      rect,
      tier: group.tier,
    });
    metrics.chips += 1;
    if (resolved?.clamped) {
      metrics.clamped += 1;
    }
  }

  metrics.overlaps = countOverlaps([
    ...placements.map((placement) => placement.rect),
    ...chips.map((chip) => chip.rect),
  ]);

  return { chips, collapsedIds: collapsedIds.sort(), metrics, placements };
}

interface ResolvedPlacement {
  rect: PlantScreenRect;
  center: PlantScreenPoint;
  offsetIndex: number;
  clamped: boolean;
  clipped: boolean;
}

function place(
  candidate: PlantLabelCandidate,
  size: PlantLabelSize,
  hash: PlantScreenHash,
  safeRect: PlantScreenRect,
  gap: number,
): ResolvedPlacement | undefined {
  const offsets = plantLabelOffsets(size, gap);
  for (let index = 0; index < offsets.length; index += 1) {
    const offset = offsets[index];
    const proposed = centeredPlantScreenRect(
      { x: candidate.point.x + offset.x, y: candidate.point.y + offset.y },
      size.width,
      size.height,
    );
    const clamp = clampPlantRect(proposed, safeRect);
    if (clamp.clipped) {
      continue;
    }
    if (hash.overlaps(clamp.rect)) {
      continue;
    }
    return {
      center: {
        x: clamp.rect.left + clamp.rect.width / 2,
        y: clamp.rect.top + clamp.rect.height / 2,
      },
      clamped: clamp.clamped,
      clipped: clamp.clipped,
      offsetIndex: index,
      rect: clamp.rect,
    };
  }
  return undefined;
}

/**
 * Last-resort placement: a deterministic scan of the safe rectangle.
 *
 * The offset ladder only reaches a few label-widths from the anchor, so a
 * crowded neighbourhood can exhaust it while the rest of the plant is empty.
 * A chip or a non-collapsible control must still land somewhere that does not
 * cover another label, otherwise "never overlap silently" is a slogan rather
 * than a guarantee. The scan is ordered by distance from the preferred point,
 * so the result is stable and as close to the anchor as the space allows.
 */
function placeAnywhere(
  size: PlantLabelSize,
  hash: PlantScreenHash,
  safeRect: PlantScreenRect,
  preferred: PlantScreenPoint,
  gap: number,
): ResolvedPlacement | undefined {
  const stepX = Math.max(8, (size.width + gap) / 2);
  const stepY = Math.max(8, (size.height + gap) / 2);
  const minX = safeRect.left + size.width / 2;
  const maxX = safeRect.right - size.width / 2;
  const minY = safeRect.top + size.height / 2;
  const maxY = safeRect.bottom - size.height / 2;
  if (minX > maxX || minY > maxY) {
    return undefined;
  }
  const columns = Math.max(1, Math.floor((maxX - minX) / stepX) + 1);
  const rows = Math.max(1, Math.floor((maxY - minY) / stepY) + 1);
  const slots: { center: PlantScreenPoint; distance: number }[] = [];
  for (let row = 0; row < rows; row += 1) {
    for (let column = 0; column < columns; column += 1) {
      const center = {
        x: Math.min(maxX, minX + column * stepX),
        y: Math.min(maxY, minY + row * stepY),
      };
      slots.push({
        center,
        distance: Math.hypot(center.x - preferred.x, center.y - preferred.y),
      });
    }
  }
  slots.sort(
    (left, right) =>
      left.distance - right.distance ||
      left.center.y - right.center.y ||
      left.center.x - right.center.x,
  );
  for (const slot of slots) {
    const rect = centeredPlantScreenRect(slot.center, size.width, size.height);
    if (hash.overlaps(rect)) {
      continue;
    }
    return {
      center: slot.center,
      clamped: true,
      clipped: false,
      offsetIndex: -1,
      rect,
    };
  }
  return undefined;
}

function collapse(
  groups: Map<
    string,
    { ids: string[]; tier: PlantLabelTier; priority: number; origin: PlantScreenPoint }
  >,
  candidate: PlantLabelCandidate,
  priority: number,
): void {
  const existing = groups.get(candidate.groupId);
  if (!existing) {
    groups.set(candidate.groupId, {
      ids: [candidate.id],
      origin: candidate.origin,
      priority,
      tier: candidate.tier,
    });
    return;
  }
  existing.ids.push(candidate.id);
  if (priority > existing.priority) {
    existing.priority = priority;
    existing.tier = candidate.tier;
  }
}

function countOverlaps(rects: readonly PlantScreenRect[]): number {
  let overlaps = 0;
  for (let index = 0; index < rects.length; index += 1) {
    for (let other = index + 1; other < rects.length; other += 1) {
      if (plantRectsOverlap(rects[index], rects[other])) {
        overlaps += 1;
      }
    }
  }
  return overlaps;
}

/**
 * The hit-target rectangle a semantic control occupies at its anchor.
 *
 * The minimum is enforced here rather than left to each caller, so a control
 * can never be rendered below the size a person can actually hit.
 */
export function plantHitRect(
  origin: PlantScreenPoint,
  size: number = PLANT_HIT_TARGET_MIN,
  touch = false,
): PlantScreenRect {
  const minimum = touch ? PLANT_TOUCH_HIT_TARGET_MIN : PLANT_HIT_TARGET_MIN;
  const resolved = Math.max(minimum, size);
  return plantScreenRect(
    origin.x - resolved / 2,
    origin.y - resolved / 2,
    resolved,
    resolved,
  );
}
