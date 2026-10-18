import type { ComponentPropsWithoutRef, ReactNode } from "react";

export function FilterOptions({
  label,
  children,
  className = "",
}: {
  label: string;
  children: ReactNode;
  className?: string;
}) {
  return (
    <div aria-label={label} className={`ui-filter-options ${className}`.trim()} role="group">
      {children}
    </div>
  );
}

export function FilterOption({
  selected,
  className = "",
  ...props
}: Omit<ComponentPropsWithoutRef<"button">, "aria-pressed"> & { selected: boolean }) {
  return (
    <button
      {...props}
      aria-pressed={selected}
      className={`filter-button ui-filter-option${selected ? " filter-button-active" : ""} ${className}`.trim()}
      type="button"
    />
  );
}

export function FilterField({
  label,
  children,
  className = "",
  kind = "select",
}: {
  label?: ReactNode;
  children: ReactNode;
  className?: string;
  kind?: "select" | "search" | "toggle";
}) {
  return (
    <label className={`filter-${kind} ui-filter-field ${className}`.trim()}>
      {label && <span>{label}</span>}
      {children}
    </label>
  );
}

export function ControlGroup({
  label,
  children,
  className = "",
}: {
  label: string;
  children: ReactNode;
  className?: string;
}) {
  return (
    <div aria-label={label} className={`insight-controls ui-control-group ${className}`.trim()}>
      {children}
    </div>
  );
}
