import { act, renderHook, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";
import type { UpdateAvailability } from "./api/types";
import {
  publishUpdateAvailability,
  readStoredUpdateDismissal,
  resetUpdateAvailability,
  updateDismissalStorageKey,
  useUpdateNotice,
} from "./updateNotice";

const pending: UpdateAvailability = {
  available: true,
  latestVersion: "v0.5.0",
  channel: "stable",
  checkedAt: "2026-09-11T12:00:00Z",
};

describe("useUpdateNotice", () => {
  beforeEach(() => {
    window.localStorage.clear();
    resetUpdateAvailability();
  });

  it("surfaces an update observed from a health response", async () => {
    const { result } = renderHook(() => useUpdateNotice());
    act(() => publishUpdateAvailability(pending));
    await waitFor(() => expect(result.current.update?.latestVersion).toBe("v0.5.0"));
  });

  it("picks up an update observed before it mounted", async () => {
    publishUpdateAvailability(pending);
    const { result } = renderHook(() => useUpdateNotice());
    await waitFor(() => expect(result.current.update?.latestVersion).toBe("v0.5.0"));
  });

  it("stays silent when the build is current", async () => {
    const { result } = renderHook(() => useUpdateNotice());
    act(() => publishUpdateAvailability({ ...pending, available: false }));
    await waitFor(() => expect(result.current.update).toBeUndefined());
  });

  it("stays silent when the daemon reports no check", async () => {
    const { result } = renderHook(() => useUpdateNotice());
    act(() => publishUpdateAvailability(undefined));
    await waitFor(() => expect(result.current.update).toBeUndefined());
  });

  it("clears once the operator updates and the daemon reports current", async () => {
    const { result } = renderHook(() => useUpdateNotice());
    act(() => publishUpdateAvailability(pending));
    await waitFor(() => expect(result.current.update).toBeDefined());
    act(() => publishUpdateAvailability({ ...pending, available: false }));
    await waitFor(() => expect(result.current.update).toBeUndefined());
  });

  it("persists a dismissal by version, not as a boolean", async () => {
    const { result } = renderHook(() => useUpdateNotice());
    act(() => publishUpdateAvailability(pending));
    await waitFor(() => expect(result.current.update).toBeDefined());

    act(() => result.current.dismiss());

    await waitFor(() => expect(result.current.update).toBeUndefined());
    expect(window.localStorage.getItem(updateDismissalStorageKey)).toBe("v0.5.0");
    expect(readStoredUpdateDismissal()).toBe("v0.5.0");
  });

  // The whole point of storing a version rather than a flag: a later release is
  // different news and must get through a previous dismissal.
  it("shows a newer version after an earlier one was dismissed", async () => {
    window.localStorage.setItem(updateDismissalStorageKey, "v0.5.0");
    const { result } = renderHook(() => useUpdateNotice());
    act(() => publishUpdateAvailability({ ...pending, latestVersion: "v0.6.0" }));
    await waitFor(() => expect(result.current.update?.latestVersion).toBe("v0.6.0"));
  });

  it("keeps a dismissal across a remount", async () => {
    const first = renderHook(() => useUpdateNotice());
    act(() => publishUpdateAvailability(pending));
    await waitFor(() => expect(first.result.current.update).toBeDefined());
    act(() => first.result.current.dismiss());
    first.unmount();

    const second = renderHook(() => useUpdateNotice());
    await waitFor(() => expect(second.result.current.update).toBeUndefined());
  });

  it("stops listening after unmount", () => {
    const { result, unmount } = renderHook(() => useUpdateNotice());
    unmount();
    // Publishing to a torn-down listener must not throw or warn.
    publishUpdateAvailability(pending);
    expect(result.current.update).toBeUndefined();
  });
});
