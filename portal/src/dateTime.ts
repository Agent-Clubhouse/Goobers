export type TimestampValue = string | number | Date | null | undefined;
export type DateTimeFormat = "date-time" | "precise" | "date" | "hour";

const OPTIONS: Record<DateTimeFormat, Intl.DateTimeFormatOptions> = {
  "date-time": {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  },
  precise: { dateStyle: "full", timeStyle: "long" },
  date: { month: "short", day: "numeric" },
  hour: { hour: "numeric" },
};

export function timestampDate(value: TimestampValue): Date | undefined {
  if (value === undefined || value === null || value === "") return undefined;
  const date = value instanceof Date ? value : new Date(value);
  return Number.isFinite(date.getTime()) ? date : undefined;
}

export function formatDateTime(
  value: TimestampValue,
  format: DateTimeFormat = "date-time",
): string {
  const date = timestampDate(value);
  return date ? new Intl.DateTimeFormat("en-US", OPTIONS[format]).format(date) : "Unavailable";
}

export function formatTimestamp(value: TimestampValue): string {
  return value === undefined || value === null || value === ""
    ? "In progress"
    : formatDateTime(value);
}

export function formatPreciseTimestamp(value: TimestampValue): string {
  return formatDateTime(value, "precise");
}

export function formatRelativeTimestamp(timestamp: number | undefined, now = Date.now()): string {
  if (timestamp === undefined) return "Never";
  if (!timestampDate(timestamp)) return "Unavailable";
  const difference = timestamp - now;
  const elapsed = Math.abs(difference);
  const amount =
    elapsed < 1_000
      ? "now"
      : elapsed < 60_000
        ? `${Math.round(elapsed / 1_000)}s`
        : elapsed < 3_600_000
          ? `${Math.round(elapsed / 60_000)}m`
          : `${Math.round(elapsed / 3_600_000)}h`;
  return `${formatTimestamp(timestamp)} (${amount === "now" ? "now" : difference > 0 ? `in ${amount}` : `${amount} ago`})`;
}
