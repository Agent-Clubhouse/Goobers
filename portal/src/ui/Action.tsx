import { forwardRef, type ComponentPropsWithoutRef } from "react";

type ActionVariant = "text" | "primary" | "secondary" | "icon";
type ActionSize = "normal" | "compact" | "touch";

function actionClass(variant: ActionVariant, size: ActionSize, className: string) {
  return `ui-action ui-action-${variant}${size === "normal" ? "" : ` ui-action-${size}`} ${className}`.trim();
}

export const Action = forwardRef<
  HTMLButtonElement,
  ComponentPropsWithoutRef<"button"> & { variant?: ActionVariant; size?: ActionSize }
>(function Action(
  { variant = "text", size = "normal", className = "", type = "button", ...props },
  ref,
) {
  return (
    <button {...props} className={actionClass(variant, size, className)} ref={ref} type={type} />
  );
});

export function ActionLink({
  variant = "text",
  size = "normal",
  className = "",
  ...props
}: ComponentPropsWithoutRef<"a"> & {
  variant?: Exclude<ActionVariant, "icon">;
  size?: ActionSize;
}) {
  return <a {...props} className={actionClass(variant, size, className)} />;
}
