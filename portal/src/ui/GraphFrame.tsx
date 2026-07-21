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
      <div className="panel-heading-row">
        <div>
          <p className="section-kicker">{eyebrow}</p>
          <h2>{title}</h2>
        </div>
        {action}
      </div>
      {children}
    </div>
  );
}
