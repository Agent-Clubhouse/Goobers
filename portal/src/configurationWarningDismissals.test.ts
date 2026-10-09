import { act, renderHook } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";
import { useConfigurationWarningDismissals } from "./configurationWarningDismissals";

const storageKey = "goobers-configuration-warning-dismissals";
const storageBound = 500;

function stored(): unknown {
  return JSON.parse(window.localStorage.getItem(storageKey) ?? "null");
}

function mountWithStored(raw: string) {
  window.localStorage.setItem(storageKey, raw);
  return renderHook(() => useConfigurationWarningDismissals());
}

describe("useConfigurationWarningDismissals", () => {
  beforeEach(() => {
    window.localStorage.clear();
  });

  it.each([
    { raw: "{not json", expected: [] },
    { raw: '{"a":1}', expected: [] },
    { raw: '["a",2,null,"b"]', expected: ["a", "b"] },
  ])("reads only string identities from stored value $raw", ({ raw, expected }) => {
    const { result } = mountWithStored(raw);
    expect(result.current.dismissedWarningKeys).toEqual(new Set(expected));
  });

  it("keeps only the most recent acknowledgements when the bound is exceeded", () => {
    const old = Array.from({ length: storageBound }, (_, index) => `old-${index}`);
    const { result } = mountWithStored(JSON.stringify(old));

    // Re-dismissing the oldest entry refreshes it rather than letting it age out.
    act(() => result.current.dismiss(["old-0", "new"]));

    const keys = stored() as string[];
    expect(keys).toHaveLength(storageBound);
    expect(keys).not.toContain("old-1");
    expect(keys.slice(-2)).toEqual(["old-0", "new"]);
  });

  it("follows dismissals and restores made in another tab", () => {
    const { result } = renderHook(() => useConfigurationWarningDismissals());
    act(() => result.current.dismiss(["a"]));
    expect(stored()).toEqual(["a"]);

    window.localStorage.setItem(storageKey, JSON.stringify(["b"]));
    act(() => {
      window.dispatchEvent(new StorageEvent("storage", { key: storageKey }));
    });
    expect(result.current.dismissedWarningKeys).toEqual(new Set(["b"]));

    act(() => result.current.restore(["b"]));
    expect(stored()).toEqual([]);
  });
});
