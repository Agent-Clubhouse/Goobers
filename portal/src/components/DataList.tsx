import type { ReactNode } from "react";
import { Icon } from "../foundation/Icon";

export function DataList({
  ariaLabel,
  children,
  headings,
  headerClassName,
}: {
  ariaLabel: string;
  children: ReactNode;
  headings?: string[];
  headerClassName?: string;
}) {
  return (
    <div aria-label={ariaLabel} className="data-table" role="table">
      {headings && (
        <div className={`data-header ${headerClassName ?? ""}`.trim()} role="row">
          {headings.map((heading, index) => (
            <span key={`${heading}-${index}`} role="columnheader">
              {heading || <span className="sr-only">Actions</span>}
            </span>
          ))}
        </div>
      )}
      {children}
    </div>
  );
}

export function DataCell({
  children,
  className,
}: {
  children: ReactNode;
  className?: string;
}) {
  return (
    <span className={className} role="cell">
      {children}
    </span>
  );
}

export function DataRow({
  children,
  onClick,
  label,
}: {
  children: ReactNode;
  onClick: () => void;
  label: string;
}) {
  return (
    <div className="data-row" role="row">
      {children}
      <span className="row-arrow" role="cell">
        <button aria-label={label} className="data-row-action" onClick={onClick} type="button">
          <Icon name="chevron" size={16} />
        </button>
      </span>
    </div>
  );
}
