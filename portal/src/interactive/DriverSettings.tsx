import type { ChatCapabilities, PermissionLevel, SwitchTiming } from "./client";

export interface DriverSettings {
  model?: string; level?: PermissionLevel; effort?: string;
  models: {id: string; label: string}[]; efforts: string[];
}
export type SettingsChange = {type: "session.set_model"; model: string} | {type: "session.set_level"; level: PermissionLevel} | {type: "session.set_effort"; effort: string};
const LEVELS: Record<PermissionLevel, string> = {plan: "Plan", ask: "Ask", auto_edit: "Auto edit", auto: "Auto", full_access: "Full access"};
const EFFECT: Partial<Record<SwitchTiming, string>> = {live: "Applies now", per_turn: "Applies from the next message", restart: "Applies when the agent restarts"};
// Capability-gated pills follow Clubhouse Composer.tsx at 3309665c (MIT).
// Keep the confirmed value until the host reports what the driver applied.
export function DriverSettingsPills({settings, capabilities, disabled, onChange}: {
  settings: DriverSettings; capabilities?: ChatCapabilities; disabled: boolean; onChange: (change: SettingsChange) => void;
}) {
  const levels = (Object.keys(LEVELS) as PermissionLevel[]).filter(level => level === settings.level || capabilities?.liveLevels?.includes(level));
  const modelEnabled = !disabled && !!capabilities?.modelSwitch && capabilities.modelSwitch !== "none" && settings.models.length > 0;
  const effortEnabled = !disabled && !!capabilities?.effortSwitch && capabilities.effortSwitch !== "none" && settings.efforts.length > 0;
  if (!levels.length && !settings.model && !settings.models.length && settings.effort === undefined && !settings.efforts.length) return null;
  return <div className="ix-driver-settings" aria-label="Driver settings">
    {levels.length > 0 && <label>Permissions<select aria-label="Permission level" value={settings.level ?? ""}
      disabled={disabled || !capabilities?.liveLevels?.some(level => level !== settings.level)}
      onChange={event => onChange({type: "session.set_level", level: event.target.value as PermissionLevel})}>
      {!settings.level && <option value="" disabled>Not reported</option>}
      {levels.map(level => <option key={level} value={level} disabled={!capabilities?.liveLevels?.includes(level)}>{LEVELS[level]}</option>)}
    </select></label>}
    {(settings.model || settings.models.length > 0) && <label title={EFFECT[capabilities?.modelSwitch ?? "none"]}>Model<select aria-label="Model" disabled={!modelEnabled}
      value={settings.model ?? ""} onChange={event => onChange({type: "session.set_model", model: event.target.value})}>
      {!settings.model && <option value="" disabled>Not reported</option>}
      {settings.model && !settings.models.some(model => model.id === settings.model) && <option value={settings.model} disabled>{settings.model}</option>}
      {settings.models.map(model => <option key={model.id} value={model.id}>{model.label}</option>)}
    </select></label>}
    {(settings.effort !== undefined || settings.efforts.length > 0) && <label title={EFFECT[capabilities?.effortSwitch ?? "none"]}>Effort<select aria-label="Effort" disabled={!effortEnabled}
      value={settings.effort ?? "unreported"} onChange={event => onChange({type: "session.set_effort", effort: event.target.value})}>
      {settings.effort === undefined && <option value="unreported" disabled>Not reported</option>}
      <option value="">Default</option>
      {settings.effort && !settings.efforts.includes(settings.effort) && <option value={settings.effort} disabled>{settings.effort}</option>}
      {settings.efforts.map(effort => <option key={effort} value={effort}>{effort}</option>)}
    </select></label>}
    {(modelEnabled || effortEnabled) && <small>{modelEnabled && EFFECT[capabilities?.modelSwitch ?? "none"]}{modelEnabled && effortEnabled && " · "}{effortEnabled && `Effort: ${EFFECT[capabilities?.effortSwitch ?? "none"]?.toLowerCase()}`}</small>}
  </div>;
}
