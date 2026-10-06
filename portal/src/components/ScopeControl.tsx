import type { ComponentProps } from "react";
import { InsightScopePicker } from "./InsightScopePicker";

export function ScopeControl(props: ComponentProps<typeof InsightScopePicker>) {
  return (
    <div className="insight-control">
      <span>Scope</span>
      <InsightScopePicker {...props} />
    </div>
  );
}
