import { comparePosition, type Position } from "./queryFamily";

/**
 * The window a bounded list holds, and the rules for changing it (#1930, §8.2).
 *
 * Pure functions over plain data, deliberately separated from the React hook:
 * the interesting behaviour here is *what happens to a loaded window when an
 * event arrives*, and that is exactly the part that was untestable while it
 * lived inside a `useEffect`.
 *
 * # The bug this is built around (#1713)
 *
 * A live `run` invalidation called `refresh()`, and `refresh()` reset the cursor
 * to `undefined` — discarding every page the operator had loaded and snapping
 * them back to the first 50 rows. On a busy instance that happened every few
 * seconds, which made deep paging effectively impossible: the further you
 * paged, the more work an arriving event destroyed.
 *
 * The fix is that an invalidation PATCHES the loaded window instead of
 * rebuilding it. Pagination resets for exactly two reasons — the scope changed,
 * or the user asked — and never because data arrived.
 */

/** One item in a bounded list. Only identity and ordering are required here;
 * everything else is the caller's business. */
export interface ListItem {
  id: string;
}

/** The ordering key. Lists are keyset-paginated in descending recency with an
 * ascending id tiebreak, matching the server's ORDER BY exactly — a client that
 * ordered differently would merge pages into the wrong sequence. */
export interface OrderKey {
  startedAt: string;
  id: string;
}

export interface ListWindow<T extends ListItem> {
  items: T[];
  /** Cursor for the next page, or undefined when the window is exhausted. */
  nextCursor: string | undefined;
  /** Whether more pages exist beyond the loaded window. */
  hasMore: boolean;
  /** The projection position the window reflects. */
  position: Position | undefined;
}

export function emptyWindow<T extends ListItem>(): ListWindow<T> {
  return { items: [], nextCursor: undefined, hasMore: false, position: undefined };
}

/** Descending by startedAt, ascending by id — the server's order. */
export function compareOrder(a: OrderKey, b: OrderKey): number {
  if (a.startedAt !== b.startedAt) return a.startedAt < b.startedAt ? 1 : -1;
  if (a.id === b.id) return 0;
  return a.id < b.id ? -1 : 1;
}

export interface ApplyOptions<T extends ListItem> {
  /** How to read an item's ordering key. */
  orderOf: (item: T) => OrderKey;
}

/**
 * Append a freshly-fetched page to the window.
 *
 * Used for "load more". Appends rather than merges: the server returned the
 * rows that follow the cursor, so they belong at the end by construction, and
 * re-sorting would only hide a server-side ordering bug.
 */
export function appendPage<T extends ListItem>(
  window: ListWindow<T>,
  page: { items: T[]; nextCursor: string | undefined; hasMore: boolean },
  position: Position | undefined,
): ListWindow<T> {
  const seen = new Set(window.items.map((item) => item.id));
  const added = page.items.filter((item) => !seen.has(item.id));
  return {
    items: [...window.items, ...added],
    nextCursor: page.nextCursor,
    hasMore: page.hasMore,
    position: position ?? window.position,
  };
}

/**
 * Apply a refreshed head page to an existing window, in place.
 *
 * This is the heart of #1713's fix. The refresh fetches only the FIRST page —
 * the rows most likely to have changed — and reconciles it against the loaded
 * window:
 *
 *   - a row already in the window is replaced where it sits, so a status change
 *     is reflected without moving anything;
 *   - a row not in the window is PREPENDED, because a bounded list ordered by
 *     descending recency can only gain rows at the head;
 *   - rows the window holds beyond the refreshed page are left alone, which is
 *     what preserves pagination depth.
 *
 * Deliberately NOT done: removing window rows absent from the head page. Their
 * absence is expected — they are older than the page boundary — and treating it
 * as a deletion would erase the operator's loaded pages every refresh, which is
 * the original bug wearing different clothes.
 */
export function applyRefresh<T extends ListItem>(
  window: ListWindow<T>,
  page: { items: T[]; nextCursor: string | undefined; hasMore: boolean },
  position: Position | undefined,
  options: ApplyOptions<T>,
): ListWindow<T> {
  const verdict = comparePosition(position, window.position);
  if (verdict === "stale") {
    // A response from behind what we already applied. Dropping it is safe: a
    // newer one is either applied or in flight.
    return window;
  }
  if (verdict === "epoch-changed") {
    // The store was rebuilt. Snapshot rather than patch — see comparePosition
    // for why keeping the old window would pin the client to a dead epoch.
    return {
      items: page.items,
      nextCursor: page.nextCursor,
      hasMore: page.hasMore,
      position,
    };
  }

  const index = new Map(window.items.map((item, at) => [item.id, at]));
  const items = window.items.slice();
  const fresh: T[] = [];
  for (const item of page.items) {
    const at = index.get(item.id);
    if (at === undefined) {
      fresh.push(item);
      continue;
    }
    items[at] = item;
  }

  if (fresh.length > 0) {
    // New rows sort into the head, not blindly to position 0: two events can
    // arrive out of order, and the window must stay in the server's order or
    // the next cursor would not continue it.
    fresh.sort((a, b) => compareOrder(options.orderOf(a), options.orderOf(b)));
    items.unshift(...fresh);
  }

  return {
    items,
    // The window's own cursor is preserved. The refresh fetched the head, so
    // its nextCursor points just past the FIRST page — adopting it would
    // silently rewind pagination to page two.
    nextCursor: window.nextCursor,
    // hasMore likewise describes the window's tail, not the head page's.
    hasMore: window.hasMore,
    position: position ?? window.position,
  };
}

/**
 * Whether an invalidation touches this list's scope.
 *
 * A list scoped to one gaggle must not refetch because a run in another gaggle
 * changed. Without this, every list on every open tab refetches on every event,
 * which is how a handful of active runs turns into a request storm.
 */
export function invalidationTouchesScope(
  invalidation: { runIds?: string[]; workflows?: { gaggle: string; workflow: string }[] },
  scope: { gaggle?: string; workflow?: string },
  windowIds: ReadonlySet<string>,
): boolean {
  if (!scope.gaggle && !scope.workflow) return true;
  if (invalidation.workflows?.length) {
    for (const reference of invalidation.workflows) {
      if (scope.gaggle && reference.gaggle !== scope.gaggle) continue;
      if (scope.workflow && reference.workflow !== scope.workflow) continue;
      return true;
    }
  }
  // A run id already in the window is in scope by construction — it is on
  // screen. This is what makes a status change to a loaded row update in place
  // even when the event carries no workflow reference.
  if (invalidation.runIds?.some((id) => windowIds.has(id))) return true;
  // An unscoped invalidation (no ids, no workflows) means "something changed,
  // details unknown". Refreshing is the safe reading: the alternative is
  // ignoring a real change.
  return !invalidation.runIds?.length && !invalidation.workflows?.length;
}
