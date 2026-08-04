import "@testing-library/jest-dom/vitest";
import { beforeEach } from "vitest";

// jsdom's window (and its storage) is shared across every test in a file. The
// live-data manager persists its SSE cursor to sessionStorage (liveData.tsx)
// so a page reload resumes instead of re-snapshotting; without this, a cursor
// left behind by one test's simulated live event leaks into the next test
// that mounts a live connection, silently swallowing its events as
// already-applied.
beforeEach(() => {
  if (typeof window !== "undefined") {
    window.sessionStorage.clear();
  }
});
