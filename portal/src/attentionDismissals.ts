import { useCallback, useEffect, useState } from "react";

export const attentionDismissalsStorageKey = "goobers-attention-dismissed-runs";

export function readStoredAttentionDismissals(): ReadonlySet<string> {
  try {
    const raw = window.localStorage.getItem(attentionDismissalsStorageKey);
    if (!raw) {
      return new Set();
    }
    const parsed = JSON.parse(raw);
    return Array.isArray(parsed) ? new Set(parsed.filter((id) => typeof id === "string")) : new Set();
  } catch {
    return new Set();
  }
}

function persistAttentionDismissals(dismissedRunIds: ReadonlySet<string>): void {
  try {
    window.localStorage.setItem(
      attentionDismissalsStorageKey,
      JSON.stringify([...dismissedRunIds]),
    );
  } catch {
    // The dismissal still applies for this session when browser storage is unavailable.
  }
}

/**
 * Durable, cross-session dismiss state for the Overview "Needs attention" list (#2535).
 * Persisted to localStorage rather than an in-memory Set, per #1199's own noted
 * precedent in configurationWarnings.ts, so a resolved escalation stays dismissed
 * across reloads. Supports dismissing/restoring many run IDs in one call for bulk actions.
 */
export function useAttentionDismissals() {
  const [dismissedRunIds, setDismissedRunIds] = useState<ReadonlySet<string>>(() =>
    readStoredAttentionDismissals(),
  );

  useEffect(() => {
    persistAttentionDismissals(dismissedRunIds);
  }, [dismissedRunIds]);

  const dismiss = useCallback((runIds: readonly string[]) => {
    if (runIds.length === 0) {
      return;
    }
    setDismissedRunIds((current) => {
      const next = new Set(current);
      for (const runId of runIds) {
        next.add(runId);
      }
      return next;
    });
  }, []);

  const restore = useCallback((runIds: readonly string[]) => {
    if (runIds.length === 0) {
      return;
    }
    setDismissedRunIds((current) => {
      const next = new Set(current);
      for (const runId of runIds) {
        next.delete(runId);
      }
      return next;
    });
  }, []);

  return { dismissedRunIds, dismiss, restore };
}
