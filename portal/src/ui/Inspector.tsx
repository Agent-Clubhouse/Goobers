import type { WorkflowStage } from "../prototypeData";
import { Icon } from "./Icon";

interface InspectorProps {
  children: React.ReactNode;
  className: string;
  label: string;
}

export function Inspector({ children, className, label }: InspectorProps) {
  return (
    <aside aria-label={label} className={className}>
      {children}
    </aside>
  );
}

export function InspectorHeading({ stage }: { stage: WorkflowStage }) {
  return (
    <div className="inspector-heading">
      <span className={`primitive-icon primitive-${stage.kind}`}>
        <Icon
          name={stage.kind === "gate" ? "gate" : stage.kind === "agentic" ? "code" : "workflow"}
          size={17}
        />
      </span>
      <div>
        <span>{stage.kind}</span>
        <h3>{stage.name}</h3>
      </div>
    </div>
  );
}
