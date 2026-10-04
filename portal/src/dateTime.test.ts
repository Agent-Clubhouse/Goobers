import { describe, expect, it, vi } from "vitest";
import {
  formatDateTime,
  formatPreciseTimestamp,
  formatRelativeTimestamp,
  formatTimestamp,
} from "./dateTime";

describe("canonical portal dates", () => {
  it("reuses a bounded set of formatters for repeated table and chart dates", async () => {
    vi.resetModules();
    const { formatDateTime: formatCachedDate } = await import("./dateTime");
    const constructor = vi.spyOn(Intl, "DateTimeFormat");
    try {
      const formats = ["date-time", "precise", "date", "hour"] as const;
      const expected = formats.map((format) => formatCachedDate(0, format));
      for (let index = 0; index < 20; index++) {
        formats.forEach((format, position) => {
          expect(formatCachedDate(0, format)).toBe(expected[position]);
        });
      }
      expect(constructor).toHaveBeenCalledTimes(formats.length);
    } finally {
      constructor.mockRestore();
    }
  });

  it("uses one local-time English format with seconds across string, epoch and Date inputs", () => {
    const value = "2026-10-04T06:00:01Z";
    const expected = new Intl.DateTimeFormat("en-US", {
      year: "numeric",
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
    }).format(new Date(value));
    for (const input of [value, Date.parse(value), new Date(value)])
      expect(formatTimestamp(input)).toBe(expected);
  });
  it("handles missing and invalid dates explicitly, including the Unix epoch", () => {
    expect(formatTimestamp(undefined)).toBe("In progress");
    expect(formatTimestamp("invalid")).toBe("Unavailable");
    expect(formatDateTime(null)).toBe("Unavailable");
    expect(formatDateTime(Number.NaN)).toBe("Unavailable");
    expect(formatTimestamp(0)).not.toBe("Unavailable");
  });
  it("uses the same locale for precise tooltips and compact chart ticks", () => {
    const date = new Date("2026-10-04T06:00:01Z");
    expect(formatPreciseTimestamp(date)).toBe(
      new Intl.DateTimeFormat("en-US", {
        dateStyle: "full",
        timeStyle: "long",
      }).format(date),
    );
    expect(formatDateTime(date, "date")).toBe(
      new Intl.DateTimeFormat("en-US", { month: "short", day: "numeric" }).format(date),
    );
  });
  it("adds relative freshness without a second absolute-time convention", () => {
    const now = Date.parse("2026-10-04T06:00:00Z");
    expect(formatRelativeTimestamp(now - 60_000, now)).toBe(
      `${formatTimestamp(now - 60_000)} (1m ago)`,
    );
    expect(formatRelativeTimestamp(now + 5_000, now)).toBe(
      `${formatTimestamp(now + 5_000)} (in 5s)`,
    );
    expect(formatRelativeTimestamp(now, now)).toBe(`${formatTimestamp(now)} (now)`);
    expect(formatRelativeTimestamp(undefined, now)).toBe("Never");
    expect(formatRelativeTimestamp(Number.NaN, now)).toBe("Unavailable");
  });
});
