import type { ComponentPropsWithoutRef } from "react";

export function MetadataGrid({
  className = "",
  layout = "grid",
  ...props
}: ComponentPropsWithoutRef<"dl"> & { layout?: "grid" | "inline" }) {
  return <dl {...props} className={`ui-metadata-grid ui-metadata-${layout} ${className}`.trim()} />;
}
