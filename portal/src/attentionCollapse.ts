import { useEffect, useState } from "react";

export const attentionCollapsedStorageKey = "goobers-overview-attention-collapsed";

export function readStoredAttentionCollapsed(): boolean {
  try {
    return window.localStorage.getItem(attentionCollapsedStorageKey) === "true";
  } catch {
    return false;
  }
}

function persistAttentionCollapsed(collapsed: boolean): void {
  try {
    window.localStorage.setItem(attentionCollapsedStorageKey, String(collapsed));
  } catch {
    // The toggle still applies for this session when browser storage is unavailable.
  }
}

/**
 * Durable, cross-session collapsed state for the Overview "Needs attention"
 * section (#2660), following the same localStorage read/persist shape as
 * attentionDismissals.ts and theme.ts. Distinct from that file: this is
 * view state (is the section expanded), not which runs are dismissed.
 * Absent storage (first visit, or storage unavailable) defaults to expanded.
 */
export function useAttentionCollapsed(): [boolean, (collapsed: boolean) => void] {
  const [collapsed, setCollapsedState] = useState<boolean>(() => readStoredAttentionCollapsed());

  useEffect(() => {
    persistAttentionCollapsed(collapsed);
  }, [collapsed]);

  return [collapsed, setCollapsedState];
}
