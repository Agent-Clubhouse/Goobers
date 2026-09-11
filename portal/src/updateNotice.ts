import { useCallback, useEffect, useState } from "react";
import type { UpdateAvailability } from "./api/types";

/**
 * Durable, per-version dismissal for the "update available" strip (#4920).
 *
 * The stored value is the dismissed VERSION, not a boolean. That is what makes
 * the portal mirror the daemon's own announce-once-per-distinct-version rule:
 * dismissing v0.5.0 must never suppress v0.6.0, which is different news. A
 * boolean would silently swallow every future release.
 *
 * Follows the localStorage precedent in attentionDismissals.ts — a dismissal
 * that evaporates on reload is not a dismissal.
 */
export const updateDismissalStorageKey = "goobers-dismissed-update-version";

export function readStoredUpdateDismissal(): string | undefined {
  try {
    const raw = window.localStorage.getItem(updateDismissalStorageKey);
    return typeof raw === "string" && raw !== "" ? raw : undefined;
  } catch {
    return undefined;
  }
}

function persistUpdateDismissal(version: string | undefined): void {
  try {
    if (version === undefined) {
      window.localStorage.removeItem(updateDismissalStorageKey);
      return;
    }
    window.localStorage.setItem(updateDismissalStorageKey, version);
  } catch {
    // The dismissal still applies for this session when storage is unavailable.
  }
}

/**
 * Update availability is OBSERVED from health responses the app already makes,
 * never fetched on its own. This mirrors publishReadState in liveData.tsx, and
 * it is not merely an optimization: a second, independent health poller would
 * change the ORDER in which health requests arrive, which several consumers
 * key on (a client that fails only the first request, for instance, would have
 * that failure silently absorbed by the extra poll). The daemon serves a
 * cached answer that changes at most once a day, so there is nothing here
 * worth its own request.
 */
let latestUpdate: UpdateAvailability | undefined;
const listeners = new Set<(update: UpdateAvailability | undefined) => void>();

/** Called wherever a health response arrives. */
export function publishUpdateAvailability(update: UpdateAvailability | undefined): void {
  latestUpdate = update;
  for (const listener of listeners) {
    listener(update);
  }
}

/** Subscribes to observed values. Returns an unsubscribe function. */
export function onUpdateAvailability(
  listener: (update: UpdateAvailability | undefined) => void,
): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** Test seam: drops any observed value so cases cannot leak into each other. */
export function resetUpdateAvailability(): void {
  latestUpdate = undefined;
}

export interface UpdateNoticeState {
  /** The pending update to render, or undefined when there is nothing to show. */
  update?: UpdateAvailability;
  dismiss(): void;
}

export function useUpdateNotice(): UpdateNoticeState {
  const [update, setUpdate] = useState<UpdateAvailability | undefined>(() => latestUpdate);
  const [dismissedVersion, setDismissedVersion] = useState<string | undefined>(() =>
    readStoredUpdateDismissal(),
  );

  useEffect(() => {
    persistUpdateDismissal(dismissedVersion);
  }, [dismissedVersion]);

  useEffect(() => {
    // Re-read on subscribe: a health response may have landed between this
    // component's first render and its effect.
    setUpdate(latestUpdate);
    return onUpdateAvailability((next) => setUpdate(next));
  }, []);

  const dismiss = useCallback(() => {
    setDismissedVersion(update?.latestVersion);
  }, [update?.latestVersion]);

  const pending =
    update?.available === true && update.latestVersion !== dismissedVersion ? update : undefined;

  return { update: pending, dismiss };
}
