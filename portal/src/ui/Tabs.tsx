import { forwardRef, type ComponentPropsWithoutRef, type KeyboardEvent } from "react";

export function TabList({ className = "", onKeyDown, ...props }: ComponentPropsWithoutRef<"div">) {
  const moveFocus = (event: KeyboardEvent<HTMLDivElement>) => {
    onKeyDown?.(event);
    if (event.defaultPrevented) return;
    const vertical = props["aria-orientation"] === "vertical";
    const forward = vertical ? "ArrowDown" : "ArrowRight";
    const backward = vertical ? "ArrowUp" : "ArrowLeft";
    if (![forward, backward, "Home", "End"].includes(event.key)) return;
    const tabs = [
      ...event.currentTarget.querySelectorAll<HTMLButtonElement>("[role='tab']:not(:disabled)"),
    ];
    const current = tabs.findIndex((tab) => tab === document.activeElement);
    if (tabs.length === 0 || current < 0) return;
    const next =
      event.key === "Home"
        ? 0
        : event.key === "End"
          ? tabs.length - 1
          : (current + (event.key === forward ? 1 : -1) + tabs.length) % tabs.length;
    event.preventDefault();
    tabs[next].focus();
    tabs[next].click();
  };
  return (
    <div
      {...props}
      className={`ui-tab-list ${className}`.trim()}
      onKeyDown={moveFocus}
      role="tablist"
    />
  );
}

export const Tab = forwardRef<HTMLButtonElement, ComponentPropsWithoutRef<"button">>(function Tab(
  { className = "", tabIndex, ...props },
  ref,
) {
  const selected = props["aria-selected"] === true || props["aria-selected"] === "true";
  return (
    <button
      {...props}
      className={`ui-tab ${className}`.trim()}
      ref={ref}
      role="tab"
      tabIndex={tabIndex ?? (selected ? 0 : -1)}
      type="button"
    />
  );
});
