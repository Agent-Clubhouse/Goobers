import { SectionHeading } from "./Heading";

interface GraphFrameProps {
  action?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
  eyebrow?: string;
  title?: string;
}

export function GraphFrame({
  action,
  children,
  className = "",
  eyebrow = "Structure",
  title = "Execution graph",
}: GraphFrameProps) {
  return (
    <div className={`graph-panel ${className}`.trim()}>
      <SectionHeading
        actions={action}
        className="panel-heading-row"
        eyebrow={eyebrow}
        title={title}
      />
      {children}
    </div>
  );
}
