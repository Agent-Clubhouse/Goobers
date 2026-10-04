import type { InsightWindow } from "../insightData";
import { insightScopeFromKey, insightScopeKey, type InsightScope } from "../insightScope";
import { ScopeControl } from "./ScopeControl";

const WINDOWS: readonly { label: string; value: InsightWindow }[] = [
  { label: "Last 24 hours", value: "24h" },
  { label: "Last 7 days", value: "7d" },
  { label: "Last 30 days", value: "30d" },
  { label: "All time", value: "all" },
];

export function InsightFilters({
  label,
  onScopeChange,
  onWindowChange,
  scope,
  scopes,
  window,
}: {
  label: string;
  onScopeChange: (scope: InsightScope) => void;
  onWindowChange: (window: InsightWindow) => void;
  scope: InsightScope;
  scopes: { key: string; label: string }[];
  window: InsightWindow;
}) {
  return (
    <div className="insight-controls" aria-label={label}>
      <ScopeControl
        onChange={(key) => onScopeChange(insightScopeFromKey(key))}
        scopes={scopes}
        value={insightScopeKey(scope)}
      />
      <label>
        <span>Time window</span>
        <select
          aria-label="Time window"
          onChange={(event) => {
            const selected = WINDOWS.find((option) => option.value === event.target.value);
            if (selected) onWindowChange(selected.value);
          }}
          value={window}
        >
          {WINDOWS.map((option) => (
            <option key={option.value} value={option.value}>{option.label}</option>
          ))}
        </select>
      </label>
    </div>
  );
}
