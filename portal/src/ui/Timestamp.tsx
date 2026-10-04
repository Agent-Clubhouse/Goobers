import type { ComponentPropsWithoutRef } from "react";
import {
  formatDateTime,
  formatPreciseTimestamp,
  timestampDate,
  type DateTimeFormat,
  type TimestampValue,
} from "../dateTime";

export function Timestamp({
  value,
  format = "date-time",
  prefix = "",
  missing = "Unavailable",
  title,
  ...props
}: Omit<ComponentPropsWithoutRef<"time">, "dateTime" | "children"> & {
  value: TimestampValue;
  format?: DateTimeFormat;
  prefix?: string;
  missing?: string;
}) {
  const date = timestampDate(value);
  if (!date)
    return (
      <span {...props} title={title}>
        {missing}
      </span>
    );
  return (
    <time
      {...props}
      dateTime={typeof value === "string" ? value : date.toISOString()}
      title={title ?? formatPreciseTimestamp(value)}
    >
      {prefix}
      {formatDateTime(value, format)}
    </time>
  );
}
