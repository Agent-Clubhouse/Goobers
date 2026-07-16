import { useRef } from "react";
import type { PrimaryArea, Route } from "../routes";
import { Icon, type IconName } from "./Icon";

interface NavigationItem {
  area: PrimaryArea;
  icon: IconName;
  label: string;
  route: Route;
  count?: number;
}

export function PrimaryNavigation({
  area,
  navigate,
  runCount,
  workflowCount,
}: {
  area: PrimaryArea;
  navigate: (route: Route) => void;
  runCount: number;
  workflowCount: number;
}) {
  const itemRefs = useRef<Array<HTMLButtonElement | null>>([]);
  const items: NavigationItem[] = [
    { area: "overview", icon: "overview", label: "Overview", route: { page: "overview" } },
    { area: "workflows", icon: "workflow", label: "Workflows", route: { page: "workflows" }, count: workflowCount },
    { area: "runs", icon: "run", label: "Runs", route: { page: "runs" }, count: runCount },
  ];

  const focusItem = (index: number) => {
    itemRefs.current[(index + items.length) % items.length]?.focus();
  };

  const onKeyDown = (event: React.KeyboardEvent, index: number) => {
    if (event.key === "ArrowRight" || event.key === "ArrowDown") {
      event.preventDefault();
      focusItem(index + 1);
    } else if (event.key === "ArrowLeft" || event.key === "ArrowUp") {
      event.preventDefault();
      focusItem(index - 1);
    } else if (event.key === "Home") {
      event.preventDefault();
      focusItem(0);
    } else if (event.key === "End") {
      event.preventDefault();
      focusItem(items.length - 1);
    }
  };

  return (
    <nav className="primary-nav" aria-label="Primary">
      {items.map((item, index) => (
        <button
          aria-current={area === item.area ? "page" : undefined}
          aria-label={item.label}
          className={area === item.area ? "nav-item nav-item-active" : "nav-item"}
          key={item.area}
          onClick={() => navigate(item.route)}
          onKeyDown={(event) => onKeyDown(event, index)}
          ref={(element) => {
            itemRefs.current[index] = element;
          }}
          type="button"
        >
          <Icon name={item.icon} />
          <span className="nav-label">{item.label}</span>
          {item.count !== undefined && <span className="nav-count">{item.count}</span>}
        </button>
      ))}
    </nav>
  );
}
