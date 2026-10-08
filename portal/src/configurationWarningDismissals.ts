import { useCallback, useEffect, useState } from "react";

const configurationWarningDismissalsStorageKey =
  "goobers-configuration-warning-dismissals";

// Finding identities embed the configuration digest, so every edit can mint new
// keys. Keep only the most recent acknowledgements so storage stays bounded.
const maxStoredConfigurationWarningDismissals = 500;

function readStoredConfigurationWarningDismissals(): ReadonlySet<string> {
  try {
    const raw = window.localStorage.getItem(configurationWarningDismissalsStorageKey);
    if (!raw) {
      return new Set();
    }
    const parsed: unknown = JSON.parse(raw);
    return Array.isArray(parsed)
      ? new Set(
          parsed
            .filter((key): key is string => typeof key === "string")
            .slice(-maxStoredConfigurationWarningDismissals),
        )
      : new Set();
  } catch {
    return new Set();
  }
}

function persistConfigurationWarningDismissals(keys: ReadonlySet<string>): void {
  try {
    window.localStorage.setItem(
      configurationWarningDismissalsStorageKey,
      JSON.stringify([...keys]),
    );
  } catch {
    // The dismissal still applies for this session when browser storage is unavailable.
  }
}

/**
 * Durable, cross-session dismiss state for configuration warnings (#5839).
 * Entries are finding identities (see configurationWarningKey), so a materially
 * changed finding has a new key and reappears without clearing older entries.
 */
export function useConfigurationWarningDismissals() {
  const [dismissedWarningKeys, setDismissedWarningKeys] = useState<ReadonlySet<string>>(
    readStoredConfigurationWarningDismissals,
  );

  useEffect(() => {
    persistConfigurationWarningDismissals(dismissedWarningKeys);
  }, [dismissedWarningKeys]);

  useEffect(() => {
    const onStorage = (event: StorageEvent) => {
      if (event.key === configurationWarningDismissalsStorageKey || event.key === null) {
        setDismissedWarningKeys(readStoredConfigurationWarningDismissals());
      }
    };
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, []);

  const dismiss = useCallback((keys: readonly string[]) => {
    if (keys.length === 0) {
      return;
    }
    setDismissedWarningKeys((current) => {
      const next = new Set(current);
      for (const key of keys) {
        // Re-insert so the newest acknowledgement survives the storage bound.
        next.delete(key);
        next.add(key);
      }
      return next.size > maxStoredConfigurationWarningDismissals
        ? new Set([...next].slice(-maxStoredConfigurationWarningDismissals))
        : next;
    });
  }, []);

  const restore = useCallback((keys: readonly string[]) => {
    if (keys.length === 0) {
      return;
    }
    setDismissedWarningKeys((current) => {
      if (!keys.some((key) => current.has(key))) {
        return current;
      }
      const next = new Set(current);
      for (const key of keys) {
        next.delete(key);
      }
      return next;
    });
  }, []);

  return { dismissedWarningKeys, dismiss, restore };
}
