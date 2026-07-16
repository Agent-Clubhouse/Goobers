import type { StageKind } from "../fixtures";
import { Icon } from "./Icon";

export function Inspector({
  children,
  className,
  kind,
  title,
}: {
  children: React.ReactNode;
  className: "definition-panel" | "run-inspector";
  kind: StageKind;
  title: string;
}) {
  return (
    <aside aria-label={`${title} inspector`} className={className}>
      <div className="inspector-heading">
        <span className={`primitive-icon primitive-${kind}`}>
          <Icon name={kind === "gate" ? "gate" : kind === "agentic" ? "code" : "workflow"} size={17} />
        </span>
        <div>
          <span>{kind}</span>
          <h3>{title}</h3>
        </div>
      </div>
      {children}
    </aside>
  );
}
